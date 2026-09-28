package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
)

// Reverse 实现 GET /geo/reverse?lat=&lng=（坐标 → 地址）。
func (h *GeoHandler) Reverse(c *gin.Context) {
	lat, lng, given, ok := geoCoord(c.Query("lat"), c.Query("lng"))
	if !ok || !given {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "lat、lng 必填且在有效范围内（WGS-84）")
		return
	}
	if !h.ready(c) {
		return
	}
	p, err := h.p.Reverse(c.Request.Context(), lat, lng)
	if err != nil {
		writeGeoError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiGeoPlace(p))
}
