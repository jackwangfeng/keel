package app

import (
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// EnvStockFlagInterval 是商品列表有货排序标记全量刷新的间隔（Go duration，如 30m / 2h）。
// 空或非法时用 service.DefaultStockFlagInterval（1 小时）。可售数跨 0 由库存服务的二阶段消息即时同步
// （service/stock_flags.go 文件头），这一轮只兜「漏发」与 SKU 上下架；列表上显示的「无货」不受它影响（现问库存服务）。
const EnvStockFlagInterval = "KEEL_STOCK_FLAG_INTERVAL"

// 以下三个是「库存跨 0 → 有货标记」这条二阶段消息在 Run 里的装配（service/stock_flags.go、inventory 包
// stock_msg.go）。集中在这里，Run 里只剩几行调用（都标了「跨 0 通知」）。

// newStockNotifier 建库存在本进程里时的跨 0 通知器（core 角色没有：库存在远端）。
// 嵌入式协调器：投到 local://stock_changed、回查 local://…；独立部署的协调器：发到主题（core 自己也订阅它）、
// 回查走本服务内网地址。
func newStockNotifier(s SplitConfig, invPool *pgxpool.Pool, self dtm.BranchResolver) *inventory.StockNotifier {
	if s.Role == RoleCore {
		return nil
	}
	action := "local://" + inventory.BranchStockChanged
	if s.remoteDTM() {
		action = dtm.TopicPrefix + inventory.TopicStockZeroCrossing
	}
	return inventory.NewStockNotifier(repository.NewInventoryStore(invPool), action, self.BranchURL(inventory.BranchStockMsgQuery))
}

// StockMsgBranches 是跨 0 通知要注册的分支：core 的接收分支一定有（拆分时库存远端发、core 收）；
// 回查分支只在库存也在本进程（n != nil）时有。
func StockMsgBranches(n *inventory.StockNotifier, flags *service.StockFlagService) map[string]dtm.BranchFuncEx {
	out := map[string]dtm.BranchFuncEx{inventory.BranchStockChanged: flags.StockMsgBranch()}
	if n != nil {
		out[inventory.BranchStockMsgQuery] = dtm.Ex(n.QueryBranch())
	}
	return out
}

func attachStockNotifier(n *inventory.StockNotifier, tc dtm.Coordinator) {
	if n != nil {
		n.Attach(tc)
	}
}

func stockFlagIntervalFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvStockFlagInterval))
	if raw == "" {
		return service.DefaultStockFlagInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("KEEL_STOCK_FLAG_INTERVAL 解析不了，用默认值", "value", raw, "default", service.DefaultStockFlagInterval)
		return service.DefaultStockFlagInterval
	}
	return d
}

// EnvGeoProvider / EnvGeoKey 配地图服务商（docs/POI-设计.md）：目前只有 amap（高德 Web 服务 key，绑服务器 IP 白名单）。
// 两个都没配时 /geo/* 回 501，客户端退回手填 + 地图选点。
const (
	EnvGeoProvider = "KEEL_GEO_PROVIDER"
	EnvGeoKey      = "KEEL_GEO_KEY"
)

// EnvMapTiles / EnvTiandituKey / EnvTiandituReferer 配地图底图（docs/POI-设计.md「地图底图」）：tianditu（天地图，
// 「浏览器端」类型的 key + 站点地址，代理取瓦片时带作 Referer）或 osm（只给开发自测）。没配时 /geo/map 报 enabled=false。
const (
	EnvMapTiles        = "KEEL_MAP_TILES"
	EnvTiandituKey     = "KEEL_TIANDITU_KEY"
	EnvTiandituReferer = "KEEL_TIANDITU_REFERER"
	// EnvTileProxy 是瓦片请求专用的出口代理（只影响 /geo/tiles 向服务商取图，见 geo.TilesFromEnv）。
	EnvTileProxy = "KEEL_TILE_PROXY"
)

// EnvSearchVectorFloor 覆盖检索的向量相关度下限（service.DefaultVectorFloor，余弦相似度）。
// 没设用默认值；负数关闭下限（回到「向量永远凑满 size 条」）。换 embedding 模型后要重新量。
const EnvSearchVectorFloor = "KEEL_SEARCH_VECTOR_FLOOR"
