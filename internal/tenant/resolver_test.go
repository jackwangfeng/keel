package tenant_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/tenant"
)

// TestMain 加载 db/seed/dev.sql。
//
// 不走 `psql -f`：宿主机上不一定有 psql（这个仓库的开发形态是数据库跑在容器里，
// 客户端二进制并不随之出现在 PATH 上），而 exec 一个不存在的命令，报出来的是
// "executable file not found"，看上去像环境坏了，而不是「种子没加载」。
// 用 Go 读文件再 Exec，依赖面只剩下已经被测试依赖的 pgx。
//
// 用管理员连接：种子要往带 RLS 的表里写（眼下只有 merchants/shop_settings 没有
// RLS，但 categories/products 随时会加进来），而 keel_app 在没有租户上下文的
// 连接上一行都插不进去。这是少数几个正当使用 AdminDSN 的地方之一。
func TestMain(m *testing.M) {
	if err := loadSeed(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func loadSeed() error {
	sql, err := os.ReadFile(filepath.Join("..", "..", "db", "seed", "dev.sql"))
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// 无参数的 Exec 走简单查询协议，一次可以发多条语句。
	_, err = conn.Exec(ctx, string(sql))
	return err
}

// newRouter 造一个挂了解析中间件的路由，/probe 回显当前租户。
//
// 池经 db.NewPool 拿，不是 pgxpool.New：后者不检查角色，于是「测试跑在一条能
// 绕过 RLS 的连接上」这个洞会原样回来。这里虽然只读 merchants（无 RLS），
// 但这个 helper 迟早会被抄去写别的测试。
func newRouter(t *testing.T, defaultCode string) *gin.Engine {
	t.Helper()
	pool, err := db.NewPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(tenant.NewResolver(pool, defaultCode).Middleware())
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
// 只断言状态码不够：回落到错误的商家同样是 200。
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

// 单商家部署：配了默认商家，不指名任何店的裸主机名解析到它。
func TestDefaultMerchantResolves(t *testing.T) {
	got := resolved(t, do(newRouter(t, "shop-a"), "localhost:8080"))
	if want := merchantID(t, "shop-a"); got != want {
		t.Fatalf("期望解析到 shop-a(%d)，实得 %d", want, got)
	}
}

// 多商家部署：子域名匹配 merchants.code。
func TestSubdomainResolves(t *testing.T) {
	got := resolved(t, do(newRouter(t, ""), "shop-b.example.com"))
	if want := merchantID(t, "shop-b"); got != want {
		t.Fatalf("期望解析到 shop-b(%d)，实得 %d", want, got)
	}
}

// 自定义域名：域名里不含 code，只能靠 shop_settings.domain 匹配。
//
// shop-c 的域名是 custom.example.net，它的第一段 "custom" 不是任何商家的 code。
// 拿子域名去比完整域名的实现会在这里 404。
func TestCustomDomainResolves(t *testing.T) {
	got := resolved(t, do(newRouter(t, ""), "custom.example.net"))
	if want := merchantID(t, "shop-c"); got != want {
		t.Fatalf("期望解析到 shop-c(%d)，实得 %d", want, got)
	}
}

// Host 大小写不敏感，端口与结尾的点都不参与匹配。
func TestHostNormalization(t *testing.T) {
	want := merchantID(t, "shop-b")
	for _, host := range []string{
		"SHOP-B.Example.COM",
		"shop-b.example.com:8443",
		"shop-b.example.com.",
	} {
		if got := resolved(t, do(newRouter(t, ""), host)); got != want {
			t.Fatalf("Host %q 期望解析到 shop-b(%d)，实得 %d", host, want, got)
		}
	}
}

// Review Focus 第 1 条：未知子域名必须 404，**不能回落到默认商家**。
// 回落等于任何人拼一个不存在的子域名就能看到默认店的数据。
func TestUnknownHostIs404NotFallback(t *testing.T) {
	if got := do(newRouter(t, "shop-a"), "nope.example.com").Code; got != 404 {
		t.Fatalf("未知 Host 期望 404，实得 %d —— 是否错误地回落到了默认商家？", got)
	}
}

// 单商家部署里，Host 也不能把请求领到别的商家去。
//
// Host 是客户端可以随便写的。配了默认商家的部署只服务那一家，一个指名了别家店的
// Host 必须 404 —— 否则「单商家部署」只是配置上的说法，实际谁都能挑租户。
func TestDefaultDeploymentRejectsAnotherMerchantsHost(t *testing.T) {
	if got := do(newRouter(t, "shop-a"), "shop-b.example.com").Code; got != 404 {
		t.Fatalf("指向别家店的 Host 期望 404，实得 %d", got)
	}
}

// 但默认商家自己的域名当然要能用：单商家部署也可以挂在真实域名上。
func TestDefaultDeploymentAcceptsItsOwnDomain(t *testing.T) {
	got := resolved(t, do(newRouter(t, "shop-a"), "shop-a.example.com"))
	if want := merchantID(t, "shop-a"); got != want {
		t.Fatalf("期望解析到 shop-a(%d)，实得 %d", want, got)
	}
}

// Review Focus 第 2 条：停用的商家不可访问。
func TestDisabledMerchantIs404(t *testing.T) {
	if got := do(newRouter(t, ""), "shop-closed.example.com").Code; got != 404 {
		t.Fatalf("停用商家期望 404，实得 %d", got)
	}
}

// 停用的商家被配成默认商家时同样不可访问 —— 否则 status = 2 只拦住了一条路径。
func TestDisabledDefaultMerchantIs404(t *testing.T) {
	if got := do(newRouter(t, "shop-closed"), "localhost:8080").Code; got != 404 {
		t.Fatalf("停用的默认商家期望 404，实得 %d", got)
	}
}

// 多商家部署里没有默认商家可回落，裸主机名就是 404。
func TestBareHostWithoutDefaultIs404(t *testing.T) {
	if got := do(newRouter(t, ""), "localhost:8080").Code; got != 404 {
		t.Fatalf("无默认商家的裸主机名期望 404，实得 %d", got)
	}
}

// 租户不可由请求头指定。
//
// 公开接口没有鉴权，支持用请求头挑租户等于让调用方自己声明它是哪家店。
// 这个测试钉住「我们没有偷偷加一个方便本地开发的头」。
func TestRequestHeadersCannotChooseTheTenant(t *testing.T) {
	a := merchantID(t, "shop-a")
	headers := [][]string{
		{"X-Merchant-Id", "2"},
		{"X-Merchant-Code", "shop-b"},
		{"X-Tenant-Id", "2"},
		{"X-Forwarded-Host", "shop-b.example.com"},
		{"X-Tenant", "shop-b"},
	}
	for _, h := range headers {
		// 单商家部署：无论头里写什么，都还是默认商家。
		if got := resolved(t, do(newRouter(t, "shop-a"), "localhost:8080", h...)); got != a {
			t.Fatalf("请求头 %v 改变了解析结果：期望 shop-a(%d)，实得 %d", h, a, got)
		}
		// 多商家部署：头不能替 Host 背书，未知 Host 仍是 404。
		if got := do(newRouter(t, ""), "nope.example.com", h...).Code; got != 404 {
			t.Fatalf("请求头 %v 让未知 Host 解析成功了（%d）", h, got)
		}
	}
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

	count := func() (int64, int64) {
		t.Helper()
		var merchants, settings int64
		if err := conn.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM merchants), (SELECT count(*) FROM shop_settings)`).
			Scan(&merchants, &settings); err != nil {
			t.Fatal(err)
		}
		return merchants, settings
	}

	m1, s1 := count()
	if err := loadSeed(); err != nil {
		t.Fatal(err)
	}
	m2, s2 := count()
	if m1 != m2 || s1 != s2 {
		t.Fatalf("重复加载种子改变了行数：merchants %d→%d，shop_settings %d→%d", m1, m2, s1, s2)
	}
}
