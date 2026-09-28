package app

import (
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/keel/keel/internal/service"
)

// EnvStockFlagInterval 是商品列表有货排序标记全量刷新的间隔（Go duration，如 30s / 2m）。
// 空或非法时用 service.DefaultStockFlagInterval（1 分钟）。它决定下单扣减 / 关单回补之后
// 列表排序最多晚多久跟上；列表上显示的「无货」不受它影响（那是现问库存服务的）。
const EnvStockFlagInterval = "KEEL_STOCK_FLAG_INTERVAL"

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
