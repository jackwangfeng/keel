package app_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm/dtmtest"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/testdb"
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

// TestMain 给本包一个只属于它的库（keel_test_app），迁移之后加载种子。
// 每次运行都是新库，理由见 internal/testdb。
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, testdb.Package{Name: "app", Setup: loadSeed}))
}

func loadSeed(ctx context.Context) error {
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return err
	}
	defer admin.Close(ctx)

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

// env 把四个变量一次设全，包括要清空的那些。
//
// 只设要用的、不清其它的话，测试会继承开发机上已有的 KEEL_* 变量，
// 于是同一份代码在两台机器上跑出不同结果 —— 而这组测试断言的正是配置的组合。
//
// 协调器的存储每条测试一个临时 sqlite 文件：它是有状态的（存的就是「哪些事务
// 还没跑完」），共用一份的话，前一条测试留下的事务会在后一条里被推进，
// 而那正是这组测试最不该有的那种耦合。
func env(t *testing.T, defaultMerchant, baseDomain string) {
	t.Helper()
	t.Setenv(app.EnvDefaultMerchant, defaultMerchant)
	t.Setenv(app.EnvBaseDomain, baseDomain)
	t.Setenv(app.EnvAddr, "127.0.0.1:0")
	// 走 dtmtest：Run 返回（协调器已 Close）之后 dtmrs 的 sqlite 线程还会在目录里
	// 删建文件，直接用 t.TempDir() 会偶发 `directory not empty`。见 dtmtest 包注释。
	t.Setenv(app.EnvDTMDSN, dtmtest.SQLiteDSN(t))
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

// 没配协调器存储时，Run 必须在监听之前退出。
//
// 这一条守的是一个「不配也能跑起来」的诱惑：给 KEEL_DTM_DSN 一个默认值
// （当前目录下的 sqlite 文件，或者干脆内存）就没人会看见这条错误，而代价要到
// 很久以后才显形 —— 协调器存的是「哪些全局事务还没跑完」，这份状态随容器一起
// 消失时，正向阶段已经扣掉的库存与已经核销的券就再也没人回补。
// 架构 §5 说的「少卖」在那一刻从可恢复变成永久漏账。
//
// 断言的是行为：给一份除了 DSN 之外完全合法的配置，看 Run 有没有在监听之前退出。
func TestRunRefusesToStartWithoutCoordinatorStore(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvDTMDSN, "")

	var s spy
	err := app.Run(context.Background(), s.listen)
	if err == nil {
		t.Fatal("没配协调器存储，Run 必须拒绝启动")
	}
	if s.called {
		t.Fatalf("没配协调器存储却已经开始监听（addr=%q）", s.addr)
	}
	// 断言的是**那句给人看的话**，不是「有没有报错」。
	//
	// 这一条是实测出来的：把 Run 里那个空值检查删掉，dtmrs 自己也会失败 ——
	// 但它说的是 `error with configuration: relative URL without a base`，
	// 一句不指向任何该做的事的话。检查还在的话，运维看到的是变量名加上
	// 单机 / 多实例两种配法。所以这里要的是后者，只查「报了错」是查不出区别的。
	for _, want := range []string{app.EnvDTMDSN, "必须显式指定", "sqlite:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息里没有 %q —— 这说明拒绝启动的不是那个显式检查，"+
				"而是 dtmrs 自己在一个说不清真因的地方失败了：%v", want, err)
		}
	}
	t.Logf("Run 如期拒绝启动：%v", err)
}

// 协调器起不来时，Run 必须在监听之前退出。
//
// 上一条只证明「空配置会被挡住」，那是一句纯字符串判断 —— 单有它的话，一个
// 「先监听、再在后台慢慢把协调器拉起来」的实现照样绿。这条走的是真的去建存储
// 那一支：DSN 合法但落不下去，失败只能来自 dtm.Start。
//
// 它守的是顺序。协调器起不来就一笔订单也做不了，而此时开始接请求，客人看到的是
// 「提交订单」按下去之后的 500，编排系统看到的是一个健康的进程 ——
// 没有任何东西会把这两件事联系起来。
func TestRunRefusesToStartWhenCoordinatorCannotStart(t *testing.T) {
	env(t, "", "example.com")
	// 目录不存在 → sqlite 落不下去。Open 不碰数据库，Start 才碰，
	// 所以这里失败的一定是 Start。
	t.Setenv(app.EnvDTMDSN, "sqlite:"+filepath.Join(t.TempDir(), "没有这个目录", "dtm.db"))

	var s spy
	err := app.Run(context.Background(), s.listen)
	if err == nil {
		t.Fatal("协调器存储落不下去，Run 必须拒绝启动")
	}
	if s.called {
		t.Fatalf("协调器没起来却已经开始监听（addr=%q）—— "+
			"dtm.Start 排到监听后面去了", s.addr)
	}
	if !strings.Contains(err.Error(), "协调器") {
		t.Fatalf("错误信息不像是协调器启动失败：%v", err)
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

	// 协调器传 nil：这条测试只看路由表，而挂路由不需要协调器。
	// 真要下单时 Create 会当场报「协调器没有接上」—— 那是刻意的失败方向。
	orders := service.NewOrderService(repository.New(pool), nil, nil)
	r := app.Router(pool, tenant.NewResolver(pool, tenant.Config{BaseDomain: "example.com"}),
		auth.NewSigner([]byte("keel-test-secret-key-32-bytes-long!!")), orders,
		// 引擎传 nil：这条测试只看路由表，而 /search 在没有引擎时照样挂得上去
		// （退化成纯关键词召回，语义检索层 §8）。传 nil 同时是一次形状断言 ——
		// 哪天 Router 变成「没有引擎就不挂这条路由」，下面那张表会红。
		service.PaymentConfig{Sandbox: true}, nil)
	want := map[string]bool{
		"GET /healthz":                           false,
		"GET /version":                           false,
		"GET /api/v1/products":                   false,
		"GET /api/v1/categories":                 false,
		"POST /api/v1/search":                    false,
		"GET /api/v1/products/:product_id":       false,
		"POST /api/v1/orders":                    false,
		"POST /api/v1/orders/preview":            false,
		"GET /api/v1/orders":                     false,
		"GET /api/v1/orders/:order_no":           false,
		"POST /api/v1/orders/:order_no/payments": false,

		// 后台那 7 条（M4 后台身份）。它们和上面那些一样只是在核路径 ——
		// 「挂没挂对中间件」由 internal/handler 那一组行为测试守着。
		"POST /api/v1/admin/auth/bootstrap":   false,
		"POST /api/v1/admin/auth/email-link":  false,
		"POST /api/v1/admin/auth/session":     false,
		"GET /api/v1/admin/me":                false,
		"GET /api/v1/admin/staff":             false,
		"POST /api/v1/admin/staff":            false,
		"PATCH /api/v1/admin/staff/:staff_id": false,

		// 开店（M4 收尾）。它挂同一道 staffAuth，而「只有平台级管理员能调」
		// 那一条是业务规则，由 internal/handler 的
		// TestOpeningAShopIsPlatformOnly 打一次商家级会话来证明。
		"POST /api/v1/admin/merchants": false,

		// 商家管理（列表 / 详情 / 改名与停用启用）。「只有平台级能调」由
		// internal/handler 的 tenant_switch_test.go 打商家级会话来证明。
		"GET /api/v1/admin/merchants":                false,
		"GET /api/v1/admin/merchants/:merchant_id":   false,
		"PATCH /api/v1/admin/merchants/:merchant_id": false,

		// 商家自助发布那 16 条（M4 Task 3）。同样只核路径 ——
		// 「每一条都挂了 staffAuth」由 internal/handler 的
		// TestAdminCatalogRoutesAllRequireStaffSession 逐条打一次来证明，
		// 因为把某一行的中间件删掉，这张路由表一个字都不会变。
		"POST /api/v1/admin/uploads":                          false,
		"GET /api/v1/admin/products":                          false,
		"POST /api/v1/admin/products":                         false,
		"GET /api/v1/admin/products/:product_id":              false,
		"PATCH /api/v1/admin/products/:product_id":            false,
		"DELETE /api/v1/admin/products/:product_id":           false,
		"POST /api/v1/admin/products/:product_id/publication": false,
		"PUT /api/v1/admin/products/:product_id/images":       false,
		"POST /api/v1/admin/products/:product_id/skus":        false,
		"PATCH /api/v1/admin/skus/:sku_id":                    false,
		"DELETE /api/v1/admin/skus/:sku_id":                   false,
		"PUT /api/v1/admin/skus/:sku_id/inventory":            false,
		"GET /api/v1/admin/categories":                        false,
		"POST /api/v1/admin/categories":                       false,
		"PATCH /api/v1/admin/categories/:category_id":         false,
		"DELETE /api/v1/admin/categories/:category_id":        false,

		// 读文件那两跳（M4 收尾）。第二条**不在契约里**（它是第一跳签出来的
		// 限时地址，形状随 driver 变），在 contract_test.go 的
		// nonContractRoutes 里挂着账 —— 但它照样要在这张表里，
		// 因为「挂没挂上」和「在不在契约里」是两件事。
		"GET /api/v1/uploads/:upload_id":      false,
		"GET /api/v1/uploads/:upload_id/blob": false,

		// 买家侧门店那两条（00020）。契约里它们是 security: []，
		// 所以这里没有对应的「要不要令牌」断言 —— 那正是它们与下面 21 条的
		// 唯一差别，而差别只写在 app.go 那两行有没有 staffAuth 上。
		"GET /api/v1/stores":         false,
		"GET /api/v1/stores/resolve": false,

		// 门店 / 大区后台那 21 条（00020）。同样只核路径 ——
		// 「每一条都挂了 staffAuth」由 internal/handler 的
		// TestAdminStoreRoutesAllRequireStaffSession 逐条打一次来证明。
		"GET /api/v1/admin/regions":                                         false,
		"POST /api/v1/admin/regions":                                        false,
		"PATCH /api/v1/admin/regions/:region_id":                            false,
		"DELETE /api/v1/admin/regions/:region_id":                           false,
		"GET /api/v1/admin/regions/:region_id/products":                     false,
		"PUT /api/v1/admin/regions/:region_id/products/:product_id/listing": false,
		"PUT /api/v1/admin/regions/:region_id/skus/:sku_id/price":           false,
		"DELETE /api/v1/admin/regions/:region_id/skus/:sku_id/price":        false,
		"GET /api/v1/admin/stores":                                          false,
		"POST /api/v1/admin/stores":                                         false,
		"GET /api/v1/admin/stores/:store_id":                                false,
		"PATCH /api/v1/admin/stores/:store_id":                              false,
		"DELETE /api/v1/admin/stores/:store_id":                             false,
		"PUT /api/v1/admin/stores/:store_id/fence":                          false,
		"PUT /api/v1/admin/stores/:store_id/default":                        false,
		"GET /api/v1/admin/stores/:store_id/products":                       false,
		"PUT /api/v1/admin/stores/:store_id/products/:product_id/listing":   false,
		"PUT /api/v1/admin/stores/:store_id/skus/:sku_id/price":             false,
		"DELETE /api/v1/admin/stores/:store_id/skus/:sku_id/price":          false,
		"GET /api/v1/admin/stores/:store_id/inventories":                    false,
		"PUT /api/v1/admin/stores/:store_id/skus/:sku_id/inventory":         false,

		// 买家自己的三组：个人信息 6 条、地址簿 6 条、购物车 7 条。同样只核路径 ——
		// 「每一条都挂了 auth.Bearer」由 internal/handler 的
		// TestBuyerSelfRoutesRequireToken 逐条不带令牌打一次来证明。
		"GET /api/v1/me":                            false,
		"PATCH /api/v1/me":                          false,
		"GET /api/v1/me/identities":                 false,
		"POST /api/v1/me/identities/wechat":         false,
		"DELETE /api/v1/me/identities/:provider":    false,
		"POST /api/v1/me/phone":                     false,
		"GET /api/v1/addresses":                     false,
		"POST /api/v1/addresses":                    false,
		"GET /api/v1/addresses/:address_id":         false,
		"PUT /api/v1/addresses/:address_id":         false,
		"DELETE /api/v1/addresses/:address_id":      false,
		"PUT /api/v1/addresses/:address_id/default": false,
		"GET /api/v1/cart":                          false,
		"DELETE /api/v1/cart":                       false,
		"POST /api/v1/cart/items":                   false,
		"PUT /api/v1/cart/selection":                false,
		"POST /api/v1/cart/items/batch-delete":      false,
		"PATCH /api/v1/cart/items/:item_id":         false,
		"DELETE /api/v1/cart/items/:item_id":        false,
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
