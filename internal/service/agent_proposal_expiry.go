package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 提案过期（AI 经营 M9 任务 4）：待处理超过 expires_at（创建后 48 小时）的置 50 已过期，之后不能再批 ——
// 两天前的「这件三天后卖断」早就不是现在的事实了。执行中（15）的不动，它们在等结果写回。

// ProposalExpiryInterval 是扫描间隔。
const ProposalExpiryInterval = 10 * time.Minute

// ProposalExpiryRepository 是过期任务要的仓储能力。
type ProposalExpiryRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
}

// ExpireProposalsOnce 按商户逐个把过期的待处理提案置 50，返回总条数。
func ExpireProposalsOnce(ctx context.Context, repo ProposalExpiryRepository) (int64, error) {
	merchants, err := repo.ActiveMerchants(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, m := range merchants {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		err := repo.WithTenant(tenant.NewContext(ctx, m), func(tx repository.Tx) error {
			n, err := tx.ExpireAgentProposals(ctx)
			total += n
			return err
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// RunProposalExpiry 每 ProposalExpiryInterval 扫一轮，直到 ctx 结束。
func RunProposalExpiry(ctx context.Context, repo ProposalExpiryRepository, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(ProposalExpiryInterval)
	defer t.Stop()
	for {
		if n, err := ExpireProposalsOnce(ctx, repo); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "AI 员工提案过期扫描出错", "err", err)
		} else if n > 0 {
			log.InfoContext(ctx, "AI 员工提案过期", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
