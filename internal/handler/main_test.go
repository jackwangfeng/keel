package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/tenant"
)

// baseDomain 与种子里登记的域名一致。
const baseDomain = "example.com"

var (
	testPool   *pgxpool.Pool
	testEngine *gin.Engine
)

// TestMain 备好 schema、加载种子、装一次路由。
//
// 种子不走 `psql -f`：宿主机上不一定有 psql（数据库跑在容器里，客户端二进制
// 并不随之出现在 PATH 上），而 exec 一个不存在的命令报的是 "executable file
// not found" —— 看上去像环境坏了，而不是「种子没加载」。用 Go 读文件再 Exec，
// 依赖面只剩下已经被测试依赖的 pgx。internal/tenant 的测试同此惯例。
func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "准备测试环境失败: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	testPool.Close()
	os.Exit(code)
}

func setup() error {
	ctx := context.Background()
	if err := ensureSchema(ctx); err != nil {
		return err
	}
	if err := loadSeed(ctx); err != nil {
		return err
	}

	pool, err := db.NewPool(ctx)
	if err != nil {
		return err
	}
	testPool = pool

	// 路由由 internal/app.Router 装 —— 与 cmd/keel 走的是同一个函数，
	// 不是一份在旁边慢慢跑偏的复制品。测试里若自己 r.GET("/api/v1/products", ...)，
	// 那么「main 把路由挂错了路径」这类错误谁也发现不了。
	//
	// 刻意不配默认商家：跨租户测试要走 Host 解析那条真实路径。
	gin.SetMode(gin.TestMode)
	testEngine = app.Router(pool, tenant.NewResolver(pool, tenant.Config{BaseDomain: baseDomain}))
	return nil
}

// ensureSchema 只在缺 schema 时才跑迁移，惯例同 internal/repository 的测试。
func ensureSchema(ctx context.Context) error {
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return err
	}
	defer admin.Close(ctx)

	var ok bool
	if err := admin.QueryRow(ctx,
		`SELECT to_regclass('public.products') IS NOT NULL`).Scan(&ok); err != nil {
		return err
	}
	if ok {
		return nil
	}
	out, err := exec.Command("make", "-C", "../..", "migrate",
		"GOOSE_DBSTRING="+db.AdminDSN()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

// loadSeed 用管理员连接加载种子：categories/products 带 RLS，
// keel_app 在没有租户上下文的连接上一行都插不进去。
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

// do 发一个请求到装好的真实路由上。host 决定它落到哪个租户。
func do(t *testing.T, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

// rawProductCount 用管理员连接（绕过 RLS）直接数库里的行：
// 该店可见的（status = 1 且未软删）与全部的。
//
// 它是「草稿与软删商品不可见」那条测试的阳性对照：库里没有不可见的行时，
// 那条测试什么也证明不了，而这个差值会当场说出来。
func rawProductCount(t *testing.T, code string) (visible, all int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	if err := conn.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE p.status = 1 AND p.deleted_at IS NULL),
		       count(*)
		  FROM products p
		  JOIN merchants m ON m.id = p.merchant_id
		 WHERE m.code = $1`, code).Scan(&visible, &all); err != nil {
		t.Fatal(err)
	}
	return visible, all
}
