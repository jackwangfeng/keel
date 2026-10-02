package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/testdb"
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

	// testUploadRoot 是本地磁盘 driver 在测试里的根目录，由 setup 建。
	// 上传那条测试拿它把 storage_key 翻成真实路径去比对字节 ——
	// 「文件真的存下来了」这件事只能在文件系统上判。
	testUploadRoot string

	// testOrders 是路由里那一个下单服务 —— 同一个实例。分支注册在它身上，
	// 而失败原因是经它内部那张表递给 HTTP 那一侧的（见 service/order_saga.go
	// 的 branchNotes）。换一个实例，那条路径就断了而测试看不出来。
	testOrders *service.OrderService

	// testInvLocal 是建在测试池上的进程内库存服务，它的两个 SAGA 分支注册在 testTC 上（单体形态）。
	testInvLocal *inventory.Local

	// testQuotaSync 是包级路由上活动配额同步的发送方（二阶段消息，service/promotion_quota_msg.go）：
	// 回查与接收分支都注册在 testTC 上，与单体的 app.Run 同一个装法 —— 于是既有的活动测试全部走消息那条路。
	testQuotaSync *service.QuotaSync

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
	return service.NewSweepService(repository.New(testPool), localInventory(), cfg, nil)
}

// TestMain 备好 schema、加载种子、装一次路由。
//
// 种子不走 `psql -f`：宿主机上不一定有 psql（数据库跑在容器里，客户端二进制
// 并不随之出现在 PATH 上），而 exec 一个不存在的命令报的是 "executable file
// not found" —— 看上去像环境坏了，而不是「种子没加载」。用 Go 读文件再 Exec，
// 依赖面只剩下已经被测试依赖的 pgx。internal/tenant 的测试同此惯例。
//
// 库是本包自己的（keel_test_handler），由 testdb.Main 新建并迁移 —— 每次运行
// 都是新库，所以不存在「可变状态跨轮次累积」（种子是 NOT EXISTS 幂等的，补不回
// 被扣掉的库存；以前同一个库连跑到第四轮必红）和「改了已应用的迁移、暖库假绿」。
// 以前这两件事靠 ensureSchema 无条件 DROP SCHEMA 保证，见 internal/testdb。
// 第二条是「删掉被守护的那段逻辑、看它红不红」这套方法的地基：验收有一轮变异
// 跑在被污染的库上，5 条测试同时红、红的全是夹具，结论作废重做。
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, testdb.Package{Name: "handler", Setup: setup, Teardown: teardown}))
}

func teardown() {
	if testTC != nil {
		testTC.Close()
	}
	if testPool != nil {
		testPool.Close()
	}
}

func setup(ctx context.Context) error {
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
	invLocal := inventory.NewLocal(repository.NewInventoryStore(pool))
	testInvLocal = invLocal
	testOrders = service.NewOrderService(repository.New(pool), invLocal, nil, nil)
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

	// 活动配额同步：回查 + 接收（库存在进程内，回源是进程内实现）。
	testQuotaSync = service.NewQuotaSync(repository.New(pool), dtm.BranchResolver{}, dtm.BranchResolver{})
	exBranches := app.InventoryBranches(invLocal)
	for name, fn := range app.QuotaSyncBranches(testQuotaSync, invLocal, service.NewPromotionQuotaSource(repository.New(pool))) {
		exBranches[name] = fn
	}

	// 库存的两个分支（带载荷）与配额同步（载荷带定义）与单体一样注册在进程内。
	tc, err := dtm.StartEx("sqlite:"+filepath.Join(dtmDir, "dtm.db"), 0, branches, exBranches)
	if err != nil {
		return fmt.Errorf("起协调器失败: %w", err)
	}
	testTC = tc
	testOrders.AttachCoordinator(tc)
	testQuotaSync.Attach(tc)

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
	// /search/events 那只桶同理（它与 /search 各自一只），执行者是
	// search_event_test.go 的 TestSearchEventsAreRateLimitedInTheirOwnBucket。
	os.Setenv(app.EnvSearchEventRateLimit, "100000")
	os.Setenv(app.EnvSearchEventRateBurst, "100000")

	// 商品图落在一个临时目录里。**必须显式配**：不配的话
	// app.uploadStoreFromEnv 会取 os.TempDir()/keel-uploads，
	// 那是一个跨测试轮次、跨进程共享的目录，而
	// TestUploadChecksMediaTypeAndActuallyStoresTheBytes 要按 storage_key
	// 去磁盘上比对内容 —— 共享目录下那条断言仍然会绿，只是它验的东西
	// 可能是上一轮留下的。
	uploadRoot, err := os.MkdirTemp("", "keel-handler-uploads-")
	if err != nil {
		return err
	}
	testUploadRoot = uploadRoot
	os.Setenv(app.EnvUploadRoot, uploadRoot)

	testEngine = app.Router(pool,
		tenant.NewResolver(pool, tenant.Config{BaseDomain: baseDomain}), testSigner, testOrders,
		service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithQuotaSync(testQuotaSync))
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
