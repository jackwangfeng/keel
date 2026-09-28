package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// Suggest 实现 GET /geo/suggest?q=&lat=&lng=&city=（关键字 → 候选地点）。
func (h *GeoHandler) Suggest(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	city := strings.TrimSpace(c.Query("city"))
	lat, lng, _, ok := geoCoord(c.Query("lat"), c.Query("lng"))
	if q == "" || len([]rune(q)) > 50 || len([]rune(city)) > 20 || !ok {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"q 必填（1–50 字）；lat / lng 要么都给要么都不给；city 不超过 20 字")
		return
	}
	if !h.ready(c) {
		return
	}
	ps, err := h.p.Suggest(c.Request.Context(), q, lat, lng, city)
	if err != nil {
		writeGeoError(c, err)
		return
	}
	items := make([]api.GeoPlace, 0, len(ps))
	for _, p := range ps {
		items = append(items, apiGeoPlace(p))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
