package app

// 拆分部署的配置与装配（docs/电商系统-微服务拆分方案.md 阶段 0）。
//
// 单独一个文件，是因为这一块随拆分的阶段长：阶段 1a 接进了库存服务的内网路由与 core 侧的
// 远端客户端，SAGA 分支地址在阶段 1b 接进来。app.go 里的 Run 只多了几行调用。

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/buildinfo"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/rpc"
	"github.com/keel/keel/internal/service"
)

// EnvRole 选部署形态里本进程的角色。见 Role。
const EnvRole = "KEEL_ROLE"

// 拆分部署用的其余几个变量，名字定义在各自的包里（它们的错误信息也在那儿），
// 这里重新导出，好让 app 的测试与文档检查只认一个地方。
const (
	EnvInventoryDSN           = db.EnvInventoryDSN
	EnvInternalAddr           = rpc.EnvInternalAddr
	EnvInternalSecret         = rpc.EnvInternalSecret
	EnvInternalSecretPrevious = rpc.EnvInternalSecretPrevious
	EnvInventoryURL           = rpc.EnvInventoryURL
)

// EnvCoreURL 是库存服务找 core 内网端口的地址（如 http://app:8091），只在 KEEL_ROLE=inventory 时用，两件事：
//
//   - 可售数跨过 0 时，库存进程自己的协调器把二阶段消息投递到 <它>/internal/v1/saga/stock_changed
//     （inventory 包 stock_msg.go、service/stock_flags.go）。所以配了它就必须同时配 KEEL_DTM_DSN（库存进程的
//     协调器存储）；
//   - 活动配额同步消息（core 发、库存收）的接收分支经它回源读 core 的配额定义（inventory 包 activity_msg.go）。
//
// 不配：库存服务不发跨 0 通知（有货排序只靠 core 的低频全量刷新），配额同步消息读不到定义、一直重试
// （活动上线那一刻 core 仍会直接同步配额，上线的活动不受影响）。
//
// 它让「inventory 不回调 core」那条边界（拆分方案「服务边界」）多了一个例外，但只是一个方向很窄的例外：
// 库存服务从不**等** core —— 投递是协调器异步做的、带重试，core 不在时库存照常扣减，通知晚到而已。
const EnvCoreURL = "KEEL_CORE_URL"

// Role 是本进程在部署里扮演的角色。**同一个二进制**，靠它决定起哪些东西。
//
//	all（默认）  单体：公网 API + 全部后台任务 + 协调器，库存在进程内。今天的形态。
//	core         拆分形态里的主服务。库存的读与后台写经 KEEL_INVENTORY_URL 调库存服务
//	             （阶段 1a 起；**必须配这个地址**，见 validate）。下单扣减、关单 / 退款回补
//	             在阶段 1b 之前仍在进程内。
//	inventory    拆分形态里的库存服务。只起内网服务（KEEL_INTERNAL_ADDR）：
//	             /healthz、/version、/readyz 与 /internal/v1/...；
//	             不挂任何公网业务路由，不跑任何后台任务。下单 SAGA 的协调器在 core，库存分支是被它
//	             远程调用的一方；配了 KEEL_CORE_URL 时另起一个**只发跨 0 通知**的协调器（EnvCoreURL）。
//	             阶段 1a 起 /internal/v1/inventory/... 挂着库存服务的读与后台写；
//	             SAGA 库存分支在阶段 1b 挂上。
//
// 不认识的值拒绝启动，而不是回落到 all：把 "inventroy" 拼错的库存容器若回落成
// 单体，会带着一整套后台任务连上库存库，而编排系统看到的是一个健康的进程。
type Role string

const (
	RoleAll       Role = "all"
	RoleCore      Role = "core"
	RoleInventory Role = "inventory"
)

func roleFromEnv() Role {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvRole)))
	if v == "" {
		return RoleAll
	}
	return Role(v)
}

// SplitConfig 是拆分部署的那几项。全空 = 今天的单体。
type SplitConfig struct {
	Role           Role
	InventoryDSN   string
	InternalAddr   string
	InternalSecret string
	// InternalSecretPrevious 是 KEEL_INTERNAL_SECRET_PREVIOUS 的原始值（未解析）。
	// 轮换期间仍要接受的旧密钥，逗号分隔可以给多个；见 rpc.EnvInternalSecretPrevious
	// 的注释里的轮换步骤。空 = 不在轮换（今天的行为）。
	InternalSecretPrevious string
	InventoryURL           string
	// CoreURL 是 KEEL_CORE_URL（只在 inventory 角色上有意义）。
	CoreURL string
	// DTMDSN 是 KEEL_DTM_DSN 的一份副本：inventory 角色配了 CoreURL 时要起自己的协调器（runInventory），
	// 而那条启动路径只拿得到 SplitConfig。
	DTMDSN string
}

func splitConfigFromEnv() SplitConfig {
	return SplitConfig{
		Role:                   roleFromEnv(),
		InventoryDSN:           strings.TrimSpace(os.Getenv(EnvInventoryDSN)),
		InternalAddr:           strings.TrimSpace(os.Getenv(EnvInternalAddr)),
		InternalSecret:         os.Getenv(EnvInternalSecret),
		InternalSecretPrevious: os.Getenv(EnvInternalSecretPrevious),
		InventoryURL:           strings.TrimSpace(os.Getenv(EnvInventoryURL)),
		CoreURL:                strings.TrimRight(strings.TrimSpace(os.Getenv(EnvCoreURL)), "/"),
		DTMDSN:                 os.Getenv(EnvDTMDSN),
	}
}

// previousSecrets 解析 InternalSecretPrevious。validate 已经用同一个解析器
// 确认过它不会出错，这里只是拿结果——签名是 []string 而不是 (..., error)，
// 是因为调用点（internalRouter）已经在 validate 通过之后才跑得到。
func (s SplitConfig) previousSecrets() []string {
	prev, _ := rpc.ParsePreviousSecrets(s.InternalSecretPrevious)
	return prev
}

// validate 只查配置本身，不碰网络与数据库 —— 所以它排在 Run 的最前面，
// 一份注定要被拒绝的配置不该先去建连接池。
func (s SplitConfig) validate() error {
	switch s.Role {
	case RoleAll, RoleCore, RoleInventory:
	default:
		return fmt.Errorf("%s=%q 不认识，只能是 all（默认）/ core / inventory", EnvRole, string(s.Role))
	}
	if s.Role == RoleCore && s.InventoryURL == "" {
		// 决定（阶段 1a）：core 不回落到进程内库存。core 形态存在的全部理由是「库存在另一个
		// 进程里」，没配地址还照常起来，就是一个以为自己拆了、其实在读写本地库存表的 core ——
		// 拆分部署下那是另一个库里一份没人维护的旧数据，而页面上一切正常。
		return fmt.Errorf("%s=core 时必须配 %s（如 http://inventory:8090）：core 的库存调用只走远端；"+
			"单体请用 %s=all", EnvRole, EnvInventoryURL, EnvRole)
	}
	if s.Role == RoleInventory {
		if s.InternalAddr == "" {
			return fmt.Errorf("%s=inventory 时必须配 %s（如 :8090）：那是库存服务唯一的端口",
				EnvRole, EnvInternalAddr)
		}
		if s.InventoryURL != "" {
			// 库存服务自己就是 KEEL_INVENTORY_URL 指向的那一方。配了它，
			// 阶段 1 的代码就可能让库存服务去远程调用自己。
			return fmt.Errorf("%s=inventory 时不该配 %s（它是 core 用来找库存服务的地址）",
				EnvRole, EnvInventoryURL)
		}
	}
	if s.CoreURL != "" {
		if s.Role != RoleInventory {
			// core / all 自己就是这个地址指向的一方（all 的通知走进程内 local://）。
			return fmt.Errorf("%s 只在 %s=inventory 时配（它是库存服务投递跨 0 通知的目标）", EnvCoreURL, EnvRole)
		}
		if s.DTMDSN == "" {
			return fmt.Errorf("%s=inventory 配了 %s 就必须配 %s：跨 0 通知由库存进程自己的协调器投递，"+
				"它要一份自己的存储（与 core 那份分开，比如挂在卷上的 sqlite:/var/lib/keel/inventory-dtm.db）",
				EnvRole, EnvCoreURL, EnvDTMDSN)
		}
		if u, err := url.Parse(s.CoreURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
			u.Host == "" || u.RawQuery != "" {
			return fmt.Errorf("%s=%q 不是 http(s)://host[:port] 形式（不带 query）", EnvCoreURL, s.CoreURL)
		}
	}
	if (s.InternalAddr != "" || s.InventoryURL != "" || s.CoreURL != "") && len(s.InternalSecret) < rpc.MinSecretLen {
		// 内网服务不验签就是一个对任意租户开放的写接口；远端客户端没有密钥就签不了名。
		// 两种情况都不是「降级能跑」，所以拒绝启动。
		return fmt.Errorf("配了 %s、%s 或 %s 就必须配 %s（至少 %d 字节，所有进程同一个值；"+
			"例如 openssl rand -base64 48）", EnvInternalAddr, EnvInventoryURL, EnvCoreURL,
			EnvInternalSecret, rpc.MinSecretLen)
	}
	if _, err := rpc.ParsePreviousSecrets(s.InternalSecretPrevious); err != nil {
		// 同样的下限：轮换期间还在接受的旧密钥，安全余量不能比当前密钥低。
		return err
	}
	if s.InventoryURL != "" {
		// 两个构造器都会解析地址：客户端（阶段 1 的库存调用）与分支地址解析器。
		// 这里建一次只为了让格式错误在启动时暴露，而不是第一笔订单时。
		if _, err := rpc.NewClient(s.InventoryURL, s.InternalSecret, 0); err != nil {
			return err
		}
		if _, err := dtm.NewBranchResolver(s.InventoryURL, s.InternalSecret); err != nil {
			return err
		}
	}
	return nil
}

// inventoryPool 按 KEEL_INVENTORY_DSN 建库存池。没配时**就是** main 这个池
// （同一个 *pgxpool.Pool，不是连同一个库的第二个池）—— 单体形态的连接数一条都不多。
// 返回的 closeFn 只关自己建的池。
func inventoryPool(ctx context.Context, s SplitConfig, main *pgxpool.Pool) (*pgxpool.Pool, func(), error) {
	if s.InventoryDSN == "" {
		return main, func() {}, nil
	}
	p, err := db.NewPoolFromDSN(ctx, s.InventoryDSN)
	if err != nil {
		// 错误里不带 DSN：它含口令。
		return nil, nil, fmt.Errorf("建库存连接池失败（%s）: %w", EnvInventoryDSN, err)
	}
	return p, p.Close, nil
}

// internalRouter 建内网引擎：探针（/readyz 查库存池）+ 本进程拥有的服务间接口。
//
// 库存接口（/internal/v1/inventory/...）只挂在**拥有库存**的进程上：inventory 与 all。
// core 不挂 —— 它是库存服务的调用方，挂上就成了第二个库存服务入口，而且读写的是它自己
// 那个库里的库存表。SAGA 库存分支（dtm.MountBranches 到 routes.Saga）在阶段 1b 挂。
//
// x 是两条二阶段消息在内网上的那几端，见 internalExtras。
func internalRouter(s SplitConfig, inv *pgxpool.Pool, x internalExtras) *gin.Engine {
	r, routes := rpc.NewRouter(rpc.ServerConfig{
		Secret:          s.InternalSecret,
		PreviousSecrets: s.previousSecrets(),
		Ready:           inv.Ping,
	})
	if s.Role == RoleCore {
		// 拆分形态下库存进程发来的两样：跨 0 通知的投递（分支令牌准入），配额同步回源读定义（验签 + 租户头）。
		if x.flags != nil {
			dtm.MountBranches(routes.Saga, map[string]dtm.BranchFuncEx{
				inventory.BranchStockChanged: dtm.Ex(x.flags.StockMsgBranch()),
			})
		}
		if x.quotaSrc != nil {
			inventory.MountQuotaSource(routes.Tenant, x.quotaSrc)
		}
	}
	if s.Role == RoleInventory || s.Role == RoleAll {
		local := inventory.NewLocal(repository.NewInventoryStore(inv)).WithStockNotifier(x.notifier)
		inventory.Mount(routes.Tenant, local)
		// 库存的 SAGA 分支（阶段 1b）：core 的协调器经 http://…/internal/v1/saga/<名字> 调它们，
		// 分支令牌准入（rpc.Routes.Saga）。屏障记在库存池指向的库里。
		inventory.MountSaga(routes.Saga, local)
		if s.Role == RoleInventory {
			// 配额同步的接收分支：core 的协调器经 http://…/internal/v1/saga/inventory_activity_sync 投递
			// （单体走进程内 local://，不挂）。回源读定义经 KEEL_CORE_URL；没配时分支一律 Unknown 并喊出来。
			dtm.MountBranches(routes.Saga, map[string]dtm.BranchFuncEx{
				inventory.BranchActivitySync: dtm.Ex(local.ActivitySyncBranch(x.quotaSrc)),
			})
		}
	}
	return r
}

// internalExtras 是内网路由上除库存接口之外的那几样 —— 两条二阶段消息的端点（app/stock_flags.go、quota_sync.go）：
//
//	notifier  inventory / all：本进程 Local 共用的跨 0 通知器（nil = 不发）
//	flags     core：跨 0 通知的接收方
//	quotaSrc  core：挂出去给库存服务回源读活动配额定义；inventory：配额同步分支回源用（经 KEEL_CORE_URL）
type internalExtras struct {
	notifier *inventory.StockNotifier
	flags    *service.StockFlagService
	quotaSrc inventory.QuotaSource
}

// inventoryService 按角色选库存服务的实现：core 走 HTTP（KEEL_INVENTORY_URL，validate 已经
// 保证配了），其余走建在库存池上的进程内实现（单体时库存池就是业务池）。
//
// notifier 只接在进程内实现上（core 的 HTTP 实现不改库存，通知由库存进程发）。
func inventoryService(s SplitConfig, invPool *pgxpool.Pool, notifier *inventory.StockNotifier) (inventory.Service, error) {
	if s.Role == RoleCore {
		c, err := rpc.NewClient(s.InventoryURL, s.InternalSecret, 0)
		if err != nil {
			return nil, err
		}
		return inventory.NewRemote(c), nil
	}
	return inventory.NewLocal(repository.NewInventoryStore(invPool)).WithStockNotifier(notifier), nil
}

// RouterOption 是 Router 的可选项。
type RouterOption func(*routerOptions)

type routerOptions struct {
	inventory inventory.Service
	quotaSync *service.QuotaSync
}

// WithInventory 指定公网路由用的库存服务实现。不给时是建在业务池上的进程内实现。
// 跨进程测试用它把 core 的路由接到一个指向 httptest 库存服务的 HTTP 实现上。
func WithInventory(inv inventory.Service) RouterOption {
	return func(o *routerOptions) { o.inventory = inv }
}

// runInventory 是 KEEL_ROLE=inventory 的整条启动路径。
//
// 它不建业务库的池、不跑租户自检、不起协调器、不跑后台任务：这些都属于 core。
// 它只认库存库 —— KEEL_INVENTORY_DSN，没配时回落到 PG* 拼出来的那个 DSN
// （两库合一的拆分部署，或本机试跑）。
func runInventory(ctx context.Context, s SplitConfig, listen func(addr string, h http.Handler) error) error {
	dsn := s.InventoryDSN
	if dsn == "" {
		dsn = db.DSN()
	}
	inv, err := db.NewPoolFromDSN(ctx, dsn)
	if err != nil {
		return fmt.Errorf("建库存连接池失败: %w", err)
	}
	defer inv.Close()

	// 跨 0 通知（stock_msg.go）：配了 KEEL_CORE_URL 才起自己的协调器。它只注册一个分支 —— 回查，
	// 回答「本地事务提交了没有」，所以必须在本进程；投递目标是 core 的内网分支。
	var notifier *inventory.StockNotifier
	var quotaSrc inventory.QuotaSource
	if s.CoreURL != "" {
		c, err := rpc.NewClient(s.CoreURL, s.InternalSecret, 0)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvCoreURL, err)
		}
		quotaSrc = inventory.NewRemoteQuotaSource(c)
		res, err := dtm.NewBranchResolver(s.CoreURL, s.InternalSecret)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvCoreURL, err)
		}
		notifier = inventory.NewStockNotifier(repository.NewInventoryStore(inv), res.BranchURL(inventory.BranchStockChanged))
		tc, err := dtm.Start(s.DTMDSN, 0, map[string]dtm.BranchFunc{inventory.BranchStockMsgQuery: notifier.QueryBranch()})
		if err != nil {
			return fmt.Errorf("启动库存进程的事务协调器失败（%s）: %w", EnvDTMDSN, err)
		}
		defer tc.Close()
		notifier.Attach(tc)
	} else {
		slog.WarnContext(ctx, "没有配 "+EnvCoreURL+"：可售数跨 0 时不通知 core，商品列表的有货排序只靠 core 的全量刷新（"+
			EnvStockFlagInterval+"，默认 1 小时）；活动配额的同步消息读不到 core 的定义、会一直重试，"+
			"配额只在活动上线那一刻由 core 直接同步")
	}

	slog.InfoContext(ctx, "以 "+EnvRole+"=inventory 启动：只监听内网服务，没有公网接口与后台任务",
		"internal_addr", s.InternalAddr, "version", buildinfo.String(), "stock_notify", notifier != nil)
	return listen(s.InternalAddr, internalRouter(s, inv, internalExtras{notifier: notifier, quotaSrc: quotaSrc}))
}

// serveBoth 同时监听公网与内网两个端口，任何一个返回就返回。
//
// 用同一个可注入的 listen 起两个服务，好让 Run 的测试看得见内网那一个。
// 任何一个先退出都让 Run 返回，而不是只剩半个进程继续跑：公网挂了而内网
// 还活着，编排系统探内网端口会以为一切正常；反过来 core 的 SAGA 分支就没人接。
// 进程退出由 main 完成，另一个服务随进程一起结束。
func serveBoth(listen func(addr string, h http.Handler) error,
	publicAddr string, public http.Handler, internalAddr string, internal http.Handler) error {
	errc := make(chan error, 2)
	go func() {
		if err := listen(internalAddr, internal); err != nil {
			errc <- fmt.Errorf("内网服务（%s）: %w", internalAddr, err)
			return
		}
		errc <- nil
	}()
	go func() { errc <- listen(publicAddr, public) }()
	return <-errc
}
