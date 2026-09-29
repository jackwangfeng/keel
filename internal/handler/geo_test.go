package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/geo"
	"github.com/keel/keel/internal/handler"
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

// 地图底图：测试环境没配瓦片服务商 → /geo/map 报 enabled=false（客户端不画地图），/geo/tiles 回 501。
func TestGeoMapWithoutTiles(t *testing.T) {
	sh := newAdminShop(t)
	w := getNoAuth(t, sh.Host, "/api/v1/geo/map")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) || !strings.Contains(w.Body.String(), `"layers":[]`) {
		t.Fatalf("/geo/map：%d %s", w.Code, w.Body.String())
	}
	if p := problemOf(t, getNoAuth(t, sh.Host, "/api/v1/geo/tiles/base/1/0/0"), http.StatusNotImplemented); p.Type != problem.TypeNotImplemented {
		t.Errorf("type %s", p.Type)
	}
}

type fakeTiles struct{ calls int }

func (f *fakeTiles) Name() string        { return "fake" }
func (f *fakeTiles) Layers() []string    { return []string{"base", "label"} }
func (f *fakeTiles) MaxZoom() int        { return 18 }
func (f *fakeTiles) Attribution() string { return "© 假地图" }
func (f *fakeTiles) Tile(_ context.Context, layer string, z, x, y int) ([]byte, string, error) {
	f.calls++
	if layer == "label" && z == 3 {
		return nil, "", geo.ErrUpstream
	}
	return []byte(fmt.Sprintf("%s/%d/%d/%d", layer, z, x, y)), "image/png", nil
}

// 配了瓦片服务商：配置照实报、瓦片原样转发并带缓存头；层名 / 越界 / 服务商出错各自的状态码。
func TestGeoTilesProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := &fakeTiles{}
	h := handler.NewMapHandler(f)
	r := gin.New()
	r.GET("/geo/map", h.Config)
	r.GET("/geo/tiles/:layer/:z/:x/:y", h.Tile)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	var cfg api.GeoMapConfig
	if w := get("/geo/map"); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cfg) != nil ||
		!cfg.Enabled || len(cfg.Layers) != 2 || cfg.MaxZoom != 18 || cfg.Attribution != "© 假地图" {
		t.Fatalf("/geo/map：%d %s", w.Code, w.Body.String())
	}
	w := get("/geo/tiles/label/12/3421/1560")
	if w.Code != http.StatusOK || w.Body.String() != "label/12/3421/1560" || w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("Cache-Control") != "public, max-age=604800" {
		t.Fatalf("瓦片：%d %q %v", w.Code, w.Body.String(), w.Header())
	}
	for _, c := range []struct {
		path string
		want int
	}{
		{"/geo/tiles/satellite/1/0/0", http.StatusNotFound},
		{"/geo/tiles/base/19/0/0", http.StatusUnprocessableEntity},
		{"/geo/tiles/base/2/4/0", http.StatusUnprocessableEntity},
		{"/geo/tiles/base/a/0/0", http.StatusUnprocessableEntity},
		{"/geo/tiles/base/1/-1/0", http.StatusUnprocessableEntity},
		{"/geo/tiles/label/3/0/0", http.StatusServiceUnavailable},
	} {
		if w := get(c.path); w.Code != c.want {
			t.Errorf("%s：%d，期望 %d", c.path, w.Code, c.want)
		}
	}
	if f.calls != 2 {
		t.Errorf("参数不合法的请求不该打到服务商：实际调用 %d 次，期望 2 次", f.calls)
	}
}
