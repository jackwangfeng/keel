package geo

import (
	"context"
	"errors"
)

// Place 是一个地点（地址），坐标一律 WGS-84。字段与收货地址对齐：Adcode 就是地址的 region_code（运费按它算）。
type Place struct {
	Name     string  `json:"name"`
	Address  string  `json:"address"`
	Province string  `json:"province"`
	City     string  `json:"city"`
	District string  `json:"district"`
	Adcode   string  `json:"adcode"`
	Street   string  `json:"street"`
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
}

// Provider 是地图服务商。实现方负责把自家坐标系换成 WGS-84（入参也按 WGS-84 给）。
type Provider interface {
	// Name 是服务商名（amap / tencent），进日志与响应头。
	Name() string
	// Reverse 是逆地理编码：坐标 → 地址。
	Reverse(ctx context.Context, lat, lng float64) (Place, error)
	// Suggest 是输入提示：关键字 → 候选地点（带坐标的）。lat/lng 可为 0（不按位置排序），city 可为空。
	Suggest(ctx context.Context, q string, lat, lng float64, city string) ([]Place, error)
}

var (
	// ErrNotConfigured：没配服务商或 key。接口回 501，客户端退回手填 + 地图选点。
	ErrNotConfigured = errors.New("没有配置地图服务商（KEEL_GEO_PROVIDER / KEEL_GEO_KEY）")
	// ErrUpstream：服务商报错或不可达（额度用完、key 失效、网络）。接口回 503。
	ErrUpstream = errors.New("地图服务商不可用")
)
