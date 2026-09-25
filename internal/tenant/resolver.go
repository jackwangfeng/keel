package tenant

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Resolver 把请求映射到商家。它是租户进入 context 的唯一入口，
// 后面的 handler / service / repository 都只从 ctx 里取。
//
// 租户来源只有两种，都不可由客户端伪造：
//   - 配置的默认商家（单商家部署，docker compose 的常规形态）
//   - Host 头（多商家部署）
//
// 刻意不支持用请求头指定租户。公开接口没有鉴权，
// 那等于让调用方自己声明它是哪家店。本地开发的便利由默认商家提供。
//
// Host 当然是调用方能随便写的字符串，所以它只被当作「一个查询键」，
// 而不是一份凭据：写什么都要在库里对上一家 status = 1 的商家，对不上就是 404。
// 真实部署里由反向代理按域名分流，能到达进程的 Host 已经被前面一层过滤过一轮。
type Resolver struct {
	pool *pgxpool.Pool

	// defaultCode 非空即「单商家部署」：这套部署只服务这一家。
	defaultCode string

	mu    sync.RWMutex
	cache map[string]entry
}

type entry struct {
	id  int64
	exp time.Time
}

// cacheTTL 决定一次商家变更最久多久生效。
//
// 不做成永不过期：停用一家商家（status = 2）后，它必须真的访问不了，
// 而不是「等到下次重启进程才访问不了」。缓存本来就是为了省掉每个请求一次查询，
// 半分钟的窗口足够达到那个目的，同时把「停用」的生效延迟压在运维能接受的范围内。
const cacheTTL = 30 * time.Second

// ErrNoMerchant 表示这个请求找不到对应的、可访问的商家。
// 它是「这家店不存在」，不是「服务器出错」——中间件据此回 404 而不是 500。
var ErrNoMerchant = errors.New("没有匹配的可访问商家")

// NewResolver 建解析器。defaultCode 为空表示多商家部署，租户完全由 Host 决定。
//
// pool 请用 db.NewPool 建：它把「这条连接能不能绕过 RLS」的自检挂在每条物理
// 连接上。这里查的 merchants/shop_settings 没有 RLS，但同一个池会被 repository
// 拿去查有 RLS 的表。
func NewResolver(pool *pgxpool.Pool, defaultCode string) *Resolver {
	return &Resolver{pool: pool, defaultCode: defaultCode, cache: map[string]entry{}}
}

// Middleware 解析租户并放进 request context。解析不到就中止请求。
func (r *Resolver) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := r.Resolve(c.Request.Context(), c.Request.Host)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoMerchant):
			// 这家店不存在（或已停用）。404，而不是回落到某个商家。
			c.AbortWithStatus(http.StatusNotFound)
			return
		default:
			// 查库失败是服务端故障。把它也报成 404 的话，一次数据库抖动会表现为
			// 「所有店铺集体下架」，而监控上看不到任何 5xx。
			_ = c.Error(err)
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		c.Request = c.Request.WithContext(NewContext(c.Request.Context(), id))
		c.Next()
	}
}

// Resolve 由 Host 与配置定出商家 ID。
//
// 规则（两种部署形态的差别全在这里）：
//
//	配了默认商家（单商家部署）
//	    裸主机名（localhost、容器名、IP）→ 默认商家
//	    域名                            → 必须解析到默认商家本身，否则 404
//	没配默认商家（多商家部署）
//	    一律由 Host 决定，解析不到就 404
//
// 「域名必须解析到默认商家本身」这条是要点：
//   - 不能在解析不到时回落到默认商家 —— 那样随便拼一个不存在的子域名就能
//     看到默认店的数据，等于把它挂在了整个通配域下面。
//   - 也不能干脆无视 Host —— 那样 nope.example.com 照样返回默认店的数据，
//     观察到的行为和上一条一模一样。
//   - 更不能让 Host 在单商家部署里把请求领到别家店去：Host 是调用方写的，
//     那就成了「调用方自己挑租户」。
//
// 于是单商家部署要挂在真实域名上时，需要给那家店在 shop_settings.domain 里
// 登记这个域名 —— 一行数据，换来「域名归属某家店」这件事是库里的事实，
// 而不是配置文件里的口头约定。
func (r *Resolver) Resolve(ctx context.Context, host string) (int64, error) {
	name := normalizeHost(host)

	if r.defaultCode == "" {
		return r.byHost(ctx, name)
	}

	def, err := r.byCode(ctx, r.defaultCode)
	if err != nil {
		// 默认商家不存在或已停用。这是配置问题，但从调用方看确实没有可访问的店。
		return 0, err
	}
	if isBareHost(name) {
		// 主机名没有指名任何店，走部署的默认值。
		return def, nil
	}
	id, err := r.byHost(ctx, name)
	if err != nil {
		return 0, err
	}
	if id != def {
		// Host 指名了另一家店，而这套部署不服务它。
		return 0, ErrNoMerchant
	}
	return def, nil
}

// normalizeHost 去掉端口、统一小写、去掉 FQDN 结尾的点。
//
// Host 头里这三样都可能出现，而它们都不改变「这是哪个域名」：不归一化的话，
// SHOP-B.example.com. 和 shop-b.example.com 是两个不同的缓存键、两次查不到的
// 查询，最后是一个只在某些客户端上复现的 404。
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	// SplitHostPort 只在真有端口时成功，同时正确处理 [::1]:8080 这种字面量。
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// isBareHost 判断这个主机名有没有在指名某一家店。
//
// 裸主机名 = 不带点的单段名字（localhost、compose 里的服务名）或 IP 字面量。
// 这类地址不可能编码店铺身份，所以单商家部署下它们走默认商家。
// 带点的域名一律当作在指名某家店，必须能在库里对上 —— 对不上就是 404。
func isBareHost(name string) bool {
	if name == "" {
		return true
	}
	if net.ParseIP(strings.Trim(name, "[]")) != nil {
		return true
	}
	return !strings.Contains(name, ".")
}

// shopLabel 取域名的第一段，也就是子域名形态下的商家 code。
func shopLabel(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// byHost 按主机名找商家：子域名匹配 merchants.code，或完整域名匹配
// shop_settings.domain。
//
// 两个参数不能是同一个值：自定义域名（custom.example.net）的第一段不是任何
// 商家的 code，拿第一段去比完整域名的话，自定义域名这条路永远匹配不上。
func (r *Resolver) byHost(ctx context.Context, name string) (int64, error) {
	if name == "" {
		return 0, ErrNoMerchant
	}
	// status = 1 才算可访问：停用的商家不该还能被逛。
	// 同一个主机名理论上可能一边撞上某家的 code、一边撞上另一家的自定义域名；
	// 完整域名是更具体的匹配，让它赢，并用 id 兜底保证结果稳定。
	const q = `
	SELECT m.id
	  FROM merchants m
	  LEFT JOIN shop_settings s ON s.merchant_id = m.id
	 WHERE m.deleted_at IS NULL
	   AND m.status = 1
	   AND (m.code = $1 OR s.domain = $2)
	 ORDER BY (s.domain IS NOT DISTINCT FROM $2) DESC, m.id
	 LIMIT 1`
	return r.lookup(ctx, "host:"+name, q, shopLabel(name), name)
}

// byCode 按 code 找商家，用于配置的默认商家。
func (r *Resolver) byCode(ctx context.Context, code string) (int64, error) {
	const q = `
	SELECT m.id
	  FROM merchants m
	 WHERE m.deleted_at IS NULL
	   AND m.status = 1
	   AND m.code = $1`
	return r.lookup(ctx, "code:"+code, q, code)
}

// lookup 查一次并缓存命中结果。
//
// 只缓存命中：把「查不到」也缓存起来的话，一家店建好后的头 TTL 秒会继续 404，
// 而那正是有人盯着页面刷新的时刻。查不到的键每次都要查库，代价是一次索引命中。
func (r *Resolver) lookup(ctx context.Context, key, q string, args ...any) (int64, error) {
	now := time.Now()

	r.mu.RLock()
	e, ok := r.cache[key]
	r.mu.RUnlock()
	if ok && now.Before(e.exp) {
		return e.id, nil
	}

	var id int64
	switch err := r.pool.QueryRow(ctx, q, args...).Scan(&id); {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrNoMerchant
	default:
		return 0, err
	}

	r.mu.Lock()
	r.cache[key] = entry{id: id, exp: now.Add(cacheTTL)}
	r.mu.Unlock()
	return id, nil
}
