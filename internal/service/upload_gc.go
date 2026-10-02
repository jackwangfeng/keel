package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 孤儿上传文件回收（数据模型 §13「孤儿回收的安全条件」）。
//
// uploads 里 referenced = FALSE、创建超过 24 小时的记录：传了凭证没提交售后、传了商品图
// 没挂到商品上、换头像换下来的旧头像（PATCH /me 取消引用）。删存储里的文件，再删记录。
// 买家上传与后台上传一视同仁 —— 判据只有 referenced 与创建时间，不看是谁传的。
//
// # 24 小时
//
// 给「传了但还没点提交」的人留的余量（§13 原话）。写死而不是店铺设置：它不是经营口径，
// 是「上传与引用之间最长隔多久」的工程假设，商家没有理由去调它。
//
// # 与「提交售后时标记引用」赛跑
//
// 安全条件是 §13 那一句：referenced 必须在引用它的业务对象落库的**同一个事务**里置 TRUE
// （claimEvidence / ReplaceProductImages / SKU 小图 / 头像都是如此）。回收这一侧的配合是：
// 删记录那条 DELETE 在谓词里把「没被引用、够老」再判一遍（DeleteOrphanUpload），不是按 id 删。
// 两条语句撞在同一行上时谁先拿到行锁谁赢，两种结局都不留下坏数据：
//
//   - 标引用先拿到：DELETE 等它提交后按新版本重判，referenced 已是 TRUE，0 行，文件留着；
//   - DELETE 先拿到并提交：标引用那条 UPDATE 影响 0 行，售后申请以「凭证不存在」失败（422），
//     而不是落库一张指着 404 图片的申请。
//
// # 先删记录、提交，再删文件
//
// 反过来（先删文件再提交）的话，提交失败时记录还在、文件没了 —— 而那条记录此刻仍然可以被
// 引用（行锁随回滚释放），引用它的那张售后单从此指着一个 404。先提交再删文件，最坏的结局是
// 文件删失败（磁盘只读、权限），留下一个谁也不认识的文件：浪费磁盘，不坏数据。
// 那种情况打一条 Error 日志，让人去看。
//
// # 跑法
//
// 与自动确认收货同一套：枚举活跃商家、逐家进租户事务（uploads 有 RLS，没有能跨租户的 DELETE），
// fairRound 分预算。一小时一轮：孤儿文件晚删一小时没有任何后果。
// §13 原稿说「挂在 jobs 上」，与自动确认同一处出入 —— 本仓库没有以平台身份出队的 worker
// （sweep.go 文件头），所以按租户扫描。
type UploadGCService struct {
	repo  UploadGCRepository
	store UploadStore
	log   *slog.Logger
	cfg   SweepConfig

	cursor uint64
	now    func() time.Time
}

// UploadOrphanGrace 是孤儿文件的宽限期：创建超过它仍没被引用才回收（数据模型 §13）。
const UploadOrphanGrace = 24 * time.Hour

// DefaultUploadGCInterval 一小时一轮。
const DefaultUploadGCInterval = time.Hour

// UploadGCRepository 是这个任务需要的仓储能力。
type UploadGCRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
}

// UploadGCReport 是一轮的结果。
type UploadGCReport struct {
	Tenants int
	// Deleted 记录与文件都删掉的条数。
	Deleted int
	// Raced 扫到了，但删的那一刻已经被引用了。正常路径。
	Raced int
	// FileErrors 记录删了、文件没删掉的条数（留下一个没人认识的文件）。非零要有人看。
	FileErrors int
	// Failed 删记录时出错。
	Failed   int
	Fallback bool
}

// NewUploadGCService 建孤儿回收服务。store 必须与写文件的是同一个 driver（同一个根目录）。
func NewUploadGCService(r UploadGCRepository, store UploadStore, cfg SweepConfig, log *slog.Logger) *UploadGCService {
	if log == nil {
		log = slog.Default()
	}
	if cfg.PerTenantCap <= 0 {
		cfg.PerTenantCap = DefaultPerTenantCap
	}
	if cfg.RoundBudget <= 0 {
		cfg.RoundBudget = DefaultRoundBudget
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultUploadGCInterval
	}
	return &UploadGCService{repo: r, store: store, log: log, cfg: cfg, now: time.Now}
}

// WithClock 换掉时钟，**只给测试用**。
func (s *UploadGCService) WithClock(now func() time.Time) *UploadGCService {
	s.now = now
	return s
}

// Run 按 Interval 一轮一轮地跑，直到 ctx 被取消。启动后立刻跑一轮，单轮出错不退出。
func (s *UploadGCService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		rep, err := s.CollectOnce(ctx)
		switch {
		case err != nil:
			s.log.ErrorContext(ctx, "孤儿文件回收这一轮没跑起来", "err", err)
		case rep.Deleted == 0 && rep.Raced == 0 && rep.Failed == 0 && rep.FileErrors == 0:
			s.log.DebugContext(ctx, "孤儿文件回收这一轮没有可删的", "tenants", rep.Tenants)
		default:
			s.log.InfoContext(ctx, "孤儿文件回收完成一轮", "tenants", rep.Tenants, "deleted", rep.Deleted,
				"raced", rep.Raced, "file_errors", rep.FileErrors, "failed", rep.Failed, "fallback", rep.Fallback)
		}
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "孤儿文件回收任务收到停止信号，退出")
			return
		case <-t.C:
		}
	}
}

// CollectOnce 跑一轮。导出的理由同 SweepOnce：测试要能不等 ticker 驱动它。
func (s *UploadGCService) CollectOnce(ctx context.Context) (UploadGCReport, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return UploadGCReport{}, err
	}
	rep := UploadGCReport{Tenants: len(merchants)}
	if len(merchants) == 0 {
		return rep, nil
	}
	start := int(s.cursor % uint64(len(merchants)))
	s.cursor++
	rep.Fallback = fairRound(merchants, start, s.cfg.PerTenantCap, s.cfg.RoundBudget,
		func(merchantID int64, limit int) int {
			return s.collectTenant(ctx, merchantID, limit, &rep)
		})
	return rep, nil
}

func (s *UploadGCService) collectTenant(ctx context.Context, merchantID int64, limit int, rep *UploadGCReport) int {
	if limit <= 0 {
		return 0
	}
	tctx := tenant.NewContext(ctx, merchantID)
	log := s.log.With("merchant_id", merchantID)
	cutoff := s.now().Add(-UploadOrphanGrace)

	// 每个配了的 driver 各扫一遍（UploadStorage：换存储的过渡期里两个 driver 上都可能有孤儿），
	// 删文件用写它的那个 driver。预算按 driver 依次用，总数不超过 limit。
	done := 0
	for _, d := range storeDrivers(s.store) {
		if done >= limit {
			break
		}
		st, err := storeFor(s.store, d)
		if err != nil {
			log.ErrorContext(ctx, "孤儿回收拿不到 driver", "driver", d, "err", err)
			rep.Failed++
			continue
		}
		var due []repository.OrphanUpload
		if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
			var err error
			due, err = tx.ListOrphanUploads(tctx, d, cutoff, int32(limit-done))
			return err
		}); err != nil {
			log.ErrorContext(ctx, "扫描孤儿文件失败", "driver", d, "err", err)
			rep.Failed++
			continue
		}
		for _, u := range due {
			s.collectOne(tctx, log, st, u, cutoff, rep)
		}
		done += len(due)
	}
	return done
}

// collectOne 删一条：事务里条件删除记录（与引用赛跑的裁判），提交之后删文件。
func (s *UploadGCService) collectOne(ctx context.Context, log *slog.Logger, st UploadStore, u repository.OrphanUpload,
	cutoff time.Time, rep *UploadGCReport) {
	var key string
	var ok bool
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		key, ok, err = tx.DeleteOrphanUpload(ctx, u.ID, cutoff)
		return err
	}); err != nil {
		rep.Failed++
		log.ErrorContext(ctx, "删孤儿文件记录失败", "upload_id", u.ID, "err", err)
		return
	}
	if !ok {
		rep.Raced++
		log.InfoContext(ctx, "孤儿文件在删之前被引用了（多半是刚提交的售后申请），留着", "upload_id", u.ID)
		return
	}
	if err := st.Remove(key); err != nil {
		rep.FileErrors++
		log.ErrorContext(ctx, "孤儿文件的记录已删，存储里的文件没删掉 —— 留下一个没人认识的文件，请人工清理",
			"upload_id", u.ID, "storage_key", key, "err", err)
		return
	}
	rep.Deleted++
	log.InfoContext(ctx, "已回收孤儿文件", "upload_id", u.ID)
}
