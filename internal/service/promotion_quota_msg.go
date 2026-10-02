package service

// 活动配额同步的发送方：写活动的本地事务里把版本 +1、读出定义，连同定义登记一条 dtmrs 二阶段消息；库存服务收到后
// 按版本只接受更新的那份、整组设配额（接收方在 inventory 包 activity_msg.go）。约定见 docs/电商系统-总体架构.md
// 「派生数据同步约定」；为什么定义随载荷来而不是让库存回 core 读，见 docs/电商系统-微服务部署方案.md 4.1（下层不调上层）。
//
// 这一侧有三样东西：
//
//   - QuotaSync：登记 / 提交 / 作废消息，外加回查分支（promotion_quota_msg_query，永远在 core 这一侧 ——
//     它回答「写活动的那个本地事务提交了没有」，屏障记在业务库；嵌入式协调器 local://，独立协调器经 core 内网回调）；
//   - PromotionQuotaSource：inventory.QuotaSource 的 core 实现，只剩单体用（升级前登记、没有载荷的旧消息还能回源读）；
//   - 目标地址：单体 local://inventory_activity_sync，拆分 <KEEL_INVENTORY_URL>/internal/v1/saga/…（dtm.BranchResolver）。

import (
	"context"
	"encoding/json"
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
	query  string
	tc     atomic.Value // quotaCoord
}

type quotaCoord struct{ c dtm.Coordinator }

// NewQuotaSync 建发送方。repo 是业务库上的仓储（回查屏障记在那里），res 决定接收分支的地址。
// 协调器 Start 之后再 Attach —— 回查分支要在 Start 之前注册，与 StockNotifier 同一个环。
// res 解析库存服务的接收分支（单体 local://，拆分 http://）；self 解析本服务自己的回查分支（嵌入式协调器 local://，
// 独立协调器时是本服务内网上的 HTTP 地址）。
func NewQuotaSync(repo *repository.Repo, res, self dtm.BranchResolver) *QuotaSync {
	return &QuotaSync{repo: repo, action: res.BranchURL(inventory.BranchActivitySync),
		query: self.BranchURL(BranchPromotionQuotaQuery)}
}

// Attach 接上已经启动的协调器。
func (q *QuotaSync) Attach(tc dtm.Coordinator) { q.tc.Store(quotaCoord{tc}) }

func (q *QuotaSync) coord() dtm.Coordinator {
	b, _ := q.tc.Load().(quotaCoord)
	return b.c
}

func (q *QuotaSync) ready() bool { return q != nil && q.coord() != nil }

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
	payload, err := quotaPayload(ctx, tx, promotionID)
	if err != nil {
		return "", false, err
	}
	tc := q.coord()
	if err := tc.PrepareMsgEx(gid, []string{q.action}, []string{payload}, q.query, quotaMsgGraceSecs, false); err != nil {
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
	tc := q.coord()
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
// quotaPayload 在写活动的同一个事务里把版本 +1、读出当前定义，编成消息载荷（00240，部署方案 4.1）。
// 库存服务按版本只接受更新的那份，不再回 core 读——那是下层调上层。
func quotaPayload(ctx context.Context, tx repository.Tx, promotionID int64) (string, error) {
	rev, found, err := tx.BumpPromotionQuotaRev(ctx, promotionID)
	if err != nil {
		return "", err
	}
	p := inventory.QuotaSyncPayload{PromotionID: promotionID, Rev: rev, Found: found, Items: []inventory.QuotaSyncItem{}, Keep: []int64{}}
	if found {
		_, items, err := tx.PromotionQuotaDefinition(ctx, promotionID)
		if err != nil {
			return "", err
		}
		for _, it := range items {
			if it.Quota == nil {
				p.Keep = append(p.Keep, it.SKUID)
				continue
			}
			p.Items = append(p.Items, inventory.QuotaSyncItem{SKUID: it.SKUID, Quota: *it.Quota})
		}
	}
	b, err := json.Marshal(p)
	return string(b), err
}

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
