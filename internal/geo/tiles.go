package geo

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
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
//	            要「浏览器端」类型的 key（按域名白名单校验，代理带 Referer，见 NewTianditu）。
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
	referer           string
	client            *http.Client
}

func (h *httpTiles) Name() string        { return h.name }
func (h *httpTiles) Layers() []string    { return h.layers }
func (h *httpTiles) MaxZoom() int        { return h.maxZoom }
func (h *httpTiles) Attribution() string { return h.attribution }

// maxTileBytes：一张 256px 瓦片正常几 KB 到几十 KB；超过这个数按服务商出错处理，不往缓存里塞。
const maxTileBytes = 1 << 20

// tileAttempts：天地图的域名解析到两台（华为云 WAF）节点，2026-09-29 实测其中一台对同一个合法请求
// **每次**都回 418「疑似攻击」，另一台每次都 200。所以 418 不是「请求有问题」，换一台再来就好：
// 每次重试都新建连接（不复用那条落在坏节点上的 keep-alive），并随机挑一个解析出来的地址（见 dialAnyAddr）。
const tileAttempts = 3

func (h *httpTiles) Tile(ctx context.Context, layer string, z, x, y int) ([]byte, string, error) {
	if !hasLayer(h, layer) || !ValidTile(h, z, x, y) {
		return nil, "", ErrTileNotFound
	}
	var err error
	for i := 0; i < tileAttempts; i++ {
		var b []byte
		var ct string
		var status int
		b, ct, status, err = h.fetch(ctx, h.url(layer, z, x, y), i > 0)
		if err == nil || status != http.StatusTeapot || ctx.Err() != nil {
			return b, ct, err
		}
	}
	return nil, "", err
}

func (h *httpTiles) fetch(ctx context.Context, u string, fresh bool) ([]byte, string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("User-Agent", tileUA)
	if h.referer != "" {
		req.Header.Set("Referer", h.referer)
	}
	req.Close = fresh
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		// 天地图 key 不对 / 超额时回 200 + XML 或 HTML 错误页，按 Content-Type 认出来。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxTileBytes))
		return nil, "", resp.StatusCode, fmt.Errorf("%w: HTTP %d %s", ErrUpstream, resp.StatusCode, ct)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTileBytes+1))
	if err != nil {
		return nil, "", resp.StatusCode, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if len(b) > maxTileBytes {
		return nil, "", resp.StatusCode, fmt.Errorf("%w: 瓦片超过 %d 字节", ErrUpstream, maxTileBytes)
	}
	return b, ct, resp.StatusCode, nil
}

// dialAnyAddr 解析出全部地址、从随机一个开始依次试。默认拨号器总按解析顺序拨第一个能连上的，
// 顺序不变时每次重试都会落回同一台坏节点。
func dialAnyAddr(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	start := rand.IntN(len(ips))
	d := &net.Dialer{Timeout: 3 * time.Second}
	var last error
	for i := range ips {
		ip := ips[(start+i)%len(ips)]
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func newTileClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dialAnyAddr
	return &http.Client{Timeout: 5 * time.Second, Transport: t}
}

// NewTianditu 建天地图瓦片源。base 为空用官方 t0–t7 子域名轮换（测试时指向 httptest）。
//
// key 必须是**浏览器端**类型：天地图的服务端 key 只开放地名搜索 / 地理编码这类 Web 服务，取瓦片回 403
// 「权限类型错误」（2026-09-29 实测）。浏览器端 key 按来源域名校验，所以代理替客户端取瓦片时带上
// referer（部署的站点地址，与控制台里 key 的域名白名单一致）—— 在天地图看来就是这个站点在显示地图。
// 同一天实测：不带 Referer、从服务端连打两百来次的一把浏览器端 key 被禁用（403「Key已被禁用」）。
func NewTianditu(key, base, referer string) TileSource {
	layerOf := map[string]string{"base": "vec", "label": "cva"}
	return &httpTiles{
		name: "tianditu", attribution: "© 天地图 · 国家地理信息公共服务平台", maxZoom: 18,
		layers: []string{"base", "label"}, client: newTileClient(), referer: referer,
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
		layers: []string{"base"}, client: newTileClient(),
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

	// 总闸（缓存没命中、真要打到服务商的那一步）：不分来源 IP 的一只令牌桶。按 IP 的限流挡得住一个脚本，
	// 挡不住很多个 IP 一起刷；天地图的 key 有日调用量上限，超额或被判滥用就整站没有地图。
	// 超过总闸的请求回 ErrUpstreamBusy（接口 503），不排队等。upRate <= 0 关闭总闸。
	upRate, upBurst, upTokens float64
	upLast                    time.Time
}

// ErrUpstreamBusy：瓦片总闸满了（全站向服务商取瓦片的速率超过上限）。
var ErrUpstreamBusy = fmt.Errorf("%w: 瓦片请求太多，稍后再试", ErrUpstream)

// DefaultTileUpstreamPerSec / DefaultTileUpstreamBurst：全站每秒最多向服务商取 10 张、瞬时 40 张。
// 一个人打开地图是两层四五十张的一屏，缓存冷的时候瞬时额度够一个人；缓存热了之后绝大多数请求不过这道闸。
// 按天算，10 张/秒打满也是 86 万张 —— 真打到这个量级先该怀疑是被刷了。
const (
	DefaultTileUpstreamPerSec = 10.0
	DefaultTileUpstreamBurst  = 40.0
)

// SetUpstreamLimit 设总闸；ratePerSec <= 0 关闭。
func (c *CachedTiles) SetUpstreamLimit(ratePerSec, burst float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upRate, c.upBurst, c.upTokens, c.upLast = ratePerSec, burst, burst, c.now()
}

func (c *CachedTiles) takeUpstreamLocked() bool {
	if c.upRate <= 0 {
		return true
	}
	now := c.now()
	c.upTokens = min(c.upBurst, c.upTokens+now.Sub(c.upLast).Seconds()*c.upRate)
	c.upLast = now
	if c.upTokens < 1 {
		return false
	}
	c.upTokens--
	return true
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
	if !ValidTile(c.TileSource, z, x, y) { // 越界的不占总闸的额度
		c.mu.Unlock()
		return nil, "", ErrTileNotFound
	}
	if !c.takeUpstreamLocked() {
		c.mu.Unlock()
		return nil, "", ErrUpstreamBusy
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

// TilesFromEnv 按 KEEL_MAP_TILES / KEEL_TIANDITU_KEY / KEEL_TIANDITU_REFERER 建瓦片源；没配返回 nil（地图不开）。
// 选了 tianditu 却没给 key 或 referer、或者写了不认识的名字，是部署错误（启动即失败，与 KEEL_GEO_PROVIDER 同一个处理）。
//
// upPerSec / upBurst 是总闸（见 CachedTiles.upRate），<= 0 关闭。
func TilesFromEnv(provider, tiandituKey, tiandituReferer string, upPerSec, upBurst float64) (TileSource, error) {
	provider, tiandituKey = strings.TrimSpace(strings.ToLower(provider)), strings.TrimSpace(tiandituKey)
	switch provider {
	case "":
		return nil, nil
	case "tianditu":
		if tiandituKey == "" {
			return nil, errors.New("KEEL_MAP_TILES=tianditu 需要 KEEL_TIANDITU_KEY（天地图「浏览器端」类型的 key）")
		}
		ref := strings.TrimSpace(tiandituReferer)
		if !strings.HasPrefix(ref, "https://") && !strings.HasPrefix(ref, "http://") {
			return nil, errors.New("KEEL_MAP_TILES=tianditu 需要 KEEL_TIANDITU_REFERER（站点地址，如 https://shop.example.com/，" +
				"与天地图控制台里 key 的域名白名单一致）")
		}
		return withUpstreamLimit(NewCachedTiles(NewTianditu(tiandituKey, "", ref), DefaultTileCacheBytes), upPerSec, upBurst), nil
	case "osm":
		return withUpstreamLimit(NewCachedTiles(NewOSMTiles(""), DefaultTileCacheBytes), upPerSec, upBurst), nil
	default:
		return nil, fmt.Errorf("KEEL_MAP_TILES=%q 不认识（tianditu / osm）", provider)
	}
}

func withUpstreamLimit(c *CachedTiles, perSec, burst float64) *CachedTiles {
	c.SetUpstreamLimit(perSec, burst)
	return c
}
