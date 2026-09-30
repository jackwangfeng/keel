package service

// 活动配额同步的发送方：写活动的本地事务里登记一条 dtmrs 二阶段消息，库存服务收到后回源读 core 的定义、
// 整组设配额（接收方在 inventory 包 activity_msg.go）。约定见 docs/电商系统-总体架构.md「派生数据同步约定」，
// 为什么改、改了之后各条路径怎么走见 admin_promotion.go 的文件头。
//
// 这一侧有三样东西：
//
//   - QuotaSync：登记 / 提交 / 作废消息，外加回查分支（local://promotion_quota_msg_query，永远在 core 进程里 ——
//     它回答「写活动的那个本地事务提交了没有」，屏障记在业务库）；
//   - PromotionQuotaSource：inventory.QuotaSource 的 core 实现，接收方回源读的就是它（单体进程内直接调，
//     拆分时挂在 core 的内网端口上，inventory.MountQuotaSource）；
//   - 目标地址：单体 local://inventory_activity_sync，拆分 <KEEL_INVENTORY_URL>/internal/v1/saga/…（dtm.BranchResolver）。

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// BranchPromotionQuotaQuery 是配额同步消息的回查分支名。
const BranchPromotionQuotaQuery = "promotion_quota_msg_query"

const (
	// quotaMsgGraceSecs：prepare 之后多久开始回查。写活动的事务在登记之后只剩几条语句与一次提交。
	quotaMsgGraceSecs = 30
	// quotaMsgWaitMS：提交之后等投递结果的上限。等得到（库存服务在）时后台接口的回显就是同步之后的配额，
	// 与改成消息之前的体验一样；等不到（库存服务不在）也照样回成功 —— 消息保证最终送到，这里只是不让
	// 运营在正常情况下看到一个「还没同步」的回显。
	quotaMsgWaitMS = 2000
)

// QuotaSync 是配额同步消息的发送方。零值不可用；nil 表示没接协调器（部分测试的装配），
// AdminPromotionService 此时退回改成消息之前的直接调用。
type QuotaSync struct {
	repo   *repository.Repo
	action string
	tc     atomic.Pointer[dtm.TC]
}

// NewQuotaSync 建发送方。repo 是业务库上的仓储（回查屏障记在那里），res 决定接收分支的地址。
// 协调器 Start 之后再 Attach —— 回查分支要在 Start 之前注册，与 StockNotifier 同一个环。
func NewQuotaSync(repo *repository.Repo, res dtm.BranchResolver) *QuotaSync {
	return &QuotaSync{repo: repo, action: res.BranchURL(inventory.BranchActivitySync)}
}

// Attach 接上已经启动的协调器。
func (q *QuotaSync) Attach(tc *dtm.TC) { q.tc.Store(tc) }

func (q *QuotaSync) ready() bool { return q != nil && q.tc.Load() != nil }

// QueryBranch 是回查分支：写活动的本地事务提交了没有。
func (q *QuotaSync) QueryBranch() dtm.BranchFunc {
	return func(gid, branchID, op string) int {
		_, err := inventory.ParseActivityMsgGID(gid)
		var ctx context.Context
		if err == nil {
			ctx, _, err = dtm.TenantContextFromTenantGID(context.Background(), inventory.ActivityMsgGIDPrefix, gid)
		}
		if err != nil {
			slog.Error("配额同步消息的回查拿到的 gid 不成立，按未提交作废", "gid", gid, "err", err)
			return dtm.Failure
		}
		committed, err := q.repo.QueryPreparedMsg(ctx, gid)
		if err != nil {
			slog.Warn("配额同步消息的回查失败，协调器会再问", "gid", gid, "err", err)
			return dtm.Unknown
		}
		if !committed {
			return dtm.Failure
		}
		return dtm.Success
	}
}

// prepare 在调用方的 core 事务里登记一条配额同步消息并占下回查屏障。lost 为真表示回查已经抢先把它判成了
// 「没提交」（登记之后 30 秒这个事务还没走到这里）：事务照常提交，调用方在提交之后 resync 补一条。
func (q *QuotaSync) prepare(ctx context.Context, tx repository.Tx, merchantID, promotionID int64) (gid string, lost bool, err error) {
	gid, err = inventory.ActivityMsgGID(merchantID, promotionID)
	if err != nil {
		return "", false, err
	}
	tc := q.tc.Load()
	if err := tc.PrepareMsg(gid, []string{q.action}, "local://"+BranchPromotionQuotaQuery, quotaMsgGraceSecs); err != nil {
		return "", false, err
	}
	ok, err := tx.MarkMsgPrepared(ctx, gid)
	if err != nil {
		_ = tc.AbortMsg(gid)
		return "", false, err
	}
	if !ok {
		return "", true, nil
	}
	return gid, false, nil
}

// finish 在事务结束之后提交（并短暂等投递）或作废消息。都是尽力而为：提交失败有回查兜底，作废失败回查会判成未提交。
func (q *QuotaSync) finish(ctx context.Context, gid string, committed bool) {
	if gid == "" || !q.ready() {
		return
	}
	tc := q.tc.Load()
	if !committed {
		if err := tc.AbortMsg(gid); err != nil {
			slog.DebugContext(ctx, "作废配额同步消息失败，交给回查", "gid", gid, "err", err)
		}
		return
	}
	if err := tc.SubmitMsg(gid); err != nil {
		slog.WarnContext(ctx, "配额同步消息提交失败，回查会接着投递", "gid", gid, "err", err)
		return
	}
	if st, err := tc.WaitFinal(gid, quotaMsgWaitMS); err != nil || st != "succeed" {
		slog.InfoContext(ctx, "配额同步消息还没投递完（库存服务不在？），协调器会重试到送达", "gid", gid, "status", st, "err", err)
	}
}

// resync 在一个只登记消息的小事务里补发一条（「以 core 当前定义为准」，所以补发不需要知道上一条说了什么）。
func (q *QuotaSync) resync(ctx context.Context, merchantID, promotionID int64) {
	if !q.ready() {
		return
	}
	var gid string
	err := q.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		gid, _, e = q.prepare(ctx, tx, merchantID, promotionID)
		return e
	})
	q.finish(ctx, gid, err == nil)
	if err != nil {
		slog.ErrorContext(ctx, "补发配额同步消息失败（库存对账会报出差异）", "promotion_id", promotionID, "err", err)
	}
}

// PromotionQuotaSource 是 inventory.QuotaSource 的 core 实现：一场活动当前的配额定义（promotion_skus.quota_qty，00180）。
type PromotionQuotaSource struct{ repo CouponRepository }

func NewPromotionQuotaSource(r CouponRepository) *PromotionQuotaSource {
	return &PromotionQuotaSource{repo: r}
}

var _ inventory.QuotaSource = (*PromotionQuotaSource)(nil)

func (s *PromotionQuotaSource) QuotaDefinition(ctx context.Context, promotionID int64) (inventory.QuotaDefinition, error) {
	var d inventory.QuotaDefinition
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		found, items, err := tx.PromotionQuotaDefinition(ctx, promotionID)
		if err != nil {
			return err
		}
		d.Found = found
		for _, it := range items {
			if it.Quota == nil {
				d.Keep = append(d.Keep, it.SKUID)
				continue
			}
			d.Items = append(d.Items, inventory.ActivityQuota{SKUID: it.SKUID, Quota: *it.Quota})
		}
		return nil
	})
	return d, err
}
