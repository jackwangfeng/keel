package app_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/tenant"
)

// 这一组测试守的是一句只有一行代码的事实：**启动路径上调了 Preflight，
// 而且在开始监听之前调**。
//
// 它值得一整个文件，因为它是整个项目里最容易悄悄消失、消失之后又最看不出来的
// 一行。tenant.Preflight 里的四道检查（两个租户来源同时配置 / 默认商家加多家
// 活跃商家 / 默认商家不可服务 / 活跃商家没有任何入口）没有一道会在运行期报错 ——
// 它们防的全是「不报错但答错」。没人调它的话，那四道检查连同它们的测试会一起
// 变成死代码：tenant 包的测试直接调 Preflight，所以它们照样全绿。
//
// 断言的方式是行为而不是「某处出现了 Preflight 这个词」：给一份注定通不过自检的
// 环境变量，然后看 Run 有没有在监听之前退出。把 Run 里那行 Preflight 删掉，
// 下面 TestRunRefuses* 两条都会红。

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "准备测试环境失败: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func setup() error {
	ctx := context.Background()
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
	if !ok {
		out, err := exec.Command("make", "-C", "../..", "migrate",
			"GOOSE_DBSTRING="+db.AdminDSN()).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w\n%s", err, out)
		}
	}

	seed, err := os.ReadFile(filepath.Join("..", "..", "db", "seed", "dev.sql"))
	if err != nil {
		return err
	}
	_, err = admin.Exec(ctx, string(seed))
	return err
}

// spy 记下 Run 有没有走到「开始监听」那一步。
type spy struct {
	called bool
	addr   string
}

func (s *spy) listen(addr string, _ http.Handler) error {
	s.called = true
	s.addr = addr
	return nil
}

// env 把三个变量一次设全，包括要清空的那些。
//
// 只设要用的、不清其它的话，测试会继承开发机上已有的 KEEL_* 变量，
// 于是同一份代码在两台机器上跑出不同结果 —— 而这组测试断言的正是配置的组合。
func env(t *testing.T, defaultMerchant, baseDomain string) {
	t.Helper()
	t.Setenv(app.EnvDefaultMerchant, defaultMerchant)
	t.Setenv(app.EnvBaseDomain, baseDomain)
	t.Setenv(app.EnvAddr, "127.0.0.1:0")
}

// 阳性对照，必须排在前面。
//
// 没有它的话，「listen 没被调用」这个观察一文不值：Run 在 Preflight 之前就
// 失败（连不上库、池建不出来）同样会让 listen 没被调用，而下面两条照样绿。
// 这条证明在一份合法配置下同一个 Run 确实会走到监听那一步。
func TestRunReachesListenWithValidConfig(t *testing.T) {
	env(t, "", "example.com")

	var s spy
	if err := app.Run(context.Background(), s.listen); err != nil {
		t.Fatalf("合法配置下 Run 不该失败：%v", err)
	}
	if !s.called {
		t.Fatal("Run 没有走到监听那一步")
	}
	if s.addr != "127.0.0.1:0" {
		t.Fatalf("监听地址是 %q，期望取自 %s", s.addr, app.EnvAddr)
	}
}

// 两个租户来源同时配置时，Run 必须在监听之前退出。
//
// 这条同时钉住三件事：
//  1. 启动路径上确实调了 Preflight（不调的话这份配置不会有任何症状，Run 返回 nil）；
//  2. 调用发生在监听之前（s.called 必须是 false）；
//  3. main 读的环境变量名与 Preflight 错误信息里写的是同两个名字 —— 名字对不上的话
//     DefaultCode 会是空串，这条冲突根本不成立，Run 会一路跑到监听。
func TestRunRefusesToStartWhenBothTenantSourcesAreConfigured(t *testing.T) {
	env(t, "shop-a", "example.com")

	var s spy
	err := app.Run(context.Background(), s.listen)
	if err == nil {
		t.Fatal("同时配了默认商家与基础域名，Run 必须拒绝启动 —— " +
			"启动路径上是不是没调 Preflight？")
	}
	if s.called {
		t.Fatalf("自检没过却已经开始监听（addr=%q）—— Preflight 排在监听后面了", s.addr)
	}
	for _, want := range []string{app.EnvDefaultMerchant, app.EnvBaseDomain, "不能同时配置"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息里没有 %q，这说明拒绝启动的不是 Preflight 的那道检查：%v",
				want, err)
		}
	}
	t.Logf("Run 如期拒绝启动：%v", err)
}

// 另一道检查：单商家配置 + 库里有多家活跃商家。
//
// 上一条只用到 Preflight 里那道纯配置的检查，它不查库。单有那一条的话，
// 一个「Preflight 调了但池没接上」的实现也能绿 —— 而 Preflight 的另外三道
// 全都要查库。这条走的是查库那一支：它证明 Run 交给解析器的池是能用的。
func TestRunRefusesToStartWhenDefaultMerchantHidesOtherShops(t *testing.T) {
	env(t, "shop-a", "")

	var s spy
	err := app.Run(context.Background(), s.listen)
	if err == nil {
		t.Fatal("配了默认商家但库里有多家活跃商家，Run 必须拒绝启动")
	}
	if s.called {
		t.Fatal("自检没过却已经开始监听")
	}
	if !strings.Contains(err.Error(), "活跃商家") {
		t.Fatalf("错误信息不像是那道「默认商家遮住了其它店」的检查：%v", err)
	}
	t.Logf("Run 如期拒绝启动：%v", err)
}

// 路由挂在契约写的路径上。
//
// 路径写错（少个 /api/v1、写成 /product）不会让任何单元测试变红，
// 但它让整个服务对着契约写的客户端全是 404。
func TestRouterServesContractPaths(t *testing.T) {
	pool, err := db.NewPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	r := app.Router(pool, tenant.NewResolver(pool, tenant.Config{BaseDomain: "example.com"}))
	want := map[string]bool{
		"GET /healthz":         false,
		"GET /api/v1/products": false,
	}
	for _, ri := range r.Routes() {
		key := ri.Method + " " + ri.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("路由表里没有 %q，实际是 %v", key, r.Routes())
		}
	}
}
