// Package geo 是 POI 与地址那一层（docs/POI-设计.md）：服务商无关的接口、服务商实现（高德）与坐标换算。
package geo

import "math"

// 国内地图服务商返回的是 GCJ-02（「火星坐标」），库里、围栏、门店坐标全是 WGS-84。
// 换算放在这一处：服务商的结果进来时转成 WGS-84，客户端传给服务商的坐标出去时转成 GCJ-02。
// 算法是公开的近似式（与 web/admin/src/api/geo.ts 同源）；GCJ→WGS 用迭代求逆，误差在分米级。

const (
	earthA  = 6378245.0
	earthEE = 0.00669342162296594323
)

// OutOfChina：大陆范围以外不做偏移（GCJ-02 只在国内生效）。粗框，与各家实现一致。
func OutOfChina(lat, lng float64) bool {
	return lng < 72.004 || lng > 137.8347 || lat < 0.8293 || lat > 55.8271
}

func transformLat(x, y float64) float64 {
	r := -100.0 + 2.0*x + 3.0*y + 0.2*y*y + 0.1*x*y + 0.2*math.Sqrt(math.Abs(x))
	r += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
	r += (20.0*math.Sin(y*math.Pi) + 40.0*math.Sin(y/3.0*math.Pi)) * 2.0 / 3.0
	r += (160.0*math.Sin(y/12.0*math.Pi) + 320*math.Sin(y*math.Pi/30.0)) * 2.0 / 3.0
	return r
}

func transformLng(x, y float64) float64 {
	r := 300.0 + x + 2.0*y + 0.1*x*x + 0.1*x*y + 0.1*math.Sqrt(math.Abs(x))
	r += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
	r += (20.0*math.Sin(x*math.Pi) + 40.0*math.Sin(x/3.0*math.Pi)) * 2.0 / 3.0
	r += (150.0*math.Sin(x/12.0*math.Pi) + 300.0*math.Sin(x/30.0*math.Pi)) * 2.0 / 3.0
	return r
}

func delta(lat, lng float64) (float64, float64) {
	dLat := transformLat(lng-105.0, lat-35.0)
	dLng := transformLng(lng-105.0, lat-35.0)
	radLat := lat / 180.0 * math.Pi
	magic := math.Sin(radLat)
	magic = 1 - earthEE*magic*magic
	sqrtMagic := math.Sqrt(magic)
	dLat = (dLat * 180.0) / ((earthA * (1 - earthEE)) / (magic * sqrtMagic) * math.Pi)
	dLng = (dLng * 180.0) / (earthA / sqrtMagic * math.Cos(radLat) * math.Pi)
	return dLat, dLng
}

// WGS84ToGCJ02 把 WGS-84 转成 GCJ-02。
func WGS84ToGCJ02(lat, lng float64) (float64, float64) {
	if OutOfChina(lat, lng) {
		return lat, lng
	}
	dLat, dLng := delta(lat, lng)
	return lat + dLat, lng + dLng
}

// GCJ02ToWGS84 把 GCJ-02 转回 WGS-84（迭代求逆，最多 10 轮，收敛到 1e-9 度）。
func GCJ02ToWGS84(lat, lng float64) (float64, float64) {
	if OutOfChina(lat, lng) {
		return lat, lng
	}
	wLat, wLng := lat, lng
	for i := 0; i < 10; i++ {
		gLat, gLng := WGS84ToGCJ02(wLat, wLng)
		dLat, dLng := gLat-lat, gLng-lng
		wLat, wLng = wLat-dLat, wLng-dLng
		if math.Abs(dLat) < 1e-9 && math.Abs(dLng) < 1e-9 {
			break
		}
	}
	return wLat, wLng
}
