package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultCacheTTL 是解析结果的默认缓存时长，也就是一次商家变更最久多久生效。
//
// 不做成永不过期：停用一家商家（status = 2）后，它必须真的访问不了，
// 而不是「等到下次重启进程才访问不了」。半分钟足够省掉每个请求一次查询，
// 同时把「停用」的生效延迟压在运维能接受的范围内。
const DefaultCacheTTL = 30 * time.Second

// ErrNoMerchant 表示这个请求找不到对应的、可访问的商家。
// 它是「这家店不存在」，不是「服务器出错」——中间件据此回 404 而不是 500。
var ErrNoMerchant = errors.New("没有匹配的可访问商家")

// Config 是解析器的部署配置。两个字段决定了这套部署是哪种形态。
type Config struct {
	// DefaultCode 非空即「单商家部署」：这套部署只服务这一家，Host 完全不参与解析。
	// 对应环境变量 KEEL_DEFAULT_MERCHANT。
	//
	// 「库里其实有好几家店，却还配着默认商家」这种误配不靠每个请求去猜，
	// 由 Preflight 在启动时一次性拒绝。
	DefaultCode string

	// BaseDomain 是多商家部署的平台基础域名，例如 example.com。
	// 对应环境变量 KEEL_BASE_DOMAIN。
	//
	// 子域名匹配必须锚定在它下面：不锚定的话，`shop-b.attacker.example.org`
	// 也会解析到 shop-b —— 谁把自己的 DNS 指过来，就能在自己控制的 origin 上
	// 提供任意一家店的店面（钓鱼、cookie、CSP 全都跟着走）。
	//
	// 留空则彻底不做子域名匹配，只认 shop_settings.domain 登记过的域名。
	// 留空且有活跃商家没登记域名时，Preflight 会拒绝启动 —— 那些店谁也访问不到。
	BaseDomain string

	// CacheTTL 为零时用 DefaultCacheTTL。
	CacheTTL time.Duration

	// Now 为 nil 时用 time.Now。可注入是为了让 TTL 有个不必真的等待的测试 ——
	// 用 sleep 去测 TTL 的话，要么测试慢，要么 TTL 短到没法反映真实配置。
	Now func() time.Time

	// Log 为 nil 时用 slog.Default。
	Log *slog.Logger
}

// Resolver 把请求映射到商家。它是租户进入 context 的唯一入口，
// 后面的 handler / service / repository 都只从 ctx 里取。
//
// 租户来源只有两种，都不可由客户端伪造：
//   - 配置的默认商家（单商家部署，docker compose 的常规形态）
//   - Host 头（多商家部署）
//
// 刻意不支持用请求头指定租户。公开接口没有鉴权，
// 那等于让调用方自己声明它是哪家店。本地开发的便利由默认商家提供。
type Resolver struct {
	pool       *pgxpool.Pool
	cfg        Config
	baseDomain string // 归一化后的 cfg.BaseDomain

	mu    sync.RWMutex
	cache map[string]entry
}

type entry struct {
	id  int64
	exp time.Time
}

// NewResolver 建解析器。
//
// pool 请用 db.NewPool 建：它把「这条连接能不能绕过 RLS」的自检挂在每条物理
// 连接上。这里查的 merchants/shop_settings 没有 RLS，但同一个池会被 repository
// 拿去查有 RLS 的表。
func NewResolver(pool *pgxpool.Pool, cfg Config) *Resolver {
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = DefaultCacheTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Resolver{
		pool:       pool,
		cfg:        cfg,
		baseDomain: normalizeHost(cfg.BaseDomain),
		cache:      map[string]entry{},
	}
}

// dnsLabel 是 code 想出现在 `{code}.{BaseDomain}` 里必须满足的写法。
// 小写、数字、连字符，不以连字符开头或结尾，最长 63 —— 就是 DNS 标签的规矩。
// Host 在解析前会被统一成小写，所以 code 里但凡有大写字母就永远匹配不上。
const dnsLabel = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`

// Preflight 在启动时检查部署配置与库里的数据对不对得上，对不上就拒绝启动。
//
// 手法和 db.Guard 拒绝超级用户角色是同一个：**误配要在启动时响一次，
// 而不是每个请求静默地错一点点**。这几条如果留到运行期，症状分别是
// 「客人看到了别家的店」「全站 404」「某几家店谁也打不开」，
// 三种都不指向「配置写错了」这个真因。
//
// main 里该这么用：解析器建好之后、开始监听之前调一次，返回错误就退出。
func (r *Resolver) Preflight(ctx context.Context) error {
	// 纯配置的检查排在最前面：它不查库，而且两个变量互斥这件事比库里有什么
	// 更基础。排在后面的话，这种误配会被别的检查先报出来，指向错的方向。
	if r.cfg.DefaultCode != "" && r.baseDomain != "" {
		return fmt.Errorf(
			"KEEL_DEFAULT_MERCHANT（%q）与 KEEL_BASE_DOMAIN（%q）不能同时配置："+
				"配了默认商家就是单商家部署，Host 完全不参与解析，基础域名会被静默忽略。"+
				"多商家部署请清空 KEEL_DEFAULT_MERCHANT",
			r.cfg.DefaultCode, r.baseDomain)
	}

	if r.cfg.DefaultCode != "" {
		// 单商家模式忽略 Host，所以库里必须真的只有一家店。
		//
		// 典型误配：单商家起步，后来加了第二家商家，却忘了把
		// KEEL_DEFAULT_MERCHANT 取消。此时每个请求——包括本该属于新商家的
		// 那些——都会落到默认店上。没有这道检查，它不报错、不报警，
		// 只是所有人都看到同一家店的数据。
		var n int64
		if err := r.pool.QueryRow(ctx,
			`SELECT count(*) FROM merchants WHERE deleted_at IS NULL AND status = 1`).
			Scan(&n); err != nil {
			return fmt.Errorf("统计活跃商家失败: %w", err)
		}
		if n > 1 {
			return fmt.Errorf(
				"配置了默认商家 %q（KEEL_DEFAULT_MERCHANT），但库里有 %d 家活跃商家。"+
					"单商家模式忽略 Host，这会把所有商家的流量都送到默认店去。"+
					"多商家部署请清空 KEEL_DEFAULT_MERCHANT 并设置 KEEL_BASE_DOMAIN",
				r.cfg.DefaultCode, n)
		}
		if _, err := r.byCode(ctx, r.cfg.DefaultCode); err != nil {
			if errors.Is(err, ErrNoMerchant) {
				return fmt.Errorf(
					"默认商家 %q（KEEL_DEFAULT_MERCHANT）不存在、已停用或已删除，"+
						"这套部署没有任何可服务的商家", r.cfg.DefaultCode)
			}
			return fmt.Errorf("查默认商家 %q 失败: %w", r.cfg.DefaultCode, err)
		}
		return nil
	}

	// 多商家部署：把「谁也访问不到」的商家找出来。
	//
	// 一家活跃商家的入口只有两个：登记过的域名，或者 `{code}.{BaseDomain}`。
	// 两个都没有，它就是一家开着门却没有地址的店 —— 症状是「某几家店全站 404」，
	// 而真因（少配了 KEEL_BASE_DOMAIN，或者 code 写成了 Shop_UPPER）
	// 在 404 里看不出一丝痕迹。
	n, sample, err := r.unreachable(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		if r.baseDomain == "" {
			return fmt.Errorf(
				"没有配置 KEEL_BASE_DOMAIN，子域名解析被关闭，而有 %d 家活跃商家"+
					"（%s）没有登记 shop_settings.domain —— 它们没有任何可访问的入口。"+
					"请设置 KEEL_BASE_DOMAIN，或给这些商家登记域名", n, sample)
		}
		return fmt.Errorf(
			"有 %d 家活跃商家（%s）的 code 不是合法的 DNS 标签"+
				"（小写字母、数字、连字符，不以连字符开头结尾，最长 63），"+
				"没法出现在 {code}.%s 里，而它们也没有登记 shop_settings.domain —— "+
				"它们没有任何可访问的入口。请改 code，或给它们登记域名",
			n, sample, r.baseDomain)
	}
	return nil
}

// unreachable 数出没有任何入口的活跃商家，并列出其中前几个的 code。
//
// 列前几个而不是只列一个：只报一家的话，有 N 家不可达时运维要「改一家、
// 重启、再看下一家」，把一次修复拖成 N 轮。
func (r *Resolver) unreachable(ctx context.Context) (int64, string, error) {
	const q = `
	WITH bad AS (
	    SELECT m.code
	      FROM merchants m
	      LEFT JOIN shop_settings s ON s.merchant_id = m.id
	     WHERE m.deleted_at IS NULL
	       AND m.status = 1
	       AND s.domain IS NULL
	       AND ($1 OR m.code !~ $2)
	     ORDER BY m.code
	)
	SELECT (SELECT count(*) FROM bad),
	       coalesce((SELECT string_agg(code, ', ') FROM (SELECT code FROM bad LIMIT 5) t), '')`
	var n int64
	var sample string
	if err := r.pool.QueryRow(ctx, q, r.baseDomain == "", dnsLabel).Scan(&n, &sample); err != nil {
		return 0, "", fmt.Errorf("检查商家可达性失败: %w", err)
	}
	if n > 5 {
		sample += " …"
	}
	return n, sample, nil
}

// Middleware 解析租户并放进 request context。解析不到就中止请求。
func (r *Resolver) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		host := c.Request.Host
		id, err := r.Resolve(c.Request.Context(), host)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoMerchant):
			// 这家店不存在（或已停用）。404，而不是回落到某个商家。
			//
			// 记一条日志：裸 404 对运维是不可观测的。「有人在扫子域名」和
			// 「某家店的 DNS 配错了 / 域名忘了登记」在客户端看来是同一个 404，
			// 只有这条日志能把两者分开。
			r.cfg.Log.WarnContext(c.Request.Context(), "租户解析失败，返回 404",
				"host", host, "path", c.Request.URL.Path,
				"default_merchant", r.cfg.DefaultCode, "base_domain", r.baseDomain)
			c.AbortWithStatus(http.StatusNotFound)
			return
		default:
			// 查库失败是服务端故障。把它也报成 404 的话，一次数据库抖动会表现为
			// 「所有店铺集体下架」，而监控上看不到任何 5xx。
			r.cfg.Log.ErrorContext(c.Request.Context(), "租户解析查库失败",
				"host", host, "path", c.Request.URL.Path, "err", err)
			_ = c.Error(err)
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		c.Request = c.Request.WithContext(NewContext(c.Request.Context(), id))
		c.Next()
	}
}

// Resolve 由配置与 Host 定出商家 ID。
//
// 规则（两种部署形态的差别全在这里）：
//
//	配了默认商家（单商家部署）
//	    Host 完全不参与解析，一律是默认商家。
//	    「库里其实不止一家店」这种误配由 Preflight 在启动时挡掉，不在这里猜。
//	没配默认商家（多商家部署）
//	    Host 落在 BaseDomain 下   → 取第一段当 code 匹配，**只认 code**
//	    Host 不落在 BaseDomain 下 → 只认 shop_settings.domain 登记过的完整域名
//	    两条都对不上就 404
//
// 「基础域名下只认 code」是一条隔离要求，不是优化：
// shop_settings.domain 是商家自己填的。允许它在基础域名下生效的话，商家 C 把
// domain 填成 `shop-b.example.com`（B 自己不需要登记，子域名是天然的），
// 就在 B 的规范 URL 上开了自己的店。基础域名下的名字归平台，不归商家。
func (r *Resolver) Resolve(ctx context.Context, host string) (int64, error) {
	if r.cfg.DefaultCode != "" {
		return r.byCode(ctx, r.cfg.DefaultCode)
	}

	name := normalizeHost(host)
	if name == "" {
		return 0, ErrNoMerchant
	}
	if label, under := underBaseDomain(name, r.baseDomain); under {
		if label == "" || strings.Contains(label, ".") {
			// 基础域名本身（apex），或它下面多于一级的名字。
			// 这些名字归平台，不归任何商家，所以不查任何表，直接 404。
			return 0, ErrNoMerchant
		}
		return r.byCode(ctx, label)
	}
	return r.byDomain(ctx, name)
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

// underBaseDomain 判断 name 是不是落在 base 这片地盘里（含 base 本身），
// 是就返回 base 之前的那一段（apex 时是空串，多级时含点）。
//
// 判据是「在不在基础域名下」，**不是**「是不是恰好一级子域名」。这个区别就是
// 隔离本身：只认恰好一级的话，`example.com`（apex）和 `a.b.example.com`
// （多级）都会掉进 byDomain 那一支去读商家自己填的 domain —— 于是商家登记一个
// `example.com` 就拿到了平台主站，登记一个 `admin.internal.example.com` 就占住了
// 平台将来要用的后台域名。基础域名下的每一个名字都归平台。
//
// base 为空时永远返回 false —— 子域名匹配整个关掉，
// 而不是退化成「任何域名的第一段都算」。
func underBaseDomain(name, base string) (string, bool) {
	if base == "" || name == "" {
		return "", false
	}
	if name == base {
		return "", true
	}
	if suffix := "." + base; strings.HasSuffix(name, suffix) {
		return strings.TrimSuffix(name, suffix), true
	}
	return "", false
}

// byCode 按 merchants.code 找商家。
func (r *Resolver) byCode(ctx context.Context, code string) (int64, error) {
	// status = 1 才算可访问：停用的商家不该还能被逛。
	// deleted_at IS NULL 同理：软删的商家在库里还在，但它不该还能接客。
	const q = `
	SELECT m.id
	  FROM merchants m
	 WHERE m.deleted_at IS NULL
	   AND m.status = 1
	   AND m.code = $1`
	return r.lookup(ctx, "code:"+code, q, code)
}

// byDomain 按 shop_settings.domain 找商家（自定义域名形态）。
func (r *Resolver) byDomain(ctx context.Context, name string) (int64, error) {
	const q = `
	SELECT m.id
	  FROM merchants m
	  JOIN shop_settings s ON s.merchant_id = m.id
	 WHERE m.deleted_at IS NULL
	   AND m.status = 1
	   AND s.domain = $1`
	return r.lookup(ctx, "domain:"+name, q, name)
}

// lookup 查一次并缓存命中结果。
//
// 只缓存命中：把「查不到」也缓存起来的话，一家店建好后的头 TTL 秒会继续 404，
// 而那正是有人盯着页面刷新的时刻。查不到的键每次都要查库，代价是一次索引命中。
func (r *Resolver) lookup(ctx context.Context, key, q string, args ...any) (int64, error) {
	now := r.cfg.Now()

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
	r.cache[key] = entry{id: id, exp: now.Add(r.cfg.CacheTTL)}
	r.mu.Unlock()
	return id, nil
}
