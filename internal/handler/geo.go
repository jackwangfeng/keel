package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/geo"
	"github.com/keel/keel/internal/problem"
)

// POI 与地址（docs/POI-设计.md）：服务端代理地图服务商，坐标一律 WGS-84，key 不下发。
// 两条接口各占一个文件（geo_reverse.go、geo_suggest.go）：它们读 query 参数，contract_test.go 按文件对账 c.Query。
// 这个文件只放共用的部分。

// GeoHandler 包一个 geo.Provider；Provider 为 nil 即没配地图服务商（接口回 501）。
type GeoHandler struct {
	p geo.Provider
}

func NewGeoHandler(p geo.Provider) *GeoHandler { return &GeoHandler{p: p} }

func apiGeoPlace(p geo.Place) api.GeoPlace {
	return api.GeoPlace{Name: p.Name, Address: p.Address, Province: p.Province, City: p.City, District: p.District,
		Adcode: p.Adcode, Street: p.Street, Lat: p.Lat, Lng: p.Lng}
}

func (h *GeoHandler) ready(c *gin.Context) bool {
	if h.p == nil {
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"没有配置地图服务商：请手动填写地址，或在地图上选点")
		return false
	}
	return true
}

func writeGeoError(c *gin.Context, err error) {
	if errors.Is(err, geo.ErrUpstream) {
		_ = c.Error(err)
		problem.Write(c, http.StatusServiceUnavailable, problem.TypeGeoUnavailable, "地图服务暂时不可用，请稍后再试或手动填写")
		return
	}
	_ = c.Error(err)
	problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
}

// geoCoord 解析一对坐标；两个都没给返回 (0, 0, false, true)。
func geoCoord(latRaw, lngRaw string) (lat, lng float64, given, ok bool) {
	latRaw, lngRaw = strings.TrimSpace(latRaw), strings.TrimSpace(lngRaw)
	if latRaw == "" && lngRaw == "" {
		return 0, 0, false, true
	}
	la, err1 := strconv.ParseFloat(latRaw, 64)
	ln, err2 := strconv.ParseFloat(lngRaw, 64)
	if err1 != nil || err2 != nil || la < -90 || la > 90 || ln < -180 || ln > 180 {
		return 0, 0, true, false
	}
	return la, ln, true, true
}
