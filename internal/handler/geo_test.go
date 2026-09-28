package handler_test

import (
	"net/http"
	"testing"

	"github.com/keel/keel/internal/problem"
)

// POI 接口（docs/POI-设计.md）：测试环境没配地图服务商 → 501（客户端退回手填）；参数错先于 501 报 422。
// 服务商的调用本身在 internal/geo 的单测里（假高德）。
func TestGeoEndpointsWithoutProvider(t *testing.T) {
	sh := newAdminShop(t)
	for _, c := range []struct {
		path string
		want int
		typ  string
	}{
		{"/api/v1/geo/reverse?lat=39.9&lng=116.4", http.StatusNotImplemented, problem.TypeNotImplemented},
		{"/api/v1/geo/suggest?q=望京", http.StatusNotImplemented, problem.TypeNotImplemented},
		{"/api/v1/geo/reverse?lat=39.9", http.StatusUnprocessableEntity, problem.TypeInvalidRequest},
		{"/api/v1/geo/reverse?lat=99&lng=116.4", http.StatusUnprocessableEntity, problem.TypeInvalidRequest},
		{"/api/v1/geo/suggest?q=", http.StatusUnprocessableEntity, problem.TypeInvalidRequest},
		{"/api/v1/geo/suggest?q=望京&lat=39.9", http.StatusUnprocessableEntity, problem.TypeInvalidRequest},
	} {
		w := getNoAuth(t, sh.Host, c.path)
		if p := problemOf(t, w, c.want); p.Type != c.typ {
			t.Errorf("%s：type %s，期望 %s", c.path, p.Type, c.typ)
		}
	}
}
