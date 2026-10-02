package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 存量迁移：把一个 driver 上的上传文件搬到另一个 driver（通常是本地磁盘 → 对象存储），cmd/keel-uploads migrate 调它。
//
// 一行的步骤：从旧 driver 读进来 → 与库里记的 sha256、大小比对 → 对上了才按**同一个 storage_key** 写进新 driver →
// 把 uploads.driver 改过去（条件带旧 driver，两个迁移进程同时跑也不会改两遍）。
//
// 性质：
//   - **不停服**：迁移期间应用按行的 driver 读（UploadStorage），改指之前读旧的、之后读新的，任何时刻都读得到。
//   - **可以随时中断、重跑**：每一行独立提交；重跑时已经搬过的那些 driver 已经变了，不会再被扫到。
//   - **校验不过不搬**：字节对不上（磁盘坏块、半个文件、被人改过）就不往新 driver 写，行留在旧 driver 上、计进 Mismatch。
//   - 旧文件默认留着（DeleteSource 才删）：先迁、验、观察几天，再清。缩略图不迁，新 driver 上按需重算。
type UploadMigrator struct {
	repo UploadMigrateRepository
	from UploadStore
	to   ObjectWriter
	log  *slog.Logger
}

// UploadMigrateRepository 是迁移要的仓储能力。
type UploadMigrateRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	AllMerchantIDs(ctx context.Context) ([]int64, error)
}

// ObjectWriter 是迁移目标要有的能力：按给定的 key 写（而不是像 Put 那样生成新 key）。
type ObjectWriter interface {
	UploadStore
	PutObject(key string, r io.Reader, size int64) error
}

var _ ObjectWriter = (*S3Store)(nil)

// UploadMigrateOptions 是一次迁移的选项。
type UploadMigrateOptions struct {
	Merchant     int64 // 0 = 全部商家（含停用的：它们的文件同样要搬）
	DryRun       bool  // 只数、只检查源文件在不在，不写不改
	DeleteSource bool  // 改指之后删掉旧 driver 上的文件（含它的缩略图缓存）
	Batch        int32 // 每页几行，0 用 200
}

// UploadMigrateReport 是一次迁移的结果。
type UploadMigrateReport struct {
	Scanned  int // 扫到的、还在旧 driver 上的行
	Moved    int // 搬完并改指的
	Raced    int // 改指时发现已经不在旧 driver 上了（另一个迁移进程搬走了）
	Missing  int // 旧 driver 上没有这个文件（库与存储不一致，要人看）
	Mismatch int // 字节对不上库里的 sha256 / 大小
	Failed   int // 其它错误（网络、权限），重跑会再试
}

// NewUploadMigrator 建迁移器。
func NewUploadMigrator(repo UploadMigrateRepository, from UploadStore, to ObjectWriter, log *slog.Logger) (*UploadMigrator, error) {
	if from.Driver() == to.Driver() {
		return nil, fmt.Errorf("源和目标是同一个 driver（%d）", from.Driver())
	}
	if log == nil {
		log = slog.Default()
	}
	return &UploadMigrator{repo: repo, from: from, to: to, log: log}, nil
}

// Run 跑一遍。单行的失败计进报告、接着搬下一行；只有拿不到商家列表这类全局错误才返回 error。
func (m *UploadMigrator) Run(ctx context.Context, opt UploadMigrateOptions) (UploadMigrateReport, error) {
	if opt.Batch <= 0 {
		opt.Batch = 200
	}
	merchants := []int64{opt.Merchant}
	if opt.Merchant == 0 {
		var err error
		if merchants, err = m.repo.AllMerchantIDs(ctx); err != nil {
			return UploadMigrateReport{}, err
		}
	}
	var rep UploadMigrateReport
	for _, mid := range merchants {
		if err := m.merchant(tenant.NewContext(ctx, mid), mid, opt, &rep); err != nil {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			m.log.ErrorContext(ctx, "迁移这家店的上传文件中途失败，重跑会接着搬", "merchant_id", mid, "err", err)
			rep.Failed++
		}
	}
	return rep, nil
}

func (m *UploadMigrator) merchant(ctx context.Context, mid int64, opt UploadMigrateOptions, rep *UploadMigrateReport) error {
	var after int64
	for {
		var page []repository.StoredUpload
		if err := m.repo.WithTenant(ctx, func(tx repository.Tx) error {
			var err error
			page, err = tx.UploadsByDriver(ctx, m.from.Driver(), after, opt.Batch)
			return err
		}); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, u := range page {
			after = u.ID
			rep.Scanned++
			m.one(ctx, mid, u, opt, rep)
		}
	}
}

func (m *UploadMigrator) one(ctx context.Context, mid int64, u repository.StoredUpload, opt UploadMigrateOptions, rep *UploadMigrateReport) {
	log := m.log.With("merchant_id", mid, "upload_id", u.ID, "storage_key", u.StorageKey)
	rc, err := m.from.Open(u.StorageKey)
	if errors.Is(err, ErrUploadBlobMissing) {
		rep.Missing++
		log.ErrorContext(ctx, "源 driver 上没有这个文件（库里有记录、存储里没有字节），没搬，要人看")
		return
	}
	if err != nil {
		rep.Failed++
		log.ErrorContext(ctx, "读源文件失败", "err", err)
		return
	}
	defer rc.Close()
	if opt.DryRun {
		return
	}
	// 先读进来核对、再写：对不上的根本不往目标写，桶里不会出现坏的那份。上传有 10 MB 上限（MaxUploadBytes），
	// 读进内存是有界的；多读一个字节用来发现「文件比库里记的大」。
	data, err := io.ReadAll(io.LimitReader(rc, max(u.SizeBytes, MaxUploadBytes)+1))
	if err != nil {
		rep.Failed++
		log.ErrorContext(ctx, "读源文件失败", "err", err)
		return
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != u.SHA256 || int64(len(data)) != u.SizeBytes {
		rep.Mismatch++
		log.ErrorContext(ctx, "源文件的字节对不上库里记的 sha256 / 大小，没搬，这一行留在源 driver 上",
			"want_sha256", u.SHA256, "got_sha256", got, "want_size", u.SizeBytes, "got_size", len(data))
		return
	}
	if err := m.to.PutObject(u.StorageKey, bytes.NewReader(data), u.SizeBytes); err != nil {
		rep.Failed++
		log.ErrorContext(ctx, "写进目标 driver 失败", "err", err)
		return
	}
	var moved bool
	if err := m.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		moved, err = tx.MoveUploadDriver(ctx, u.ID, m.from.Driver(), m.to.Driver())
		return err
	}); err != nil {
		rep.Failed++
		log.ErrorContext(ctx, "改指失败（字节已经在目标上，重跑会覆盖写一遍再改指）", "err", err)
		return
	}
	if !moved {
		rep.Raced++
		return
	}
	rep.Moved++
	if opt.DeleteSource {
		if err := m.from.Remove(u.StorageKey); err != nil {
			log.WarnContext(ctx, "已改指，但删源文件失败（不影响读，只是多占一份空间）", "err", err)
		}
	}
}
