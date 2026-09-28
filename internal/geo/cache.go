package geo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Cached 给任意 Provider 加一层进程内缓存：服务商的免费额度很小（高德个人认证输入提示 5000 次/月），
// 同一个坐标、同一个关键字不该每次都打出去。
//
//	逆地理编码：坐标取 5 位小数（约 1 米）为键，缓存 24 小时；
//	输入提示：(关键字, 城市, 坐标取 2 位小数≈1 公里) 为键，缓存 1 小时。
//
// 只缓存成功的结果；上限 5000 条，满了整表清空（简单，且缓存丢了只是多打几次服务商）。
type Cached struct {
	P Provider

	mu      sync.Mutex
	reverse map[string]cacheEntry[Place]
	suggest map[string]cacheEntry[[]Place]
	now     func() time.Time
}

type cacheEntry[T any] struct {
	v   T
	exp time.Time
}

const cacheMax = 5000

func NewCached(p Provider) *Cached {
	return &Cached{P: p, reverse: map[string]cacheEntry[Place]{}, suggest: map[string]cacheEntry[[]Place]{}, now: time.Now}
}

func (c *Cached) Name() string { return c.P.Name() }

func (c *Cached) Reverse(ctx context.Context, lat, lng float64) (Place, error) {
	k := fmt.Sprintf("%.5f,%.5f", lat, lng)
	c.mu.Lock()
	if e, ok := c.reverse[k]; ok && c.now().Before(e.exp) {
		c.mu.Unlock()
		return e.v, nil
	}
	c.mu.Unlock()
	v, err := c.P.Reverse(ctx, lat, lng)
	if err != nil {
		return Place{}, err
	}
	c.mu.Lock()
	if len(c.reverse) >= cacheMax {
		c.reverse = map[string]cacheEntry[Place]{}
	}
	c.reverse[k] = cacheEntry[Place]{v, c.now().Add(24 * time.Hour)}
	c.mu.Unlock()
	return v, nil
}

func (c *Cached) Suggest(ctx context.Context, q string, lat, lng float64, city string) ([]Place, error) {
	k := fmt.Sprintf("%s|%s|%.2f,%.2f", strings.TrimSpace(q), city, lat, lng)
	c.mu.Lock()
	if e, ok := c.suggest[k]; ok && c.now().Before(e.exp) {
		c.mu.Unlock()
		return e.v, nil
	}
	c.mu.Unlock()
	v, err := c.P.Suggest(ctx, q, lat, lng, city)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.suggest) >= cacheMax {
		c.suggest = map[string]cacheEntry[[]Place]{}
	}
	c.suggest[k] = cacheEntry[[]Place]{v, c.now().Add(time.Hour)}
	c.mu.Unlock()
	return v, nil
}

// FromEnv 按 KEEL_GEO_PROVIDER / KEEL_GEO_KEY 建服务商；没配返回 nil（接口回 501）。
func FromEnv(provider, key string) (Provider, error) {
	provider, key = strings.TrimSpace(strings.ToLower(provider)), strings.TrimSpace(key)
	if provider == "" || key == "" {
		return nil, nil
	}
	switch provider {
	case "amap":
		return NewCached(NewAmap(key)), nil
	default:
		return nil, fmt.Errorf("KEEL_GEO_PROVIDER=%q 不认识（目前只有 amap）", provider)
	}
}
