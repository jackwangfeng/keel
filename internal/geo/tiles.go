package geo

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 地图底图瓦片（docs/POI-设计.md「地图底图」）：买家端地图选点、后台门店坐标与围栏编辑都从
// GET /geo/tiles/{layer}/{z}/{x}/{y} 取瓦片，Keel 转发给瓦片服务商。**key 只在服务端**，
// 与 /geo/reverse 同一条原则；客户端只认同源的一个地址，换服务商不用发版。
//
// 服务商：
//
//	tianditu —— 天地图（国家地理信息公共服务平台），有审图号，坐标 CGCS2000，与 WGS-84 的差在厘米级，
//	            门店 / 围栏 / 收货地址存的 WGS-84 直接画上去不偏。底图 vec + 注记 cva 两层（Web 墨卡托，_w）。
//	            要「服务端」类型的 key（绑服务器 IP 白名单）。
//	osm      —— OpenStreetMap 官方瓦片。只给开发 / 自测：国内细节少、没有审图号，
//	            而且官方瓦片服务的使用政策不许商用 App 大流量调用。
//
// 没配（KEEL_MAP_TILES 为空）时 /geo/map 报 enabled=false、/geo/tiles 回 501，客户端不显示地图。

// ErrTileNotFound 是层名不认识或坐标越界（接口回 404 / 422 由 handler 区分）。
var ErrTileNotFound = errors.New("没有这一层瓦片")

// TileSource 是一个瓦片服务商。
type TileSource interface {
	Name() string
	// Layers 是从下往上叠的层名（客户端照这个顺序叠）。
	Layers() []string
	MaxZoom() int
	Attribution() string
	// Tile 取一张瓦片，返回图片字节与 Content-Type。
	Tile(ctx context.Context, layer string, z, x, y int) ([]byte, string, error)
}

// ValidTile 判断 (z, x, y) 是不是这个服务商的一张合法瓦片（Web 墨卡托：x、y ∈ [0, 2^z)）。
func ValidTile(s TileSource, z, x, y int) bool {
	if z < 0 || z > s.MaxZoom() {
		return false
	}
	n := 1 << z
	return x >= 0 && x < n && y >= 0 && y < n
}

func hasLayer(s TileSource, layer string) bool {
	for _, l := range s.Layers() {
		if l == layer {
			return true
		}
	}
	return false
}

// tileUA：两家都要求带能识别出应用的 User-Agent（天地图对空 UA / 脚本 UA 回 418，OSM 的使用政策写明要）。
const tileUA = "Mozilla/5.0 (compatible; Keel/1.0; +https://github.com/jackwangfeng/keel)"

type httpTiles struct {
	name, attribution string
	maxZoom           int
	layers            []string
	url               func(layer string, z, x, y int) string
	client            *http.Client
}

func (h *httpTiles) Name() string        { return h.name }
func (h *httpTiles) Layers() []string    { return h.layers }
func (h *httpTiles) MaxZoom() int        { return h.maxZoom }
func (h *httpTiles) Attribution() string { return h.attribution }

// maxTileBytes：一张 256px 瓦片正常几 KB 到几十 KB；超过这个数按服务商出错处理，不往缓存里塞。
const maxTileBytes = 1 << 20

func (h *httpTiles) Tile(ctx context.Context, layer string, z, x, y int) ([]byte, string, error) {
	if !hasLayer(h, layer) || !ValidTile(h, z, x, y) {
		return nil, "", ErrTileNotFound
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url(layer, z, x, y), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", tileUA)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		// 天地图 key 不对 / 超额时回 200 + XML 或 HTML 错误页，按 Content-Type 认出来。
		return nil, "", fmt.Errorf("%w: HTTP %d %s", ErrUpstream, resp.StatusCode, ct)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if len(b) > maxTileBytes {
		return nil, "", fmt.Errorf("%w: 瓦片超过 %d 字节", ErrUpstream, maxTileBytes)
	}
	return b, ct, nil
}

// NewTianditu 建天地图瓦片源。base 为空用官方 t0–t7 子域名轮换（测试时指向 httptest）。
func NewTianditu(key, base string) TileSource {
	layerOf := map[string]string{"base": "vec", "label": "cva"}
	return &httpTiles{
		name: "tianditu", attribution: "© 天地图 · 国家地理信息公共服务平台", maxZoom: 18,
		layers: []string{"base", "label"}, client: &http.Client{Timeout: 5 * time.Second},
		url: func(layer string, z, x, y int) string {
			l := layerOf[layer]
			host := base
			if host == "" {
				host = fmt.Sprintf("https://t%d.tianditu.gov.cn", (x+y)%8)
			}
			return fmt.Sprintf("%s/%s_w/wmts?SERVICE=WMTS&REQUEST=GetTile&VERSION=1.0.0&LAYER=%s&STYLE=default"+
				"&TILEMATRIXSET=w&FORMAT=tiles&TILEMATRIX=%d&TILEROW=%d&TILECOL=%d&tk=%s", host, l, l, z, y, x, key)
		},
	}
}

// NewOSMTiles 建 OpenStreetMap 瓦片源（只给开发 / 自测，见文件头）。
func NewOSMTiles(base string) TileSource {
	if base == "" {
		base = "https://tile.openstreetmap.org"
	}
	return &httpTiles{
		name: "osm", attribution: "© OpenStreetMap contributors", maxZoom: 19,
		layers: []string{"base"}, client: &http.Client{Timeout: 5 * time.Second},
		url: func(_ string, z, x, y int) string { return fmt.Sprintf("%s/%d/%d/%d.png", base, z, x, y) },
	}
}

// CachedTiles 给瓦片源加一层进程内 LRU：同一片区域大家看的是同一批瓦片，天地图免费 key 有日调用量上限。
// 按字节数封顶（默认 64 MB，约几千张），淘汰最久没用的；缓存 7 天（底图变化很慢）。只缓存成功的结果。
type CachedTiles struct {
	TileSource

	mu       sync.Mutex
	maxBytes int
	bytes    int
	ll       *list.List
	items    map[string]*list.Element
	ttl      time.Duration
	now      func() time.Time
}

type tileEntry struct {
	key string
	b   []byte
	ct  string
	exp time.Time
}

// DefaultTileCacheBytes 是瓦片缓存的默认上限。
const DefaultTileCacheBytes = 64 << 20

func NewCachedTiles(s TileSource, maxBytes int) *CachedTiles {
	return &CachedTiles{TileSource: s, maxBytes: maxBytes, ll: list.New(), items: map[string]*list.Element{},
		ttl: 7 * 24 * time.Hour, now: time.Now}
}

func (c *CachedTiles) Tile(ctx context.Context, layer string, z, x, y int) ([]byte, string, error) {
	k := fmt.Sprintf("%s/%d/%d/%d", layer, z, x, y)
	c.mu.Lock()
	if el, ok := c.items[k]; ok {
		e := el.Value.(*tileEntry)
		if c.now().Before(e.exp) {
			c.ll.MoveToFront(el)
			c.mu.Unlock()
			return e.b, e.ct, nil
		}
		c.removeLocked(el)
	}
	c.mu.Unlock()
	b, ct, err := c.TileSource.Tile(ctx, layer, z, x, y)
	if err != nil {
		return nil, "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok { // 并发的另一次请求先放进来了
		c.removeLocked(el)
	}
	c.items[k] = c.ll.PushFront(&tileEntry{key: k, b: b, ct: ct, exp: c.now().Add(c.ttl)})
	c.bytes += len(b)
	for c.bytes > c.maxBytes && c.ll.Len() > 0 {
		c.removeLocked(c.ll.Back())
	}
	return b, ct, nil
}

func (c *CachedTiles) removeLocked(el *list.Element) {
	e := el.Value.(*tileEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.bytes -= len(e.b)
}

// TilesFromEnv 按 KEEL_MAP_TILES / KEEL_TIANDITU_KEY 建瓦片源；没配返回 nil（地图不开）。
// 选了 tianditu 却没给 key、或者写了不认识的名字，是部署错误（启动即失败，与 KEEL_GEO_PROVIDER 同一个处理）。
func TilesFromEnv(provider, tiandituKey string) (TileSource, error) {
	provider, tiandituKey = strings.TrimSpace(strings.ToLower(provider)), strings.TrimSpace(tiandituKey)
	switch provider {
	case "":
		return nil, nil
	case "tianditu":
		if tiandituKey == "" {
			return nil, errors.New("KEEL_MAP_TILES=tianditu 需要 KEEL_TIANDITU_KEY（天地图「服务端」类型的 key）")
		}
		return NewCachedTiles(NewTianditu(tiandituKey, ""), DefaultTileCacheBytes), nil
	case "osm":
		return NewCachedTiles(NewOSMTiles(""), DefaultTileCacheBytes), nil
	default:
		return nil, fmt.Errorf("KEEL_MAP_TILES=%q 不认识（tianditu / osm）", provider)
	}
}
