package app

import (
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 渠道适配层在 Run 里的装配（docs/superpowers/specs/2026-10-02-channel-adapter-design.md §8）。
//
// EnvChannels 是进程开关，默认关。关着时这里什么都不建：没有渠道路由、没有渠道分支、没有渠道后台任务，
// 库存服务的闸门不开（stock.changed 一条都不发、channel_merchants 一次都不查）—— 不变量
// 「不配渠道零开销」。拆分部署时 core 与库存进程都要配同一个值（库存进程只认它来决定发不发 stock.changed）。
const EnvChannels = "KEEL_CHANNELS"

// channelsFromEnv 读开关：空 / 0 / false / off 为关，1 / true / on 为开，别的值拒绝启动（拼错不该静默成「关」）。
func channelsFromEnv() (bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvChannels))) {
	case "", "0", "false", "off":
		return false, nil
	case "1", "true", "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q 认不出来：写 on 或 off", EnvChannels, os.Getenv(EnvChannels))
	}
}

// channelRegistry 是这个进程编进来的渠道适配器。第一期没有真实适配器（第二期起登记 Shopify）。
func channelRegistry() *channel.Registry {
	return channel.NewRegistry()
}

// channelStockAction 是 stock.changed 的投递目标：嵌入式协调器投到 core 的进程内分支，独立协调器发到主题。
func channelStockAction(s SplitConfig) string {
	if s.remoteDTM() {
		return dtm.TopicPrefix + inventory.TopicStockChanged
	}
	return "local://" + inventory.BranchChannelStockChanged
}

// withChannels 在库存的通知器上打开 stock.changed（通知器为 nil —— core 角色、或没配协调器的库存进程 —— 时什么都不做）。
func withChannels(n *inventory.StockNotifier, gate *inventory.ChannelGate, s SplitConfig) {
	if n != nil && gate.Enabled() {
		n.WithChannels(gate, channelStockAction(s))
	}
}

// newChannelService 建 core 的渠道编排。开关渠道消息的目标：core 角色指向库存服务（http://），
// 库存在本进程时指向本进程（嵌入式 local://，独立协调器经本服务内网回调）。
func newChannelService(s SplitConfig, pool *pgxpool.Pool, inv inventory.Service, self dtm.BranchResolver) (*service.ChannelService, error) {
	res := self
	if s.Role == RoleCore {
		var err error
		if res, err = dtm.NewBranchResolver(s.InventoryURL, s.InternalSecret); err != nil {
			return nil, err
		}
	}
	return service.NewChannelService(repository.New(pool), inv, channelRegistry(), res, self), nil
}

// ChannelBranches 是渠道层要注册的分支：core 的开关渠道回查、stock.changed 接收；库存在本进程（local 非 nil）时
// 再加库存这一侧的开关渠道接收。
func ChannelBranches(ch *service.ChannelService, local *inventory.Local, gate *inventory.ChannelGate) map[string]dtm.BranchFuncEx {
	out := map[string]dtm.BranchFuncEx{
		service.BranchChannelMerchantQuery:  dtm.Ex(ch.MerchantQueryBranch()),
		inventory.BranchChannelStockChanged: ch.StockChangedBranch(),
	}
	if local != nil {
		out[inventory.BranchChannelMerchantSync] = local.ChannelMerchantSyncBranch(gate)
	}
	return out
}
