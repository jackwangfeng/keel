package inventory

// 「这家商家开没开渠道」的接收方（二阶段消息；发送方是 core 的 service/channel.go，在启用 / 停用销售渠道
// binding 的同一个本地事务里登记）。收到后写库存库的 channel_merchants（00300），并让本进程的闸门立刻生效。
//
// 与活动配额同步不同，这里**信消息内容**：库存服务不认识 core（拆分形态下它连 core 的地址都没有），
// 回不了源。乱序由版本挡住：载荷带 core 登记消息时的版本 rev（单调），库存库只接受更新的版本
// （repository: InvSetChannelMerchant），关闭也留一行 —— 晚到的旧「开」翻不回来。

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// BranchChannelMerchantSync 是库存服务的接收分支：单体 local://inventory_channel_merchant_sync，
// 拆分 <KEEL_INVENTORY_URL>/internal/v1/saga/inventory_channel_merchant_sync?bt=…。
const BranchChannelMerchantSync = "inventory_channel_merchant_sync"

// ChannelMerchantMsgGIDPrefix：chm-{商家}-{随机串}。
const ChannelMerchantMsgGIDPrefix = "chm-"

// ChannelMerchantPayload 是消息载荷。
type ChannelMerchantPayload struct {
	Enabled bool  `json:"enabled"`
	Rev     int64 `json:"rev"`
}

// ChannelMerchantMsgGID 编一条消息的 gid；每次启停都是一条新消息。
func ChannelMerchantMsgGID(merchantID int64) (string, error) {
	return dtm.TenantGID(ChannelMerchantMsgGIDPrefix, merchantID, nonce())
}

// ChannelMerchantSyncBranch 是接收分支。g 是本进程的闸门（可为 nil：进程开关关闭时，分支照样把状态记进库，
// 以后打开开关时不用补发）。
func (l *Local) ChannelMerchantSyncBranch(g *ChannelGate) dtm.BranchFuncEx {
	return func(gid, branchID, op, payload string) int {
		log := slog.Default().With("gid", gid, "branch_id", branchID, "op", op, "branch", BranchChannelMerchantSync)
		if op != "action" {
			log.Error("开关渠道消息的 op 不是 action")
			return dtm.Unknown
		}
		ctx, _, err := dtm.TenantContextFromTenantGID(context.Background(), ChannelMerchantMsgGIDPrefix, gid)
		if err != nil {
			log.Error("开关渠道消息的 gid 解不开，丢弃", "err", err)
			return dtm.Success
		}
		var p ChannelMerchantPayload
		if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &p); err != nil || p.Rev <= 0 {
			// 重试一万次也一样：记下来、吞掉。core 下一次启停会再发一条完整状态。
			log.Error("开关渠道消息的载荷不成立，丢弃", "payload", payload, "err", err)
			return dtm.Success
		}
		applied := false
		if _, err := l.store.WithSagaBranch(ctx, gid, branchID, op, func(tx repository.InventoryStoreTx) error {
			var e error
			applied, e = tx.SetChannelMerchant(ctx, p.Enabled, p.Rev)
			return e
		}); err != nil {
			log.Warn("开关渠道没写进库，按 Unknown 让协调器重试", "err", err)
			return dtm.Unknown
		}
		mid, _ := tenant.FromContext(ctx)
		if applied {
			g.Remember(mid, p.Enabled)
		} else {
			g.Forget(mid) // 旧消息：不知道当前是什么，让下一次判定重查
		}
		return dtm.Success
	}
}
