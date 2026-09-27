package app

// 拆分部署的配置与装配（docs/电商系统-微服务拆分方案.md 阶段 0）。
//
// 单独一个文件，是因为这一块在阶段 1 会长：库存服务的路由、core 侧的远端客户端、
// SAGA 分支地址都从这里接进去。app.go 里的 Run 只多了几行调用。

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/buildinfo"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/rpc"
)

// EnvRole 选部署形态里本进程的角色。见 Role。
const EnvRole = "KEEL_ROLE"

// 拆分部署用的其余几个变量，名字定义在各自的包里（它们的错误信息也在那儿），
// 这里重新导出，好让 app 的测试与文档检查只认一个地方。
const (
	EnvInventoryDSN   = db.EnvInventoryDSN
	EnvInternalAddr   = rpc.EnvInternalAddr
	EnvInternalSecret = rpc.EnvInternalSecret
	EnvInventoryURL   = rpc.EnvInventoryURL
)

// Role 是本进程在部署里扮演的角色。**同一个二进制**，靠它决定起哪些东西。
//
//	all（默认）  单体：公网 API + 全部后台任务 + 协调器，库存在进程内。今天的形态。
//	core         拆分形态里的主服务。阶段 0 与 all 完全相同；阶段 1 起库存调用改走
//	             KEEL_INVENTORY_URL，那时它会要求配这个地址。
//	inventory    拆分形态里的库存服务。只起内网服务（KEEL_INTERNAL_ADDR）：
//	             /healthz、/version、/readyz 与 /internal/v1/...；
//	             不挂任何公网业务路由，不跑任何后台任务，也不起事务协调器
//	             （协调器在 core，库存分支是被它远程调用的一方）。
//	             **阶段 0 里它的 /internal/v1 下还是空的**，库存接口与分支在阶段 1 挂上。
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
	InventoryURL   string
}

func splitConfigFromEnv() SplitConfig {
	return SplitConfig{
		Role:           roleFromEnv(),
		InventoryDSN:   strings.TrimSpace(os.Getenv(EnvInventoryDSN)),
		InternalAddr:   strings.TrimSpace(os.Getenv(EnvInternalAddr)),
		InternalSecret: os.Getenv(EnvInternalSecret),
		InventoryURL:   strings.TrimSpace(os.Getenv(EnvInventoryURL)),
	}
}

// validate 只查配置本身，不碰网络与数据库 —— 所以它排在 Run 的最前面，
// 一份注定要被拒绝的配置不该先去建连接池。
func (s SplitConfig) validate() error {
	switch s.Role {
	case RoleAll, RoleCore, RoleInventory:
	default:
		return fmt.Errorf("%s=%q 不认识，只能是 all（默认）/ core / inventory", EnvRole, string(s.Role))
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
	if (s.InternalAddr != "" || s.InventoryURL != "") && len(s.InternalSecret) < rpc.MinSecretLen {
		// 内网服务不验签就是一个对任意租户开放的写接口；远端客户端没有密钥就签不了名。
		// 两种情况都不是「降级能跑」，所以拒绝启动。
		return fmt.Errorf("配了 %s 或 %s 就必须配 %s（至少 %d 字节，所有进程同一个值；"+
			"例如 openssl rand -base64 48）", EnvInternalAddr, EnvInventoryURL,
			EnvInternalSecret, rpc.MinSecretLen)
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

// internalRouter 建内网引擎。阶段 0 只有探针；/readyz 查的是库存池 ——
// 这是阶段 0 里库存池唯一的用户。阶段 1 在这里把库存接口挂到 routes.Tenant、
// 把库存分支经 dtm.MountBranches 挂到 routes.Saga。
func internalRouter(s SplitConfig, inv *pgxpool.Pool) *gin.Engine {
	r, _ := rpc.NewRouter(rpc.ServerConfig{Secret: s.InternalSecret, Ready: inv.Ping})
	return r
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

	slog.InfoContext(ctx, "以 "+EnvRole+"=inventory 启动：只监听内网服务，没有公网接口与后台任务",
		"internal_addr", s.InternalAddr, "version", buildinfo.String())
	return listen(s.InternalAddr, internalRouter(s, inv))
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
