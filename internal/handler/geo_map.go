package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/geo"
	"github.com/keel/keel/internal/problem"
)

// 地图底图（docs/POI-设计.md「地图底图」）：/geo/map 报配置，/geo/tiles 转发瓦片。key 只在服务端。

// MapHandler 包一个 geo.TileSource；为 nil 即没配瓦片服务商（/geo/map 报 enabled=false，/geo/tiles 回 501）。
type MapHandler struct {
	s geo.TileSource
}

func NewMapHandler(s geo.TileSource) *MapHandler { return &MapHandler{s: s} }

// tileMaxAge：瓦片在客户端缓存 7 天，与服务端缓存同一个时长（底图变化很慢）。
const tileMaxAge = "public, max-age=604800"

// Config 实现 GET /geo/map。
func (h *MapHandler) Config(c *gin.Context) {
	if h.s == nil {
		c.JSON(http.StatusOK, api.GeoMapConfig{Layers: []string{}})
		return
	}
	c.JSON(http.StatusOK, api.GeoMapConfig{Enabled: true, Layers: h.s.Layers(), MaxZoom: h.s.MaxZoom(),
		Attribution: h.s.Attribution()})
}

// Tile 实现 GET /geo/tiles/:layer/:z/:x/:y。
func (h *MapHandler) Tile(c *gin.Context) {
	if h.s == nil {
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented, "没有配置地图底图")
		return
	}
	known := false
	for _, l := range h.s.Layers() {
		if l == c.Param("layer") {
			known = true
		}
	}
	if !known {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "没有这一层瓦片")
		return
	}
	z, err1 := strconv.Atoi(c.Param("z"))
	x, err2 := strconv.Atoi(c.Param("x"))
	y, err3 := strconv.Atoi(c.Param("y"))
	if err1 != nil || err2 != nil || err3 != nil || !geo.ValidTile(h.s, z, x, y) {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "瓦片级别或行列号越界")
		return
	}
	b, ct, err := h.s.Tile(c.Request.Context(), c.Param("layer"), z, x, y)
	if err != nil {
		if errors.Is(err, geo.ErrTileNotFound) {
			problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "没有这一张瓦片")
			return
		}
		writeGeoError(c, err)
		return
	}
	c.Header("Cache-Control", tileMaxAge)
	c.Data(http.StatusOK, ct, b)
}
