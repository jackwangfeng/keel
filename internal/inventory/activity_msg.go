package inventory

// 活动配额同步的接收方（二阶段消息，docs/电商系统-总体架构.md「派生数据同步约定」；发送方是 core 的
// service/promotion_quota_msg.go）。
//
// core 在写活动的**同一个本地事务里**登记一条消息 promo-{商家}-{活动}-{随机串}；这里收到后**不信消息内容**
// （它也没有内容），回源读 core 里这场活动**当前**的配额定义（QuotaSource），在库存库里整组设成它。
// 于是：
//
//   - 重复投递：第一次设与子事务屏障同一个事务，第二次被判成重复；
//   - 乱序投递：两条消息都按「处理那一刻」core 的定义设；处理交错时（A 读到旧定义、B 读到新定义、B 先写、
//     A 后写）由复读核对纠正 —— 每一次写之后再读一次定义，变了就再设，与有货标记的 refreshStore 同一个办法。
//     不在库存事务里跨服务读定义：单体下两边是同一个池，事务里再要一个连接会整池互等。
//
// # 投递时才发现违反已售规则
//
// 「卖出过的 SKU 不能移出活动 / 配额不能低于已售」core 在写活动之前按库存服务的已售**预检**过（422，与改成消息
// 之前同一句话）。投递时还可能违反，只有两种来源：预检时库存服务不在（core 跳过预检照样保存，见
// admin_promotion.go），或者预检与投递之间已售又涨了（下线的活动不再被新订单命中，只剩 SAGA 重试中的在途单）。
// 这时整组**钳到不变量上**，而不是拒绝：卖出过的 SKU 留着（配额取原值与已售中的大者），配额低于已售的抬到已售
// （等于卖完）。理由：
//
//   - 拒绝没有出路：消息的目标分支没有「失败」可言（dtmrs 对失败的 action 也是重试），重试一万次已售也不会变小；
//   - 钳过的结果守住了那条规则要守的东西 —— 不超卖、关单时放得回配额 —— 而且离运营的定义最近；
//   - 后台活动详情展示的是库存服务里生效的配额与已售（fillActivity），运营看得见实际生效的数；这里记一条 Warn。
//
// 没有另开「同步失败、需处理」的状态：它要一个契约字段和一块后台界面，而它只会在上面两种边角里出现。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/repository"
)

// BranchActivitySync 是库存服务的配额同步分支：单体 local://inventory_activity_sync，
// 拆分 <KEEL_INVENTORY_URL>/internal/v1/saga/inventory_activity_sync?bt=…（core 的 dtm.BranchResolver 给地址）。
const BranchActivitySync = "inventory_activity_sync"

// ActivityMsgGIDPrefix 是配额同步消息的 gid 前缀：promo-{商家}-{活动}-{随机串}。
const ActivityMsgGIDPrefix = "promo-"

// quotaVerifyRounds 是复读核对的轮数上限。定义在这几轮之间一直在变时停下：每一次变都带来一条新消息。
const quotaVerifyRounds = 3

// ActivityMsgGID 编一条配额同步消息的 gid。每次写活动都是一条新消息（随机串），理由同 stockMsgGIDs。
func ActivityMsgGID(merchantID, promotionID int64) (string, error) {
	if promotionID <= 0 {
		return "", fmt.Errorf("%w: promotion_id 是 %d", dtm.ErrBadGID, promotionID)
	}
	return dtm.TenantGID(ActivityMsgGIDPrefix, merchantID, strconv.FormatInt(promotionID, 10)+"-"+nonce())
}

// ParseActivityMsgGID 解出配额同步消息里的活动 id（租户由 dtm.TenantContextFromTenantGID 另取）。
func ParseActivityMsgGID(gid string) (promotionID int64, err error) {
	_, rest, err := dtm.ParseTenantGID(ActivityMsgGIDPrefix, gid)
	if err != nil {
		return 0, err
	}
	pid, n, ok := strings.Cut(rest, "-")
	if !ok || n == "" {
		return 0, fmt.Errorf("%w: %q 不是 promo-商家-活动-随机串 的形状", dtm.ErrBadGID, gid)
	}
	id, ok := canonicalID(pid)
	if !ok {
		return 0, fmt.Errorf("%w: %q 的活动段 %q 不合法", dtm.ErrBadGID, gid, pid)
	}
	return id, nil
}

// QuotaDefinition 是 core 里一场活动当前的配额定义。
type QuotaDefinition struct {
	// Found 为假：core 里没有这场活动（本租户下查不到）。什么都不做 —— 已售是历史，不因为查不到就删。
	Found bool
	// Items 是定义了配额的 SKU。
	Items []ActivityQuota
	// Keep 是在活动里、但 core 没有记录配额的 SKU（00180 之前写的行）：沿用库存服务的现值。
	Keep []int64
}

func (d QuotaDefinition) equal(o QuotaDefinition) bool {
	if d.Found != o.Found || len(d.Items) != len(o.Items) || len(d.Keep) != len(o.Keep) {
		return false
	}
	for i := range d.Items {
		if d.Items[i] != o.Items[i] {
			return false
		}
	}
	for i := range d.Keep {
		if d.Keep[i] != o.Keep[i] {
			return false
		}
	}
	return true
}

// QuotaSource 是「回源」：读 core 里一场活动当前的配额定义。单体是进程内实现（service.PromotionQuotaSource），
// 拆分是 RemoteQuotaSource（经 KEEL_CORE_URL 调 core 的内网接口）。ctx 带租户。
// QuotaSyncPayload 是配额同步消息的载荷（00240）：core 在写活动的同一个事务里把版本 +1、读出定义一起放进来。
// 接收方按 Rev 只接受比已应用的新的那份（AdvanceActivitySyncRev），不回 core 读——下层不调上层（部署方案 4.1）。
type QuotaSyncPayload struct {
	PromotionID int64           `json:"promotion_id"`
	Rev         int64           `json:"rev"`
	Found       bool            `json:"found"`
	Items       []QuotaSyncItem `json:"items"`
	Keep        []int64         `json:"keep"`
}

// QuotaSyncItem 是载荷里一个 SKU 的配额。
type QuotaSyncItem struct {
	SKUID int64 `json:"sku_id"`
	Quota int32 `json:"quota"`
}

func (p QuotaSyncPayload) definition() QuotaDefinition {
	d := QuotaDefinition{Found: p.Found, Keep: p.Keep}
	for _, it := range p.Items {
		d.Items = append(d.Items, ActivityQuota{SKUID: it.SKUID, Quota: it.Quota})
	}
	if len(d.Keep) == 0 {
		d.Keep = nil
	}
	return d
}

type QuotaSource interface {
	QuotaDefinition(ctx context.Context, promotionID int64) (QuotaDefinition, error)
}

// ActivitySyncBranch 是配额同步的接收分支。src 为 nil（拆分部署没配 KEEL_CORE_URL）时一律 Unknown 并喊出来：
// 消息会一直重试到配上为止；活动上线那一刻 core 还会直接同步一次（admin_promotion.go），所以上线的活动不受影响。
func (l *Local) ActivitySyncBranch(src QuotaSource) dtm.BranchFuncEx {
	return func(gid, branchID, op, payload string) int {
		log := slog.Default().With("gid", gid, "branch_id", branchID, "op", op, "branch", BranchActivitySync)
		if op != "action" {
			log.Error("配额同步分支收到的 op 不是 action")
			return dtm.Unknown
		}
		pid, err := ParseActivityMsgGID(gid)
		var ctx context.Context
		if err == nil {
			ctx, _, err = dtm.TenantContextFromTenantGID(context.Background(), ActivityMsgGIDPrefix, gid)
		}
		if err != nil {
			log.Error("配额同步消息的 gid 解不开，丢弃", "err", err)
			return dtm.Success
		}
		if p := strings.TrimSpace(payload); p != "" && p != "{}" {
			var pl QuotaSyncPayload
			if err := json.Unmarshal([]byte(p), &pl); err != nil || pl.PromotionID != pid || pl.Rev <= 0 {
				// 解不开、对不上 gid 里的活动、或没有版本：重试一万次也一样，记下来、吞掉（库存对账会报出差异）。
				log.Error("配额同步消息的载荷不成立，丢弃", "promotion_id", pid, "payload", p, "err", err)
				return dtm.Success
			}
			if err := l.applyVersioned(ctx, gid, branchID, op, pl); err != nil {
				log.Warn("配额同步没做完，按 Unknown 让协调器重试", "promotion_id", pid, "rev", pl.Rev, "err", err)
				return dtm.Unknown
			}
			return dtm.Success
		}
		// 没有载荷：00240 之前登记、升级时还在途的消息。单体里定义就在本进程（src），照旧回源读；
		// 微服务形态没有 src（库存服务不再知道 core），这条只能丢掉，由库存对账报出差异、下一次改活动时补齐。
		if src == nil {
			log.Error("配额同步消息没有载荷（升级前登记的旧消息），本进程读不到定义，丢弃", "promotion_id", pid)
			return dtm.Success
		}
		if err := l.syncActivity(ctx, gid, branchID, op, pid, src); err != nil {
			log.Warn("配额同步没做完，按 Unknown 让协调器重试", "promotion_id", pid, "err", err)
			return dtm.Unknown
		}
		return dtm.Success
	}
}

// applyVersioned 应用载荷里的定义：版本不比已应用的新就什么都不做（乱序、重复）。版本推进与应用定义、
// 子事务屏障同一个事务——同一条消息投两次，第二次被屏障判成重复；两条消息乱序到达，旧的那条被版本挡住。
func (l *Local) applyVersioned(ctx context.Context, gid, branchID, op string, p QuotaSyncPayload) error {
	_, err := l.store.WithSagaBranch(ctx, gid, branchID, op, func(tx repository.InventoryStoreTx) error {
		newer, err := tx.AdvanceActivitySyncRev(ctx, p.PromotionID, p.Rev)
		if err != nil || !newer {
			return err
		}
		return applyQuotaDefinition(ctx, tx, p.PromotionID, p.definition())
	})
	return err
}

func (l *Local) syncActivity(ctx context.Context, gid, branchID, op string, promotionID int64, src QuotaSource) error {
	def, err := src.QuotaDefinition(ctx, promotionID)
	if err != nil {
		return err
	}
	decision, err := l.store.WithSagaBranch(ctx, gid, branchID, op, func(tx repository.InventoryStoreTx) error {
		return applyQuotaDefinition(ctx, tx, promotionID, def)
	})
	if err != nil || decision != repository.DecisionExecute {
		return err
	}
	for round := 0; round < quotaVerifyRounds; round++ {
		again, err := src.QuotaDefinition(ctx, promotionID)
		if err != nil {
			return err
		}
		if again.equal(def) {
			return nil
		}
		def = again
		if err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
			return applyQuotaDefinition(ctx, tx, promotionID, def)
		}); err != nil {
			return err
		}
	}
	slog.InfoContext(ctx, "配额同步：复读核对几轮定义都在变，停在最后一次读到的定义；后续的消息会接着同步",
		"promotion_id", promotionID)
	return nil
}

// applyQuotaDefinition 在配额行的行锁之下把一场活动整组设成定义（钳到已售不变量上，见文件头）。
// 与 SetActivityQuotas 同一把锁、同一个顺序，与下单扣减串行。
func applyQuotaDefinition(ctx context.Context, tx repository.InventoryStoreTx, promotionID int64, def QuotaDefinition) error {
	if !def.Found {
		return nil
	}
	cur, err := tx.LockPromotionActivity(ctx, promotionID)
	if err != nil {
		return err
	}
	current := make(map[int64]repository.ActivityRow, len(cur))
	for _, c := range cur {
		current[c.SKUID] = c
	}
	want := make(map[int64]int32, len(def.Items)+len(def.Keep))
	for _, it := range def.Items {
		want[it.SKUID] = it.Quota
	}
	for _, id := range def.Keep {
		if c, ok := current[id]; ok {
			want[id] = c.Quota
		}
		// 没有现值可沿用：不建行（行不在 = 配额未同步，扣减按配额不足拒绝 —— 宁可少卖）。
	}
	for _, c := range cur {
		if c.Sold == 0 {
			continue
		}
		q, ok := want[c.SKUID]
		switch {
		case !ok:
			slog.WarnContext(ctx, "配额同步：定义里去掉了一个已按活动价卖出过的 SKU，保留它（卖出的配额关单时要放得回）",
				"promotion_id", promotionID, "sku_id", c.SKUID, "sold", c.Sold)
			want[c.SKUID] = max(c.Quota, c.Sold)
		case q > 0 && q < c.Sold:
			slog.WarnContext(ctx, "配额同步：定义的配额低于已售，抬到已售（等于卖完）",
				"promotion_id", promotionID, "sku_id", c.SKUID, "quota", q, "sold", c.Sold)
			want[c.SKUID] = c.Sold
		}
	}
	keep := make([]int64, 0, len(want))
	for id := range want {
		keep = append(keep, id)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i] < keep[j] })
	for _, id := range keep {
		if c, ok := current[id]; ok && c.Quota == want[id] {
			continue // 没变的行不写
		}
		if err := tx.UpsertActivityQuota(ctx, promotionID, id, want[id]); err != nil {
			return err
		}
	}
	_, err = tx.DeleteActivityExcept(ctx, promotionID, keep)
	return err
}

// ---------------------------------------------------------------------------
