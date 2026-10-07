package tenant_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/testdb"
)

// baseDomain 是这些测试里的平台基础域名，与种子里的域名一致。
const baseDomain = "example.com"

// single / multi 是两种部署形态的配置。测试里每次都写全，是为了让每个用例
// 自己说清它在测哪种形态 —— 这两种形态的解析规则完全不同。
func single(code string) tenant.Config { return tenant.Config{DefaultCode: code} }
func multi() tenant.Config             { return tenant.Config{BaseDomain: baseDomain} }

// TestMain 加载 db/seed/dev.sql。
//
// 不走 `psql -f`：宿主机上不一定有 psql（这个仓库的开发形态是数据库跑在容器里，
// 客户端二进制并不随之出现在 PATH 上），而 exec 一个不存在的命令，报出来的是
// "executable file not found"，看上去像环境坏了，而不是「种子没加载」。
// 用 Go 读文件再 Exec，依赖面只剩下已经被测试依赖的 pgx。
//
// 用管理员连接：种子要往带 RLS 的表里写（merchants / shop_settings 没有 RLS，
// merchant_domains 有但读侧放开，而 categories/products 随时会加进来），而 keel_app
// 在没有租户上下文的连接上一行都插不进去。这是少数几个正当使用 AdminDSN 的地方之一。
//
// 库是本包自己的（keel_test_tenant），由 testdb.Main 新建并迁移。
// **以前这里不迁移**，只加载种子：它靠的是 `go test -p 1` 按字母序先跑完的
// internal/service 留下的 schema —— 在冷库上单独跑这个包、或者换一个包序，
// 种子会因为表不存在而 panic。每个包自己迁移之后，这层隐含的先后依赖没了。
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, testdb.Package{Name: "tenant", Setup: loadSeed}))
}

func loadSeed(ctx context.Context) error {
	sql, err := os.ReadFile(filepath.Join("..", "..", "db", "seed", "dev.sql"))
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// 无参数的 Exec 走简单查询协议，一次可以发多条语句。
	_, err = conn.Exec(ctx, string(sql))
	return err
}

// ---------- 夹具 ----------

// newPool 建池。用 db.NewPool 而不是 pgxpool.New：后者不检查角色，于是
// 「测试跑在一条能绕过 RLS 的连接上」这个洞会原样回来。
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := db.NewPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testWriter 把解析器的日志转给 t.Log：默认丢弃会让失败用例少掉最有用的线索，
// 直接打到 stderr 又会污染其它用例的输出。
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("resolver: %s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}

// newRouter 造一个挂了解析中间件的路由，/probe 回显当前租户。
func newRouter(t *testing.T, cfg tenant.Config) *gin.Engine {
	t.Helper()
	return newRouterWithPool(t, newPool(t), cfg)
}

func newRouterWithPool(t *testing.T, pool *pgxpool.Pool, cfg tenant.Config) *gin.Engine {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(tenant.NewResolver(pool, cfg).Middleware())
	r.GET("/probe", func(c *gin.Context) {
		id, err := tenant.FromContext(c.Request.Context())
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.JSON(http.StatusOK, gin.H{"merchant_id": id})
	})
	return r
}

// do 发一个请求。header 是可选的额外请求头，用来证明它们影响不了解析结果。
func do(r *gin.Engine, host string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Host = host
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// resolved 断言请求解析成功，并返回解析到的商家 ID。
func resolved(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实得 %d：%s", w.Code, w.Body.String())
	}
	var body struct {
		MerchantID int64 `json:"merchant_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v（%s）", err, w.Body.String())
	}
	if body.MerchantID <= 0 {
		t.Fatalf("返回了非法的 merchant_id %d", body.MerchantID)
	}
	return body.MerchantID
}

// merchantID 直接查库拿种子商家的 ID，用来断言「解析到的是哪一家」。
// 只断言状态码不够：解析到错误的商家同样是 200。
func merchantID(t *testing.T, code string) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var id int64
	if err := conn.QueryRow(ctx, `SELECT id FROM merchants WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("种子商家 %q 不存在: %v", code, err)
	}
	return id
}

// adminExec 用管理员连接改库，供需要「运行期变更商家」的用例使用。
func adminExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// setStatus 改一家商家的状态，测试结束还原。
func setStatus(t *testing.T, code string, status int16) {
	t.Helper()
	var old int16
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx,
		`UPDATE merchants SET status = $2 WHERE code = $1 RETURNING (SELECT status FROM merchants WHERE code = $1)`,
		code, status).Scan(&old); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)
	t.Cleanup(func() { adminExec(t, `UPDATE merchants SET status = $2 WHERE code = $1`, code, old) })
}

// onlyActive 把除 code 之外的活跃商家全部停用，测试结束还原。
// 用来造出「库里真的只有一家店」那种单商家部署。
func onlyActive(t *testing.T, code string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx,
		`UPDATE merchants SET status = 2
		  WHERE deleted_at IS NULL AND status = 1 AND code <> $1
		  RETURNING code`, code)
	if err != nil {
		t.Fatal(err)
	}
	var restored []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		restored = append(restored, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adminExec(t, `UPDATE merchants SET status = 1 WHERE code = ANY($1)`, restored)
	})
}

// newMerchant 建一家临时商家，测试结束删掉。domain 为空则不登记域名。
func newMerchant(t *testing.T, code, domain string) {
	t.Helper()
	adminExec(t, `INSERT INTO merchants (code, name, status) VALUES ($1, $1 || ' 的店', 1)`, code)
	t.Cleanup(func() { adminExec(t, `DELETE FROM merchants WHERE code = $1`, code) })
	if domain != "" {
		adminExec(t, `INSERT INTO merchant_domains (merchant_id, domain)
			SELECT id, $2 FROM merchants WHERE code = $1`, code, domain)
		t.Cleanup(func() {
			adminExec(t, `DELETE FROM merchant_domains WHERE merchant_id =
				(SELECT id FROM merchants WHERE code = $1)`, code)
		})
	}
}

// claimDomain 让某家商家把 domain 登记成 name，测试结束还原。
// 用来演一遍「商家自己填的域名想抢平台的名字」。
func claimDomain(t *testing.T, code, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// 先把原值读出来照原样还原。写成「还原成 code + 基础域名」会静默改掉
	// shop-c 的 custom.example.net，让后面的自定义域名用例测的是另一回事。
	// 没有登记过的店要读成 NULL，所以 LEFT JOIN：这一行不存在是常态。
	var old *string
	if err := conn.QueryRow(ctx, `SELECT d.domain FROM merchants m
		LEFT JOIN merchant_domains d ON d.merchant_id = m.id WHERE m.code = $1`, code).Scan(&old); err != nil {
		t.Fatalf("读 %s 的原域名失败: %v", code, err)
	}
	setDomain(t, code, &name)
	t.Cleanup(func() { setDomain(t, code, old) })
}

// setDomain 把 code 这家店的登记改成 domain（nil = 摘掉）。
// 表上没有那一行时要补一行 —— 登记路径本来就是「没有行 = 没登记」。
// 用 adminExec 而不是调用方那个连接：t.Cleanup 跑在测试函数返回**之后**，
// 那时函数里 defer 掉的控制台连接已经关了。
func setDomain(t *testing.T, code string, domain *string) {
	t.Helper()
	adminExec(t, `DELETE FROM merchant_domains WHERE merchant_id =
		(SELECT id FROM merchants WHERE code = $1)`, code)
	if domain == nil {
		return
	}
	adminExec(t, `INSERT INTO merchant_domains (merchant_id, domain)
		SELECT id, $2 FROM merchants WHERE code = $1`, code, *domain)
}

// fakeClock 让 TTL 不必真的等待。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---------- 单商家部署 ----------

// 单商家部署：配了默认商家，请求解析到它。
func TestDefaultMerchantResolves(t *testing.T) {
	got := resolved(t, do(newRouter(t, single("shop-a")), "localhost:8080"))
	if want := merchantID(t, "shop-a"); got != want {
		t.Fatalf("期望解析到 shop-a(%d)，实得 %d", want, got)
	}
}

// 单商家部署忽略 Host —— 任何 Host 都解析到默认商家。
//
// 这是 Ruling 22：把「哪个租户」和「这个 Host 允不允许」拆开。
// 「库里其实不止一家店，却还配着默认商家」那种误配，由 Preflight 在启动时
// 一次性拒绝（见 TestPreflightRejectsDefaultWithMultipleActiveMerchants），
// 而不是靠每个请求去猜 Host 像不像在指名某家店 —— 后者会把 k8s 的
// svc.cluster.local、PaaS 生成域名、蓝绿预发域名全部误伤成裸 404。
func TestDefaultDeploymentIgnoresHost(t *testing.T) {
	want := merchantID(t, "shop-a")
	r := newRouter(t, single("shop-a"))
	for _, host := range []string{
		"localhost:8080",
		"keel.default.svc.cluster.local",
		"10.0.0.7:8080",
		"shop-a.example.com",
		"nope.example.com",
		"custom.example.net",
		"shop-b.example.com", // 库里只有一家店时它不存在；有第二家时 Preflight 会拒绝启动
	} {
		if got := resolved(t, do(r, host)); got != want {
			t.Fatalf("Host %q：单商家部署期望恒定解析到 shop-a(%d)，实得 %d", host, want, got)
		}
	}

	// 同时配了基础域名时也一样。没有这一段的话，「单商家模式偷偷又去匹配
	// 基础域名下的子域名」这个退化通不过任何断言 —— 上面那组 Host 在
	// BaseDomain 为空时本来就走不到子域名那一支。
	//
	// 这个组合本身是会被 Preflight 拒绝的（见 TestPreflightRejectsBothTenantSourcesConfigured）。
	// 这里仍然断言它的解析行为，是纵深防御：万一有人绕过启动自检把它跑起来，
	// Host 也不该改变解析结果。
	both := single("shop-a")
	both.BaseDomain = baseDomain
	rBoth := newRouter(t, both)
	for _, host := range []string{"shop-b.example.com", "nope.example.com"} {
		if got := resolved(t, do(rBoth, host)); got != want {
			t.Fatalf("Host %q：同时配了基础域名的单商家部署仍应解析到 shop-a(%d)，实得 %d",
				host, want, got)
		}
	}
}

// 默认商家停用后不可访问。
func TestDisabledDefaultMerchantIs404(t *testing.T) {
	if got := do(newRouter(t, single("shop-closed")), "localhost:8080").Code; got != 404 {
		t.Fatalf("停用的默认商家期望 404，实得 %d", got)
	}
}

// 默认商家被软删后同样不可访问。
func TestSoftDeletedDefaultMerchantIs404(t *testing.T) {
	if got := do(newRouter(t, single("shop-deleted")), "localhost:8080").Code; got != 404 {
		t.Fatalf("软删的默认商家期望 404，实得 %d", got)
	}
}

// ---------- 多商家部署 ----------

// 子域名匹配 merchants.code。
func TestSubdomainResolves(t *testing.T) {
	got := resolved(t, do(newRouter(t, multi()), "shop-b.example.com"))
	if want := merchantID(t, "shop-b"); got != want {
		t.Fatalf("期望解析到 shop-b(%d)，实得 %d", want, got)
	}
}

// 自定义域名：域名落在基础域名之外，只能靠 merchant_domains 匹配。
//
// shop-c 的域名是 custom.example.net，它的第一段 "custom" 不是任何商家的 code。
// 拿子域名去比完整域名的实现会在这里 404。
func TestCustomDomainResolves(t *testing.T) {
	got := resolved(t, do(newRouter(t, multi()), "custom.example.net"))
	if want := merchantID(t, "shop-c"); got != want {
		t.Fatalf("期望解析到 shop-c(%d)，实得 %d", want, got)
	}
}

// Host 大小写不敏感，端口与结尾的点都不参与匹配。
func TestHostNormalization(t *testing.T) {
	want := merchantID(t, "shop-b")
	r := newRouter(t, multi())
	for _, host := range []string{
		"SHOP-B.Example.COM",
		"shop-b.example.com:8443",
		"shop-b.example.com.",
	} {
		if got := resolved(t, do(r, host)); got != want {
			t.Fatalf("Host %q 期望解析到 shop-b(%d)，实得 %d", host, want, got)
		}
	}
}

// 未知子域名必须 404，不能回落到任何商家。
// 回落等于任何人拼一个不存在的子域名就能看到某家店的数据。
func TestUnknownSubdomainIs404NotFallback(t *testing.T) {
	if got := do(newRouter(t, multi()), "nope.example.com").Code; got != 404 {
		t.Fatalf("未知子域名期望 404，实得 %d —— 是否回落到了某个商家？", got)
	}
}

// 子域名匹配必须锚定平台基础域名。
//
// 不锚定的话，攻击者把自己控制的 shop-b.attacker.example.org 指过来，就能在
// **自己的 origin** 上提供 shop-b 的店面：钓鱼页面、cookie 作用域、CSP、
// 支付回跳的白名单全部跟着那个域名走。
func TestSubdomainMatchingIsAnchoredToBaseDomain(t *testing.T) {
	r := newRouter(t, multi())
	for _, host := range []string{
		"shop-b.attacker.example.org", // 别人的域名，第一段撞上了 code
		"shop-b.example.com.evil.org", // 基础域名只是中间的一段
		"shop-b.notexample.com",       // 后缀差一点
		"a.shop-b.example.com",        // 更深的层级，「哪一段是店名」没有唯一答案
		"example.com",                 // 基础域名本身不属于任何商家
	} {
		if got := do(r, host).Code; got != 404 {
			t.Fatalf("Host %q 期望 404，实得 %d —— 子域名匹配没有锚定 %q？", host, got, baseDomain)
		}
	}
}

// 基础域名的 apex 不归任何商家 —— 哪怕有商家把它登记成了自己的 domain。
//
// 这条和「基础域名下只认 code」是同一条规则，只是往上挪了一层：
// 判据要是「是不是恰好一级子域名」，apex 就不算，于是掉进读 merchant_domains
// 那一支，商家登记一个 example.com 就拿到了平台主站。
//
// 注意：光断言 example.com → 404 是不够的（TestSubdomainMatchingIsAnchoredToBaseDomain
// 里就有那么一条），没人登记它的时候那条断言恒绿。必须真的让一家商家登记它。
func TestBaseDomainApexIsNeverAMerchant(t *testing.T) {
	claimDomain(t, "shop-c", baseDomain)
	// 再放一家 code 为空串的商家：apex 的「标签」就是空串，
	// 少了 label == "" 那道判断的实现会拿它去 byCode("") 并匹配上。
	// merchants.code 是裸 TEXT NOT NULL，'' 插得进去。
	newMerchant(t, "", "")

	if got := do(newRouter(t, multi()), baseDomain).Code; got != 404 {
		t.Fatalf("有商家登记了基础域名 apex，期望仍然 404，实得 %d —— 平台主站被商家拿走了", got)
	}
}

// 基础域名下多于一级的名字同样归平台。
//
// 判据是「在不在基础域名下」而不是「是不是恰好一级」，否则平台将来在
// example.com 下放任何两级以上的东西（api.v2、admin.internal），
// 商家都能抢先登记占走。
func TestMultiLevelNamesUnderBaseDomainAreNeverAMerchant(t *testing.T) {
	const name = "admin.internal.example.com"
	claimDomain(t, "shop-c", name)
	// 关键的一家：code 里带点。merchants.code 是裸 TEXT，没有 DNS 标签约束
	// （那正是 Preflight 检查 code 写法的前提），所以 'admin.internal' 这种 code
	// 建得出来。没有它的话，去掉「label 含点就拒绝」那道判断的实现会让
	// byCode("admin.internal") 恒查不到，本用例照样绿 —— 一个只证明了
	// 「多级名字没掉进 byDomain」、没证明「多级名字谁也拿不到」的空断言。
	newMerchant(t, "admin.internal", "")

	if got := do(newRouter(t, multi()), name).Code; got != 404 {
		t.Fatalf("有商家登记了 %q，期望仍然 404，实得 %d —— 基础域名下的多级名字被商家占走了",
			name, got)
	}
}

// 基础域名下 code 匹配必须赢过商家自己登记的 domain。
//
// merchant_domains.domain 是商家自己填的。允许它在基础域名下生效的话，商家 C 把
// domain 填成 `shop-nodomain.example.com`，就在那家店的规范 URL 上开了自己的店 ——
// 而那家店根本不需要登记域名，子域名是天然的，于是它连「域名被占了」都察觉不到。
func TestCodeWinsOverMerchantSuppliedDomainUnderBaseDomain(t *testing.T) {
	const hijacked = "shop-nodomain.example.com"
	claimDomain(t, "shop-c", hijacked)

	got := resolved(t, do(newRouter(t, multi()), hijacked))
	if want := merchantID(t, "shop-nodomain"); got != want {
		t.Fatalf("Host %q 解析到了 %d，期望 shop-nodomain(%d) —— "+
			"商家填的 domain 抢到了基础域名下别家店的规范 URL", hijacked, got, want)
	}
}

// 停用的商家不可访问。
func TestDisabledMerchantIs404(t *testing.T) {
	if got := do(newRouter(t, multi()), "shop-closed.example.com").Code; got != 404 {
		t.Fatalf("停用商家期望 404，实得 %d", got)
	}
}

// 软删的商家不可访问。
//
// shop-deleted 的 status 仍是 1，只有 deleted_at 非空 —— 单靠 status 过滤的实现
// 会在这里放行。种子里那家店就是为这条断言存在的。
func TestSoftDeletedMerchantIs404(t *testing.T) {
	if got := do(newRouter(t, multi()), "shop-deleted.example.com").Code; got != 404 {
		t.Fatalf("软删商家期望 404，实得 %d —— 是否漏了 deleted_at IS NULL？", got)
	}
}

// 多商家部署里没有默认商家可回落，裸主机名就是 404。
func TestBareHostWithoutDefaultIs404(t *testing.T) {
	if got := do(newRouter(t, multi()), "localhost:8080").Code; got != 404 {
		t.Fatalf("无默认商家的裸主机名期望 404，实得 %d", got)
	}
}

// 没配基础域名时，子域名匹配必须整个关掉，而不是退化成「任何域名的第一段都算」。
func TestWithoutBaseDomainOnlyRegisteredDomainsResolve(t *testing.T) {
	r := newRouter(t, tenant.Config{}) // 既无默认商家，也无基础域名
	// shop-nodomain 没有 merchant_domains 行，只能靠子域名匹配被找到 ——
	// 而子域名匹配此时是关掉的。（拿 shop-b 来试没有意义：它登记过
	// shop-b.example.com，解析成功是走的 domain 那一支，证明不了任何事。）
	if got := do(r, "shop-nodomain.example.com").Code; got != 404 {
		t.Fatalf("没配基础域名时子域名不该解析，实得 %d", got)
	}
	got := resolved(t, do(r, "custom.example.net"))
	if want := merchantID(t, "shop-c"); got != want {
		t.Fatalf("登记过的域名仍应可用：期望 shop-c(%d)，实得 %d", want, got)
	}
}

// ---------- 租户不可由请求头指定 ----------

// 公开接口没有鉴权，支持用请求头挑租户等于让调用方自己声明它是哪家店。
// 这个测试钉住「我们没有偷偷加一个方便本地开发的头」。
//
// X-Forwarded-Host 在列表里是有意的：将来有人「顺手」让中间件信任它，
// 这个测试会红。要信任它必须同时约定「谁在设它、谁在剥它」，不是顺手能加的。
func TestRequestHeadersCannotChooseTheTenant(t *testing.T) {
	a := merchantID(t, "shop-a")
	headers := [][]string{
		{"X-Merchant-Id", "2"},
		{"X-Merchant-Code", "shop-b"},
		{"X-Tenant-Id", "2"},
		{"X-Forwarded-Host", "shop-b.example.com"},
		{"X-Tenant", "shop-b"},
	}
	rSingle, rMulti := newRouter(t, single("shop-a")), newRouter(t, multi())
	for _, h := range headers {
		// 单商家部署：无论头里写什么，都还是默认商家。
		if got := resolved(t, do(rSingle, "localhost:8080", h...)); got != a {
			t.Fatalf("请求头 %v 改变了解析结果：期望 shop-a(%d)，实得 %d", h, a, got)
		}
		// 多商家部署：头不能替 Host 背书，未知 Host 仍是 404。
		if got := do(rMulti, "nope.example.com", h...).Code; got != 404 {
			t.Fatalf("请求头 %v 让未知 Host 解析成功了（%d）", h, got)
		}
	}
}

// ---------- 可观测性 ----------

// 解析不到的 Host 必须留下日志。
//
// 裸 404 对运维是不可观测的：「有人在扫子域名」和「某家店的域名忘了登记」
// 在客户端看来是同一个响应，只有这条日志能把两者分开。
func TestUnresolvedHostIsLogged(t *testing.T) {
	var buf bytes.Buffer
	cfg := multi()
	cfg.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if got := do(newRouter(t, cfg), "nope.example.com").Code; got != 404 {
		t.Fatalf("期望 404，实得 %d", got)
	}
	line := buf.String()
	for _, want := range []string{"WARN", "nope.example.com", "/probe"} {
		if !strings.Contains(line, want) {
			t.Fatalf("解析失败的日志里没有 %q：%s", want, line)
		}
	}
}

// ---------- 故障与缓存 ----------

// 查库失败是 500，不是 404。
//
// 报成 404 的话，一次数据库抖动会表现为「所有店铺集体下架」，
// 而监控上看不到任何 5xx —— 值班的人会先去查 CDN 和 DNS。
func TestDatabaseFailureIs500NotNotFound(t *testing.T) {
	pool := newPool(t)
	r := newRouterWithPool(t, pool, multi())
	pool.Close() // 模拟「库没了」：后续查询直接报错，而不是返回零行

	if got := do(r, "shop-b.example.com").Code; got != http.StatusInternalServerError {
		t.Fatalf("查库失败期望 500，实得 %d —— 数据库故障被伪装成了「店不存在」", got)
	}
}

// 缓存必须过期：停用一家商家之后，它要真的访问不了。
//
// 用注入的时钟而不是 sleep：TTL 是配置项，真实值是 30s，用 sleep 去测要么
// 让测试跑 30 秒，要么把 TTL 改到小得不像真实配置。
// 时钟可注入之后，这条断言既快又测的是真正的 TTL 逻辑。
func TestCacheExpiresSoDisablingAMerchantTakesEffect(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	cfg := multi()
	cfg.CacheTTL = 30 * time.Second
	cfg.Now = clock.now
	r := newRouter(t, cfg)

	if got := resolved(t, do(r, "shop-b.example.com")); got != merchantID(t, "shop-b") {
		t.Fatalf("预热失败，解析到了 %d", got)
	}

	setStatus(t, "shop-b", 2) // 停用

	// 还在 TTL 之内：缓存仍然命中（这一条同时证明缓存确实在生效，
	// 否则下面那条断言即使把缓存整个删掉也会绿）。
	clock.advance(29 * time.Second)
	if got := do(r, "shop-b.example.com").Code; got != 200 {
		t.Fatalf("TTL 之内期望仍命中缓存（200），实得 %d", got)
	}

	// 越过 TTL：必须重新查库，看到 status = 2。
	clock.advance(2 * time.Second)
	if got := do(r, "shop-b.example.com").Code; got != 404 {
		t.Fatalf("TTL 之后期望 404，实得 %d —— 缓存永不过期的话，"+
			"停用一家商家要等到进程重启才生效", got)
	}
}

// 亚秒级 TTL 在真实时钟上同样成立 —— 证明上面那条不是只对假时钟有效。
func TestCacheTTLAlsoHoldsOnTheRealClock(t *testing.T) {
	cfg := multi()
	cfg.CacheTTL = 50 * time.Millisecond
	r := newRouter(t, cfg)

	if got := resolved(t, do(r, "shop-b.example.com")); got != merchantID(t, "shop-b") {
		t.Fatalf("预热失败，解析到了 %d", got)
	}
	setStatus(t, "shop-b", 2)
	time.Sleep(80 * time.Millisecond)
	if got := do(r, "shop-b.example.com").Code; got != 404 {
		t.Fatalf("TTL 过后期望 404，实得 %d", got)
	}
}

// ---------- 启动自检 ----------

// 配了默认商家，库里却不止一家活跃商家 —— 拒绝启动。
//
// 这是 Ruling 22 的主检查：单商家模式忽略 Host，所以「单商家起步、后来加了
// 第二家商家、却忘了取消 KEEL_DEFAULT_MERCHANT」这种误配会把所有商家的流量
// 都送到默认店去。它在运行期不报错、不报警，只是所有人都看到同一家店。
func TestPreflightRejectsDefaultWithMultipleActiveMerchants(t *testing.T) {
	r := tenant.NewResolver(newPool(t), single("shop-a"))
	err := r.Preflight(context.Background())
	if err == nil {
		t.Fatal("库里有多家活跃商家却配了默认商家，Preflight 竟然放行")
	}
	for _, want := range []string{"KEEL_DEFAULT_MERCHANT", "shop-a"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息里没有 %q，运维看不出该改什么：%v", want, err)
		}
	}
	t.Logf("Preflight 如期拒绝：%v", err)
}

// 库里真的只有一家活跃商家时，单商家部署放行。
func TestPreflightAcceptsGenuineSingleTenantDeployment(t *testing.T) {
	onlyActive(t, "shop-a")
	pool := newPool(t)

	if err := tenant.NewResolver(pool, single("shop-a")).Preflight(context.Background()); err != nil {
		t.Fatalf("库里只有 shop-a 一家活跃商家，Preflight 不该报错：%v", err)
	}
	// 同一个夹具下顺便验证另外两条：默认商家不存在 / 已停用，同样拒绝启动。
	// 否则症状是「全站 404」，而真因是配置里的 code 写错了一个字母。
	for _, code := range []string{"no-such-shop", "shop-closed"} {
		if err := tenant.NewResolver(pool, single(code)).Preflight(context.Background()); err == nil {
			t.Fatalf("默认商家 %q 不可服务，Preflight 竟然放行", code)
		}
	}
}

// 多商家部署没配基础域名，而有活跃商家没登记域名 —— 拒绝启动。
// 那些店没有任何入口，症状是「某几家店 404」，不会有人想到是少配了环境变量。
func TestPreflightRejectsMultiTenantWithoutBaseDomainWhenAShopIsUnreachable(t *testing.T) {
	r := tenant.NewResolver(newPool(t), tenant.Config{})
	err := r.Preflight(context.Background())
	if err == nil {
		t.Fatal("有商家既没有基础域名也没有登记域名，Preflight 竟然放行")
	}
	for _, want := range []string{"KEEL_BASE_DOMAIN", "shop-nodomain"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息里没有 %q：%v", want, err)
		}
	}
	t.Logf("Preflight 如期拒绝：%v", err)
}

// code 不是合法 DNS 标签的商家，在所有 Host 上都 404 —— 启动自检必须说出这件事。
//
// merchants.code 在库里是裸 TEXT，没有任何 DNS 标签约束，而 Host 在解析前统一
// 转小写，所以 code 里有大写字母或下划线的商家，`{code}.{基础域名}` 永远匹配不上。
// 没有这条检查的话，症状是「某几家店全站 404」，而真因是 code 的写法 ——
// 两者之间没有任何指引。
func TestPreflightRejectsMerchantWhoseCodeCannotAppearInAHostname(t *testing.T) {
	const bad = "Shop_UPPER"
	adminExec(t, `INSERT INTO merchants (code, name, status) VALUES ($1, '写法不合法的店', 1)`, bad)
	t.Cleanup(func() { adminExec(t, `DELETE FROM merchants WHERE code = $1`, bad) })

	// 先记录症状：这家店在任何写法的 Host 上都进不去。
	r := newRouter(t, multi())
	for _, host := range []string{"Shop_UPPER.example.com", "shop_upper.example.com"} {
		if got := do(r, host).Code; got != 404 {
			t.Fatalf("Host %q 期望 404，实得 %d", host, got)
		}
	}

	err := tenant.NewResolver(newPool(t), multi()).Preflight(context.Background())
	if err == nil {
		t.Fatal("有商家的 code 没法出现在主机名里，Preflight 竟然放行")
	}
	for _, want := range []string{bad, "DNS"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息里没有 %q，运维看不出该改什么：%v", want, err)
		}
	}
	t.Logf("Preflight 如期拒绝：%v", err)
}

// 平台保留的一级名字不发给商家：请求时不解析，启动时报错。
//
// 「基础域名下的每一个名字都归平台」这句话，光有 apex 和多级名字兑现不了 ——
// 平台真正会用到的 www / api / admin / mail 恰恰都在一级这层。
func TestReservedNamesAreNotServedToMerchants(t *testing.T) {
	for _, code := range []string{"www", "admin", "api", "mail"} {
		newMerchant(t, code, "")
	}
	r := newRouter(t, multi())
	for _, code := range []string{"www", "admin", "api", "mail"} {
		if got := do(r, code+"."+baseDomain).Code; got != 404 {
			t.Fatalf("Host %q 期望 404，实得 %d —— 平台自己要用的名字被商家拿走了",
				code+"."+baseDomain, got)
		}
	}

	err := tenant.NewResolver(newPool(t), multi()).Preflight(context.Background())
	if err == nil {
		t.Fatal("有商家占用了保留名字，Preflight 竟然放行")
	}
	for _, want := range []string{"www", "保留"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息里没有 %q：%v", want, err)
		}
	}
	t.Logf("Preflight 如期拒绝：%v", err)
}

// 登记在基础域名下的 domain 等于没有登记 —— 自检的判据必须和解析行为一致。
//
// 这家店的 code 不是合法 DNS 标签（进不了子域名那条路），domain 又登记在基础
// 域名下（那片地盘只认 code，永远不会采纳它）。于是它全站 404，
// 而「有登记的 domain 就算可达」的自检会放行 —— 正是自检要消灭的那种症状。
func TestPreflightCountsABaseDomainRegistrationAsNoEntrance(t *testing.T) {
	newMerchant(t, "Bad_Hole", "bad-hole."+baseDomain)

	r := newRouter(t, multi())
	for _, host := range []string{"bad-hole." + baseDomain, "Bad_Hole." + baseDomain} {
		if got := do(r, host).Code; got != 404 {
			t.Fatalf("Host %q 期望 404，实得 %d", host, got)
		}
	}

	err := tenant.NewResolver(newPool(t), multi()).Preflight(context.Background())
	if err == nil {
		t.Fatal("这家店没有任何入口，Preflight 竟然放行 —— " +
			"判据仍然停在「有登记就算可达」？")
	}
	if !strings.Contains(err.Error(), "Bad_Hole") {
		t.Fatalf("错误信息里没有那家店：%v", err)
	}
	t.Logf("Preflight 如期拒绝：%v", err)
}

// 基础域名本身写错了形状 —— 拒绝启动。
//
// ".example.com" 这种手滑（想表达「通配」时很自然）一个字符做三件事：
// 子域名匹配整个失效、商家登记的域名重新能抢别家规范 URL、
// 而自检因为 baseDomain 非空、以为入口天然存在而一声不吭。
func TestPreflightRejectsMalformedBaseDomain(t *testing.T) {
	// "example.com." 不在这个名单里：结尾的点是 FQDN 的合法写法，
	// normalizeHost 会把它去掉，配成那样没有任何副作用。
	for _, bad := range []string{".example.com", "net", "exa mple.com", "-example.com", "EXAMPLE..com"} {
		err := tenant.NewResolver(newPool(t), tenant.Config{BaseDomain: bad}).
			Preflight(context.Background())
		if err == nil {
			t.Fatalf("KEEL_BASE_DOMAIN=%q 形状不合法，Preflight 竟然放行", bad)
		}
		if !strings.Contains(err.Error(), "KEEL_BASE_DOMAIN") {
			t.Fatalf("错误信息里没有变量名：%v", err)
		}
	}
	// 前导点是最容易手滑的一种，错误信息要直接点出来怎么改。
	err := tenant.NewResolver(newPool(t), tenant.Config{BaseDomain: ".example.com"}).
		Preflight(context.Background())
	if !strings.Contains(err.Error(), "去掉开头的点") {
		t.Fatalf("前导点的错误信息没给出改法：%v", err)
	}
	t.Logf("Preflight 如期拒绝：%v", err)
}

// 不可达的商家不止一家时，要一次报多家。
// 只报一家的话，运维得「改一家、重启、再看下一家」，一次修复被拖成 N 轮。
func TestPreflightListsSeveralUnreachableMerchants(t *testing.T) {
	for _, bad := range []string{"Bad_One", "Bad_Two"} {
		adminExec(t, `INSERT INTO merchants (code, name, status) VALUES ($1, '写法不合法的店', 1)`, bad)
		t.Cleanup(func() { adminExec(t, `DELETE FROM merchants WHERE code = $1`, bad) })
	}
	err := tenant.NewResolver(newPool(t), multi()).Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight 竟然放行")
	}
	for _, want := range []string{"Bad_One", "Bad_Two"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息里只报了一部分，%q 不在里面：%v", want, err)
		}
	}
}

// 两个租户来源同时配置 —— 拒绝启动。
//
// 单商家模式忽略 Host，于是 KEEL_BASE_DOMAIN 被静默忽略：运维以为配的是多商家，
// 实际全站只有一家店。这种误配不该靠读代码才能发现。
func TestPreflightRejectsBothTenantSourcesConfigured(t *testing.T) {
	// 库里只留一家活跃商家：否则「活跃商家 > 1」那条检查会先报错，而它的
	// 修复建议里同样出现 KEEL_DEFAULT_MERCHANT 和 KEEL_BASE_DOMAIN 两个词 ——
	// 断言写成「错误里有这两个词」就会在这条检查被整个删掉时照样绿。
	onlyActive(t, "shop-a")

	cfg := single("shop-a")
	cfg.BaseDomain = baseDomain
	err := tenant.NewResolver(newPool(t), cfg).Preflight(context.Background())
	if err == nil {
		t.Fatal("同时配了默认商家和基础域名，Preflight 竟然放行")
	}
	for _, want := range []string{"KEEL_DEFAULT_MERCHANT", "KEEL_BASE_DOMAIN", "不能同时配置"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息里没有 %q：%v", want, err)
		}
	}
	t.Logf("Preflight 如期拒绝：%v", err)
}

// 多商家部署配了基础域名，放行。
func TestPreflightAcceptsMultiTenantWithBaseDomain(t *testing.T) {
	if err := tenant.NewResolver(newPool(t), multi()).Preflight(context.Background()); err != nil {
		t.Fatalf("多商家部署配了基础域名，Preflight 不该报错：%v", err)
	}
}

// ---------- 种子 ----------

// seededCodes 返回库里那些 code 出现在种子文件里的商家。
//
// 反过来做（解析 SQL 抽 code）要写一个小小的 SQL 解析器，而且种子的写法一改
// 就失灵。拿库里的 code 去种子文本里找，只依赖「code 在文件里带引号出现过」
// 这一条，种子怎么写都成立。
func seededCodes(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	seed, err := os.ReadFile(filepath.Join("..", "..", "db", "seed", "dev.sql"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(context.Background(), `SELECT code FROM merchants`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatal(err)
		}
		if code != "" && strings.Contains(string(seed), "'"+code+"'") {
			codes = append(codes, code)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return codes
}

// 种子必须幂等：测试每跑一次就加载一次，而数据库是跨运行保留的。
// 不幂等的种子会让「每个租户 N 件商品」这类断言随运行次数漂移 ——
// 第一次绿，之后红；断言写成 >= 时则永远绿，比红更糟。
func TestSeedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// 只数种子自己播的那几家。数全表的话，并行跑的别的包在两次计数之间
	// 插入或删掉它们的夹具商家，就会表现成「种子不幂等」——
	// 一个指向错误方向的、偶发的红。
	//
	// 名单从种子文件里现取，不写死：写死的话它要靠人和 db/seed/dev.sql
	// 手工保持同步，而不同步的后果是「新加的种子行不被这条断言覆盖」——
	// 一个不会报错、只会悄悄少测一块的退化。
	seeded := seededCodes(t, conn)
	if len(seeded) < 6 {
		t.Fatalf("从种子文件里只认出 %d 个 code（%v），种子的写法是不是变了？", len(seeded), seeded)
	}
	count := func() (int64, int64, int64) {
		t.Helper()
		var merchants, settings, domains int64
		if err := conn.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM merchants WHERE code = ANY($1)),
			       (SELECT count(*) FROM shop_settings s
			          JOIN merchants m ON m.id = s.merchant_id WHERE m.code = ANY($1)),
			       (SELECT count(*) FROM merchant_domains d
			          JOIN merchants m ON m.id = d.merchant_id WHERE m.code = ANY($1))`,
			seeded).Scan(&merchants, &settings, &domains); err != nil {
			t.Fatal(err)
		}
		return merchants, settings, domains
	}

	m1, s1, d1 := count()
	if err := loadSeed(context.Background()); err != nil {
		t.Fatal(err)
	}
	m2, s2, d2 := count()
	if m1 != m2 || s1 != s2 || d1 != d2 {
		t.Fatalf("重复加载种子改变了行数：merchants %d→%d，shop_settings %d→%d，merchant_domains %d→%d",
			m1, m2, s1, s2, d1, d2)
	}
}
