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

// newStockNotifier 建本进程的跨 0 通知器。all（单体）：通知器在进程内，目标是 local://stock_changed；
// core：返回 nil —— core 不改库存，通知由库存进程发、经内网端口送进来，所以 core 没配内网端口时喊一声。
// （inventory 角色不走这里，见 runInventory。）
func newStockNotifier(s SplitConfig, invPool *pgxpool.Pool) *inventory.StockNotifier {
	if s.Role == RoleCore {
		if s.InternalAddr == "" {
			slog.Warn(EnvRole + "=core 没有配 " + EnvInternalAddr + "：库存服务的跨 0 通知送不进来（投递会一直重试），" +
				"商品列表的有货排序只靠全量刷新（" + EnvStockFlagInterval + "）")
		}
		return nil
	}
	return inventory.NewStockNotifier(repository.NewInventoryStore(invPool), "local://"+inventory.BranchStockChanged)
}

// StockMsgBranches 是跨 0 通知要注册到本进程协调器上的两个分支：接收（core 的 stock_changed）与回查
// （库存的 inventory_stock_msg_query）。只有单体两个都在本进程；n 为 nil（core）时返回空。
// 导出给测试：handler 包的协调器照 Run 的样子注册。
func StockMsgBranches(n *inventory.StockNotifier, flags *service.StockFlagService) map[string]dtm.BranchFunc {
	if n == nil {
		return nil
	}
	return map[string]dtm.BranchFunc{
		inventory.BranchStockChanged:  flags.StockMsgBranch(),
		inventory.BranchStockMsgQuery: n.QueryBranch(),
	}
}

// withBranches 把 extra 并进 base（同名时 dtm.StartEx 那边不会发现 —— 两组都是 BranchFunc —— 所以这里拒绝）。
func withBranches(base, extra map[string]dtm.BranchFunc) map[string]dtm.BranchFunc {
	for name, fn := range extra {
		if _, dup := base[name]; dup {
			panic("分支 " + name + " 重名")
		}
		base[name] = fn
	}
	return base
}

func attachStockNotifier(n *inventory.StockNotifier, tc *dtm.TC) {
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
