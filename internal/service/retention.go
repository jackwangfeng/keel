package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/tenant"
)

// 只增不删的表按保留期分批清理（2026-09-30 架构审查）。
//
// 审查时核实的现状：00012 的注释说幂等存档「清理走 jobs」，全仓没有任何按 expire_at 的清理；
// search_logs、inventory_logs、agent_tool_calls 都没有保留期。它们每一张都随流量线性增长，
// 而且都有按 (merchant_id, 时间) 的索引 —— 不删，索引与表一起膨胀，按时间窗口的读
// （报表、检索指标、补货的断货天数）也跟着变慢。
//
// # 删什么、留多久
//
//	表                   判据                        默认      变量
//	idempotency_keys     expire_at < 现在            到期即删  —（expire_at 本身就是保留期）
//	search_logs          created_at < 现在 - 保留期  90 天     KEEL_RETENTION_SEARCH_LOG_DAYS
//	agent_tool_calls     created_at < 现在 - 保留期  180 天    KEEL_RETENTION_AGENT_TOOL_CALL_DAYS
//	inventory_logs       created_at < 现在 - 保留期  180 天    KEEL_RETENTION_INVENTORY_LOG_DAYS
//
// 天数配 0 = 这张表不清。inventory_logs 的保留期有下限上的讲究（退款回补按流水判重放），
// 见 db/queries/inventory_svc.sql 的 InvPurgeLogsBefore。
//
// **barrier 表（两个库各一张）这一轮不删。** 屏障行没有「全局事务结束了没有」这一列：
// 判据在协调器自己的存储里（KEEL_DTM_DSN，另一个库，可能是 sqlite），这里 JOIN 不到。
// 只按 create_time 删的话，一个还在重试的全局事务（分支一直失败、协调器按退避一直重试）
// 的正向屏障行被删掉之后，补偿来了会插进一行「正向没发生过」—— 判成空回滚，扣掉的库存
// 再也不回补。另外 keel_app 在 barrier 上只有 INSERT（00008）。要做得先有「已结束的 gid」
// 这份清单（向协调器查），再用一个显式授了 DELETE 的角色删。
//
// # 怎么删
//
// 与超时关单、孤儿回收同一套跨租户做法（repository/sweep.go 文件头）：读商家清单，逐家进
// 租户事务。每批一个短事务、至多 Batch 行（带 LIMIT 的 ctid 子查询，db/queries/retention.sql）；
// 一家店一张表一轮至多 MaxBatches 批，删不完的留给下一轮 —— 积压了几年的库第一次上线这个
// 任务时，不会有一轮跑几个小时、把 WAL 和复制延迟一起打满。
//
// 商家清单取**全部**商家（含停用与软删，repository.AllMerchantIDs）：保留期对停用的店同样成立。
// 平台作用域的幂等存档（merchant_id IS NULL，00028）单独在平台作用域里删一次。
type RetentionService struct {
	repo RetentionRepository
	inv  InventoryLogPurger
	cfg  RetentionConfig
	log  *slog.Logger
	now  func() time.Time
}

// RetentionRepository 是这个任务需要的仓储能力（repository.Repo 满足）。
// 每个 Purge 方法自己开一个事务、只删一批，租户从 ctx 取。
type RetentionRepository interface {
	AllMerchantIDs(ctx context.Context) ([]int64, error)
	PurgeExpiredIdempotencyKeys(ctx context.Context, before time.Time, batch int32) (int64, error)
	PurgeExpiredPlatformIdempotencyKeys(ctx context.Context, before time.Time, batch int32) (int64, error)
	PurgeSearchLogs(ctx context.Context, before time.Time, batch int32) (int64, error)
	PurgeAgentToolCalls(ctx context.Context, before time.Time, batch int32) (int64, error)
	PurgeSearchJudgments(ctx context.Context, before time.Time, batch int32) (int64, error)
}

// InventoryLogPurger 删库存流水（repository.InventoryStore 满足）。可以为 nil：
// 拆分部署下 core 进程握不到库存库，这一张由拥有库存的进程清。
type InventoryLogPurger interface {
	PurgeInventoryLogs(ctx context.Context, before time.Time, batch int32) (int64, error)
}

// RetentionConfig 是保留期与节流参数。零值字段在 NewRetentionService 里补默认值，
// 保留期除外 —— 保留期的 0 表示「不清」，所以默认值由 DefaultRetentionConfig / 环境变量给。
type RetentionConfig struct {
	Interval   time.Duration // 一轮的间隔，默认 1 小时
	Batch      int32         // 每批至多删几行，默认 5000
	MaxBatches int           // 一家店一张表一轮至多几批，默认 20

	SearchLogs      time.Duration // 0 = 不清
	SearchJudgments time.Duration // 相关度预判（00230）太久没刷新的
	AgentToolCalls  time.Duration
	InventoryLogs   time.Duration
}

const (
	DefaultRetentionInterval   = time.Hour
	DefaultRetentionBatch      = 5000
	DefaultRetentionMaxBatches = 20

	EnvRetentionInterval           = "KEEL_RETENTION_INTERVAL"
	EnvRetentionSearchLogDays      = "KEEL_RETENTION_SEARCH_LOG_DAYS"
	EnvRetentionSearchJudgmentDays = "KEEL_RETENTION_SEARCH_JUDGMENT_DAYS"
	EnvRetentionAgentToolCallDays  = "KEEL_RETENTION_AGENT_TOOL_CALL_DAYS"
	EnvRetentionInventoryLogDays   = "KEEL_RETENTION_INVENTORY_LOG_DAYS"
	defaultSearchLogRetention      = 90 * 24 * time.Hour
	// 预判默认 14 天重判一次（SearchJudgeConfig.FreshFor），30 天还没刷新说明那条查询已经不热了。
	defaultSearchJudgmentRetention = 30 * 24 * time.Hour
	defaultAgentToolCallRetention  = 180 * 24 * time.Hour
	defaultInventoryLogRetention   = 180 * 24 * time.Hour
	minInventoryLogRetentionInDays = 30
)

// DefaultRetentionConfig 是不配任何环境变量时的保留期：检索日志 90 天，工具调用与库存流水 180 天。
func DefaultRetentionConfig() RetentionConfig {
	return RetentionConfig{
		Interval:        DefaultRetentionInterval,
		Batch:           DefaultRetentionBatch,
		MaxBatches:      DefaultRetentionMaxBatches,
		SearchLogs:      defaultSearchLogRetention,
		SearchJudgments: defaultSearchJudgmentRetention,
		AgentToolCalls:  defaultAgentToolCallRetention,
		InventoryLogs:   defaultInventoryLogRetention,
	}
}

// RetentionConfigFromEnv 在默认值上叠环境变量。写错了返回错误而不是悄悄用默认值：
// 保留期配错的后果是删多了（不可恢复）或一直不删，两种都不该等到出事才发现。
func RetentionConfigFromEnv() (RetentionConfig, error) {
	cfg := DefaultRetentionConfig()
	if v := strings.TrimSpace(os.Getenv(EnvRetentionInterval)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("%s=%q 不是正的时长（形如 1h、30m）", EnvRetentionInterval, v)
		}
		cfg.Interval = d
	}
	for _, c := range []struct {
		env string
		dst *time.Duration
		min int
	}{
		{EnvRetentionSearchLogDays, &cfg.SearchLogs, 0},
		{EnvRetentionSearchJudgmentDays, &cfg.SearchJudgments, 0},
		{EnvRetentionAgentToolCallDays, &cfg.AgentToolCalls, 0},
		{EnvRetentionInventoryLogDays, &cfg.InventoryLogs, minInventoryLogRetentionInDays},
	} {
		v := strings.TrimSpace(os.Getenv(c.env))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("%s=%q 不是非负整数天数（0 = 不清）", c.env, v)
		}
		if n > 0 && n < c.min {
			return cfg, fmt.Errorf("%s=%d 太短：至少 %d 天（理由见 db/queries/inventory_svc.sql 的 InvPurgeLogsBefore）",
				c.env, n, c.min)
		}
		*c.dst = time.Duration(n) * 24 * time.Hour
	}
	return cfg, nil
}

// NewRetentionService 建清理服务。inv 可以为 nil（不清库存流水）；log 为 nil 用 slog.Default()。
func NewRetentionService(repo RetentionRepository, inv InventoryLogPurger, cfg RetentionConfig,
	log *slog.Logger) *RetentionService {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultRetentionInterval
	}
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultRetentionBatch
	}
	if cfg.MaxBatches <= 0 {
		cfg.MaxBatches = DefaultRetentionMaxBatches
	}
	return &RetentionService{repo: repo, inv: inv, cfg: cfg, log: log, now: time.Now}
}

// WithClock 换掉时钟，**只给测试用**。
func (s *RetentionService) WithClock(now func() time.Time) *RetentionService {
	s.now = now
	return s
}

// RetentionReport 是一轮的结果：每张表删了几行、有几次失败（一次 = 一家店的一张表）。
type RetentionReport struct {
	Tenants int
	Deleted map[string]int64
	Failed  int
	// Capped 是「删满了 MaxBatches 批、还没删完」的次数：非零说明有积压，下一轮接着删。
	Capped int
}

// Run 按 Interval 一轮一轮地跑，直到 ctx 被取消。启动后立刻跑一轮，单轮出错不退出。
func (s *RetentionService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		rep, err := s.RunOnce(ctx)
		switch {
		case err != nil:
			s.log.ErrorContext(ctx, "保留期清理这一轮没跑起来", "err", err)
		case rep.Failed > 0:
			s.log.WarnContext(ctx, "保留期清理完成一轮，有失败", "tenants", rep.Tenants,
				"deleted", rep.Deleted, "failed", rep.Failed, "capped", rep.Capped)
		default:
			s.log.InfoContext(ctx, "保留期清理完成一轮", "tenants", rep.Tenants,
				"deleted", rep.Deleted, "capped", rep.Capped)
		}
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "保留期清理任务收到停止信号，退出")
			return
		case <-t.C:
		}
	}
}

type purgeFn func(ctx context.Context, before time.Time, batch int32) (int64, error)

// RunOnce 跑一轮。导出的理由同 SweepOnce：测试要能不等 ticker 驱动它。
// 返回的 error 只表示「这一轮没跑起来」（读不到商家清单）；单家店单张表的失败计进 Failed、
// 打日志，不影响别的店与别的表。
func (s *RetentionService) RunOnce(ctx context.Context) (RetentionReport, error) {
	merchants, err := s.repo.AllMerchantIDs(ctx)
	if err != nil {
		return RetentionReport{}, err
	}
	now := s.now()
	rep := RetentionReport{Tenants: len(merchants), Deleted: map[string]int64{}}

	type target struct {
		name   string
		fn     purgeFn
		before time.Time
	}
	var targets []target
	targets = append(targets, target{"idempotency_keys", s.repo.PurgeExpiredIdempotencyKeys, now})
	if s.cfg.SearchLogs > 0 {
		targets = append(targets, target{"search_logs", s.repo.PurgeSearchLogs, now.Add(-s.cfg.SearchLogs)})
	}
	if s.cfg.SearchJudgments > 0 {
		targets = append(targets, target{"search_relevance_judgments", s.repo.PurgeSearchJudgments, now.Add(-s.cfg.SearchJudgments)})
	}
	if s.cfg.AgentToolCalls > 0 {
		targets = append(targets, target{"agent_tool_calls", s.repo.PurgeAgentToolCalls, now.Add(-s.cfg.AgentToolCalls)})
	}
	if s.inv != nil && s.cfg.InventoryLogs > 0 {
		targets = append(targets, target{"inventory_logs", s.inv.PurgeInventoryLogs, now.Add(-s.cfg.InventoryLogs)})
	}

	for _, m := range merchants {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		tctx := tenant.NewContext(ctx, m)
		for _, tg := range targets {
			s.drain(tctx, &rep, tg.name, tg.fn, tg.before, "merchant_id", m)
		}
	}
	// 平台作用域那一抽屉的幂等存档：不属于任何一家店，单独删一次。
	s.drain(ctx, &rep, "idempotency_keys", s.repo.PurgeExpiredPlatformIdempotencyKeys, now, "scope", "platform")
	return rep, nil
}

// drain 对一张表一批一批地删，直到一批不满（删完了）或删满 MaxBatches 批。
func (s *RetentionService) drain(ctx context.Context, rep *RetentionReport, name string, fn purgeFn,
	before time.Time, logArgs ...any) {
	for i := 0; i < s.cfg.MaxBatches; i++ {
		n, err := fn(ctx, before, s.cfg.Batch)
		rep.Deleted[name] += n
		if err != nil {
			rep.Failed++
			s.log.ErrorContext(ctx, "保留期清理一批失败", append([]any{"table", name, "err", err}, logArgs...)...)
			return
		}
		if n < int64(s.cfg.Batch) {
			return
		}
	}
	rep.Capped++
}
