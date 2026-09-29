package geo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 假天地图：认 tk、按 LAYER 区分两层，key 不对时像真的一样回 200 + XML 错误页。
func fakeTianditu(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		q := r.URL.Query()
		if r.Header.Get("Referer") != "https://shop.example/" {
			t.Errorf("没带站点的 Referer：%q", r.Header.Get("Referer"))
		}
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		if q.Get("tk") != "good" {
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte("<ExceptionReport>invalid key</ExceptionReport>"))
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/"+q.Get("LAYER")+"_w/") {
			t.Errorf("路径 %s 与 LAYER=%s 对不上", r.URL.Path, q.Get("LAYER"))
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(q.Get("LAYER") + ":" + q.Get("TILEMATRIX") + "/" + q.Get("TILECOL") + "/" + q.Get("TILEROW")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTiandituTileAndErrors(t *testing.T) {
	var hits atomic.Int32
	srv := fakeTianditu(t, &hits)
	ctx := context.Background()

	s := NewTianditu("good", srv.URL, "https://shop.example/")
	if got := s.Layers(); len(got) != 2 || got[0] != "base" || got[1] != "label" {
		t.Fatalf("层应为 [base label]（底图在下、注记在上），实得 %v", got)
	}
	b, ct, err := s.Tile(ctx, "label", 12, 3421, 1560)
	if err != nil || ct != "image/png" || string(b) != "cva:12/3421/1560" {
		t.Fatalf("注记层：%q %q %v（TILECOL 是 x、TILEROW 是 y）", b, ct, err)
	}
	if _, _, err := s.Tile(ctx, "base", 19, 0, 0); !errors.Is(err, ErrTileNotFound) {
		t.Errorf("超过最大级别应 ErrTileNotFound，实得 %v", err)
	}
	if _, _, err := s.Tile(ctx, "base", 2, 4, 0); !errors.Is(err, ErrTileNotFound) {
		t.Errorf("x 越界应 ErrTileNotFound，实得 %v", err)
	}
	if _, _, err := s.Tile(ctx, "satellite", 2, 0, 0); !errors.Is(err, ErrTileNotFound) {
		t.Errorf("不认识的层应 ErrTileNotFound，实得 %v", err)
	}
	// key 错：服务商回 200 + XML，不能当成一张图发给客户端（更不能进缓存）。
	if _, _, err := NewTianditu("bad", srv.URL, "https://shop.example/").Tile(ctx, "base", 1, 0, 0); !errors.Is(err, ErrUpstream) {
		t.Errorf("key 错应 ErrUpstream，实得 %v", err)
	}
}

func TestCachedTilesHitsAndEvicts(t *testing.T) {
	var hits atomic.Int32
	srv := fakeTianditu(t, &hits)
	ctx := context.Background()

	c := NewCachedTiles(NewTianditu("good", srv.URL, "https://shop.example/"), 20) // 每张 9 字节（"vec:5/1/1"）：放得下两张
	for range 3 {
		if _, _, err := c.Tile(ctx, "base", 5, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("同一张瓦片取三次应只打一次服务商，实得 %d 次", hits.Load())
	}
	_, _, _ = c.Tile(ctx, "base", 5, 2, 2)
	_, _, _ = c.Tile(ctx, "base", 5, 1, 1) // 让 (1,1) 变成最近用过的
	_, _, _ = c.Tile(ctx, "base", 5, 3, 3) // 超上限：淘汰最久没用的 (2,2)
	before := hits.Load()
	_, _, _ = c.Tile(ctx, "base", 5, 1, 1)
	if hits.Load() != before {
		t.Error("最近用过的瓦片不该被淘汰")
	}
	_, _, _ = c.Tile(ctx, "base", 5, 2, 2)
	if hits.Load() != before+1 {
		t.Error("最久没用的瓦片应已被淘汰、要重新取")
	}
	if c.bytes > 20 {
		t.Errorf("缓存字节数 %d 超过上限 20", c.bytes)
	}

	// 失败不进缓存。
	bad := NewCachedTiles(NewTianditu("bad", srv.URL, "https://shop.example/"), 1<<20)
	_, _, _ = bad.Tile(ctx, "base", 1, 0, 0)
	n := hits.Load()
	_, _, _ = bad.Tile(ctx, "base", 1, 0, 0)
	if hits.Load() != n+1 {
		t.Error("服务商出错的结果不该被缓存")
	}
}

func TestTilesFromEnv(t *testing.T) {
	if s, err := TilesFromEnv("", "", "", 0, 0, ""); s != nil || err != nil {
		t.Errorf("没配应 (nil, nil)，实得 (%v, %v)", s, err)
	}
	if _, err := TilesFromEnv("tianditu", "", "https://a.example/", 0, 0, ""); err == nil {
		t.Error("选了 tianditu 却没给 key 应报错（部署错误，启动即失败）")
	}
	if _, err := TilesFromEnv("tianditu", "k", "", 0, 0, ""); err == nil {
		t.Error("选了 tianditu 却没给 referer 应报错（浏览器端 key 按域名校验）")
	}
	if _, err := TilesFromEnv("gaode", "k", "", 0, 0, ""); err == nil {
		t.Error("不认识的服务商应报错")
	}
	if s, err := TilesFromEnv(" Tianditu ", "k", "https://a.example/", 10, 40, ""); err != nil || s.Name() != "tianditu" {
		t.Errorf("大小写与空白应容忍：%v %v", s, err)
	}
	if _, err := TilesFromEnv("osm", "", "", 10, 40, "not a url"); err == nil {
		t.Error("KEEL_TILE_PROXY 写错应报错（部署错误，启动即失败）")
	}
	if s, err := TilesFromEnv("osm", "", "", 10, 40, "http://127.0.0.1:8890"); err != nil || tileProxy == nil || tileProxy.Host != "127.0.0.1:8890" || s.Name() != "osm" {
		t.Errorf("代理地址应被采用：%v %v", tileProxy, err)
	}
	if s, err := TilesFromEnv("osm", "", "", 10, 40, ""); err != nil || s.Name() != "osm" || len(s.Layers()) != 1 {
		t.Errorf("osm 不需要 key、只有一层：%v %v", s, err)
	}
}

// 总闸：不分来源、只数真打到服务商的请求；缓存命中不占额度，额度随时间回来。
func TestCachedTilesUpstreamLimit(t *testing.T) {
	var hits atomic.Int32
	srv := fakeTianditu(t, &hits)
	ctx := context.Background()
	c := NewCachedTiles(NewTianditu("good", srv.URL, "https://shop.example/"), 1<<20)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	c.SetUpstreamLimit(1, 2)

	for x := range 2 {
		if _, _, err := c.Tile(ctx, "base", 5, x, 0); err != nil {
			t.Fatalf("瞬时额度内的第 %d 张：%v", x+1, err)
		}
	}
	if _, _, err := c.Tile(ctx, "base", 5, 9, 0); !errors.Is(err, ErrUpstreamBusy) {
		t.Fatalf("超过瞬时额度应 ErrUpstreamBusy，实得 %v", err)
	}
	if _, _, err := c.Tile(ctx, "base", 5, 0, 0); err != nil {
		t.Errorf("缓存命中不该过总闸：%v", err)
	}
	if _, _, err := c.Tile(ctx, "base", 30, 0, 0); !errors.Is(err, ErrTileNotFound) {
		t.Errorf("越界的请求应先按 ErrTileNotFound 拒，不占额度：%v", err)
	}
	now = now.Add(time.Second)
	if _, _, err := c.Tile(ctx, "base", 5, 9, 0); err != nil {
		t.Errorf("一秒后额度回来一张：%v", err)
	}
	if hits.Load() != 3 {
		t.Errorf("真打到服务商的应是 3 次，实得 %d", hits.Load())
	}
}
