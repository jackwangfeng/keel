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
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// baseDomain 与种子里登记的域名一致。
const baseDomain = "example.com"

var (
	testPool   *pgxpool.Pool
	testEngine *gin.Engine

	// testTC 是**真的**嵌入式协调器，存储是一个临时目录里的 sqlite。
	//
	// 不 mock 它：下单这条链路里最容易出问题的东西全在 Go→C→Rust→C→Go 那条
	// 边界上（分支被不被调用、gid 有没有被改、失败到底触不触发补偿），
	// 而 mock 掉那条边界等于把要测的东西整个换掉。
	testTC *dtm.TC

	// testOrders 是路由里那一个下单服务 —— 同一个实例。分支注册在它身上，
	// 而失败原因是经它内部那张表递给 HTTP 那一侧的（见 service/order_saga.go
	// 的 branchNotes）。换一个实例，那条路径就断了而测试看不出来。
	testOrders *service.OrderService

	// testSigner 是路由里那一个 —— **同一个实例**，不是一份长得一样的复制品。
	// 测试要用它签出「过期的」「别家店的」「类型不对的」令牌，而那些令牌必须
	// 真的能被服务端验签，否则测试验的就只是「随便一串东西会被拒」，
	// 那对任何一条断言都没有区分力。
	testSigner = auth.NewSigner([]byte("keel-test-secret-key-32-bytes-long!!"))
)

// newSweeper 建一个超时补偿服务，接在**同一个池**上。
//
// 每条测试自己建一个而不是共用一个包级实例：SweepService 里有一个轮转游标
// （cursor），共用的话一条测试跑过之后下一条的起点就变了 —— 而公平调度那条
// 测试恰恰要断言「这一轮从哪家开始」。每条测试拿一个干净的游标，
// 断言才有确定的含义。
//
// 预算由调用方给：默认值（每租户 50、每轮 500）对公平调度那条测试没有区分力，
// 因为种子里的订单量远够不到上限。
func newSweeper(cfg service.SweepConfig) *service.SweepService {
	return service.NewSweepService(repository.New(testPool), cfg, nil)
}

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
	if testTC != nil {
		testTC.Close()
	}
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
	// 下单服务 → 注册分支 → 起协调器 → 接上。顺序与 app.Run 里一模一样，
	// 理由见 service.OrderService.AttachCoordinator。
	testOrders = service.NewOrderService(repository.New(pool), nil, nil)
	dtmDir, err := os.MkdirTemp("", "keel-handler-dtm-")
	if err != nil {
		return err
	}
	// 除了真分支，还注册两个**只在测试里存在**的分支。
	//
	// 它们是为了让「库存补偿真的把货放回去了」这条断言有一条可达的路径。
	// 生产编排里库存是最后一步，而一个失败的分支是原子回滚的（屏障那一行和
	// 业务写在同一个事务里），所以正常链路上库存的补偿永远轮不到真的执行 ——
	// 对着一条不可达的分支写断言，正是这个仓库前几轮反复踩到的那种空转。
	//
	// 有了 test_always_fail，测试可以提交一个「真库存分支 + 注定失败的第二步」
	// 的 SAGA：第一步扣完并提交，第二步失败，协调器回过头来调真的补偿。
	// 被测的是真分支、真屏障、真库，只有「让它失败」这件事是测试提供的。
	branches := app.Branches(testOrders)
	branches["test_always_fail"] = func(string, string, string) int { return dtm.Failure }
	branches["test_always_fail_undo"] = func(string, string, string) int { return dtm.Success }

	tc, err := dtm.Start("sqlite:"+filepath.Join(dtmDir, "dtm.db"), 0, branches)
	if err != nil {
		return fmt.Errorf("起协调器失败: %w", err)
	}
	testTC = tc
	testOrders.AttachCoordinator(tc)

	// 刻意不配默认商家：跨租户测试要走 Host 解析那条真实路径。
	gin.SetMode(gin.TestMode)
	// 沙箱支付打开：本包里「发起支付 → 回调 → 订单变 20」那一组测试走的就是它。
	// 关掉沙箱的那条路（501）由 TestPaymentIntentIsRefusedWhenSandboxIsOff 用
	// 一个单独装出来的路由验，不动这个包级实例 —— 换掉它会让别的测试
	// 在一个它们没预期的配置上跑。
	// 检索那一路挂的是 conceptEmbedder（search_fixture_test.go）——
	// 一个**语义可控**的替身：它按文本里出现的概念词给向量，而不是按字面哈希。
	// 为什么需要「可控」而不是「随便一个确定性向量」，写在那个文件的头上。
	//
	// 降级链（引擎真的打不通）那一条不用这个实例：它单独装一套路由，
	// 接一个指向**真的没人监听的端口**的 inference.Client，
	// 见 TestSearchDegradesToKeywordWhenEngineIsDown。
	// /search 上挂着按 IP 的限流（app/ratelimit.go）。这个包里的测试全部
	// 从同一个来源地址（httptest 的 192.0.2.1）打进来，几十次检索会把默认
	// 配额（4/秒、瞬时 8）打穿 —— 那时红的会是一批与限流毫无关系的测试，
	// 而且红得没有规律（取决于跑到第几个）。
	//
	// 所以这个包级路由把配额调到一个测试打不穿的数。**限流本身仍然有执行者**：
	// TestSearchRateLimitsAFloodFromOneIP 自己用 t.Setenv 配一个很紧的配额、
	// 自己装一套路由（走的是同一个 app.Router），所以「这道闸门真的挂在
	// /search 上」那件事由它证明。
	os.Setenv(app.EnvSearchRateLimit, "100000")
	os.Setenv(app.EnvSearchRateBurst, "100000")

	testEngine = app.Router(pool,
		tenant.NewResolver(pool, tenant.Config{BaseDomain: baseDomain}), testSigner, testOrders,
		service.PaymentConfig{Sandbox: true}, conceptEmbedder{})
	return nil
}

// ensureSchema 只在缺 schema 时才跑迁移，惯例同 internal/repository 的测试。
func ensureSchema(ctx context.Context) error {
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return err
	}
	defer admin.Close(ctx)

	// 无条件重建 schema，不做「表已经在了就跳过」。
	//
	// 跳过有两个代价，都由验收实测出来：
	//
	//  一、**可变状态会跨轮次累积。** 种子是 NOT EXISTS 幂等的，补不回被扣掉的
	//     库存。同一个库连跑 handler 包，前三轮绿、第四轮起必红
	//     （`shop-a 里找不到水位 >= 2 的 SKU`）。红的是夹具不是被测语义。
	//
	//  二、**改了一份已应用的迁移，在暖库上是假绿。** 实测：删掉 orders.user_id
	//     的复合外键，暖库上 internal/db 照样 ok，换空库才红。
	//
	// 第二条尤其要紧：它是「删掉被守护的那段逻辑、看它红不红」这套方法的地基。
	// 验收自己第一轮变异就跑在被污染的库上，5 条测试同时红、红的全是夹具，
	// 结论作废重做了一遍。一个会把「没有区分力的断言」判成「有区分力」的
	// 测试环境，比慢几秒糟得多。
	if _, err := admin.Exec(ctx,
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		return err
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
