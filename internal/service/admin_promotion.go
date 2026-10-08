package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/keel/keel/internal/catalogimport"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 营销活动的后台：列表 / 新建 / 详情 / 修改 / 上下线。数据模型 §7「营销活动」。
//
// # 权限：与券管理同一行
//
// 活动直接决定订单实付，是资金面 —— 判据是 requireMerchantWide（全店范围：商家级管理员与
// 操作员，平台级切到本店时同样算），与券模板一模一样。大区 / 门店管理员不行：活动的范围可以
// 跨大区，一个只管华北的人不该能开一个全国满减。登记在 permission_test.go 的大表里。
//
// # 上线中不许改规则
//
// 上线中的活动随时可能正被一笔试算或下单读着。边读边改，试算按旧规则、下单按新规则算钱，
// 那正是「试算与下单必须共用一份计算」要防的事，只不过换成了数据而不是代码在分叉。
// 所以上线中只能改名、下线；改规则先下线（409 promotion-online）。
// 已成交的订单不受任何修改影响：订单行快照了单价与分摊额，订单上快照了命中的活动。
//
// # 活动配额：定义在 core，生效的那一份在库存服务，二阶段消息同步（2026-09-30）
//
// 配额与已售是库存（00075）：下单扣减在库存服务里与门店库存同一个本地事务扣它们，已售只有库存服务知道。
// 但「运营配的配额是多少」是活动规则，00180 起记在 core 的 promotion_skus.quota_qty（定义）；库存服务的
// activity_stocks.quota 是按定义同步过去、扣减真正认的那一份。
//
// 改之前是「先调库存服务整组设配额、再写活动表」两段（新建反过来：先提交活动、再设配额），中间任何一步失败
// 两边就不一致，只有每小时只读只报的库存对账能发现。现在按「派生数据同步约定」：
//
//	写活动的 core 事务里登记一条二阶段消息（QuotaSync.prepare，promotion_quota_msg.go），提交后 submit；
//	库存服务的接收分支（inventory 包 activity_msg.go）不信消息内容，回源读这场活动**当前**的定义、整组设，
//	子事务屏障挡重复，复读核对挡乱序 —— 结果永远以 core 的当前定义为准。
//
// 提交之后等投递结果至多 2 秒（库存服务在的时候，写接口的回显就是同步之后的配额）；等不到也回成功。
//
// # 各条路径
//
//   - 新建（Create）：活动行、活动商品（含 quota_qty）与消息同一个事务。库存服务不在也能建（新活动一律下线，
//     配额没同步之前不会被任何一单命中）。幂等重放不再发消息（第一次那条已经保证送到）。
//   - 修改活动商品（Update，下线状态）：先在事务之外按库存服务的已售**预检**「卖出过的 SKU 不能移出活动 /
//     配额不能低于已售」（422，与改之前同一句话，checkActivityRules）；再写活动与消息。**库存服务不在时跳过预检、
//     照样保存**：预检要的已售只有它知道；投递时违反了规则的，接收方把整组钳到不变量上（保留卖出过的 SKU、
//     配额抬到已售）并记 Warn —— 为什么钳而不拒、为什么不另开「需处理」状态，写在 activity_msg.go 的文件头。
//   - 上线（0 → 1）：**照旧先直接同步**（syncQuotas，库存服务不在就 503），再写 status。上线那一刻配额必须已经
//     是定义的样子 —— 靠消息的话，库存服务不在时活动会带着旧配额先上线，窗口里按旧配额卖。写 status 的那个事务里
//     **总是再登记一条消息**（2026-10-01）；写 status 的事务失败了则补发一条（resync）。两种情况都是把库存服务拉回
//     core 的当前定义。
//
//     为什么直接同步之后还要发：直接同步的是第一遍读到的定义 A。两遍之间一次并发的改配额（下线状态，合法）
//     可能已经提交了 B 并发出它的消息；那条消息要是先于 A 的直接同步落地，库存最后是 A、core 是 B、活动已上线，
//     A > B 时按 A 超卖秒杀配额。接收方的复读核对发生在它自己写入之前，管不到之后才到的直接同步。
//     补的这一条与写 status 同一个事务，提交时直接同步已经结束，而上线之后定义冻结（上线中不许改规则），
//     所以它投递时回源读到的就是最终定义，落地之后库存与 core 一致。
//     不选「第二遍比对 plan2.sync 与 plan.sync、不一致才发」：比对相等也不能证明两遍之间没有过别的定义
//     （A → C → A 的那条 C 消息照样可能晚到），而多发一条的代价只是一次幂等的整组设。
//   - 下线（1 → 0）、只改名：定义没变，不发（「只在有意义的变化时发」）。下线后配额行照旧留着 —— 已售是历史，
//     关单时要放得回。
//   - 删除（Delete）：硬删。必须先下线；订单行 / 新人礼发放 / 每人限购累计 / 库存已售任一非零 → 409
//     promotion-in-use。日常收尾仍是下线；删除只清「配错草稿 / 演示脏数据」。删成功后尽力把库存侧
//     配额行清掉（sold 已为 0，SetActivityQuotas 空名单即可）。
//
// 没接协调器的装配（QuotaSync 为 nil，只有部分测试这么装）退回改之前的直接调用，行为与改之前逐字相同。
//
// 读（列表、详情、写接口的回显）在事务之后向库存服务按活动批量取生效的配额与已售，库存服务不在时 503；
// 写接口的回显取不到时用 core 的定义、已售记 0。

var (
	// ErrPromotionBadRequest：规则本身不成立（满 100 减 200、秒杀没配额、范围目标查不到……）。422。
	ErrPromotionBadRequest = errors.New("营销活动的配置不成立")

	// ErrPromotionNotFound：活动在本租户查不到。404。
	ErrPromotionNotFound = errors.New("营销活动不存在")

	// ErrPromotionOnline：活动上线中却想改规则或删除。409 promotion-online。
	ErrPromotionOnline = errors.New("活动上线中，改规则请先下线")

	// ErrPromotionInUse：活动已有成交 / 发放 / 已售，不能硬删。409 promotion-in-use。
	ErrPromotionInUse = errors.New("活动已有成交或发放记录，不能删除")
)

const (
	scopeAdminPromotionCreate = "admin.promotions.create"

	maxPromotionNameRunes = 60
	maxPromotionTiers     = 10
	maxPromotionScopes    = 200
	maxPromotionSkus      = 200
)

// AdminPromotionService 实现 /admin/promotions 那四条接口。
type AdminPromotionService struct {
	repo  CouponRepository
	inv   inventory.Service
	quota *QuotaSync
	now   func() time.Time
}

// WithQuotaSync 让配额同步走二阶段消息（文件头）。q 为 nil 时退回直接调用。
func (s *AdminPromotionService) WithQuotaSync(q *QuotaSync) *AdminPromotionService {
	s.quota = q
	return s
}

func NewAdminPromotionService(r CouponRepository, inv inventory.Service) *AdminPromotionService {
	return &AdminPromotionService{repo: r, inv: inv, now: time.Now}
}

// fillActivity 把库存服务里的配额与已售填进一批视图（事务之后调）。库存服务没有的行记 0 / 0。
func (s *AdminPromotionService) fillActivity(ctx context.Context, views []AdminPromotionView) error {
	ids := make([]int64, 0, len(views))
	for _, v := range views {
		if len(v.Skus) > 0 {
			ids = append(ids, v.Promotion.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	act, err := s.inv.ActivityStock(ctx, inventory.ActivityQuery{PromotionIDs: ids})
	if err != nil {
		return err
	}
	for i := range views {
		applyActivity(views[i].Skus, act)
	}
	return nil
}

func applyActivity(skus []repository.PromotionSku, act map[inventory.ActivityKey]inventory.Activity) {
	for j := range skus {
		a := act[inventory.ActivityKey{PromotionID: skus[j].PromotionID, SKUID: skus[j].SKUID}]
		skus[j].StockQty, skus[j].SoldQty = a.Quota, a.Sold
	}
}

// applyDefinition 用 core 的配额定义（quota_qty）盖住配额：合并「没传的活动商品沿用现状」时，现状以定义为准
// （库存服务里那一份可能还没同步到）。没有定义的行（00180 之前写的）保留库存服务的值。
func applyDefinition(skus []repository.PromotionSku) {
	for j := range skus {
		if skus[j].QuotaQty != nil {
			skus[j].StockQty = *skus[j].QuotaQty
		}
	}
}

// checkActivityRules 是「卖出过的 SKU 不能移出活动 / 配额不能低于已售」在 core 这一侧的预检（文件头「各条路径」）。
// act 是库存服务里这场活动现有的配额与已售。话与库存服务的 ActivityRuleError 逐字相同。
func checkActivityRules(id int64, act map[inventory.ActivityKey]inventory.Activity, skus []repository.PromotionSkuInput) error {
	want := make(map[int64]int32, len(skus))
	for _, sk := range skus {
		want[sk.SKUID] = sk.StockQty
	}
	keys := make([]inventory.ActivityKey, 0, len(act))
	for k := range act {
		if k.PromotionID == id && act[k].Sold > 0 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].SKUID < keys[j].SKUID })
	for _, k := range keys {
		a := act[k]
		q, ok := want[k.SKUID]
		var rule *inventory.ActivityRuleError
		switch {
		case !ok:
			rule = &inventory.ActivityRuleError{SKUID: k.SKUID, Sold: a.Sold, Removed: true}
		case q > 0 && q < a.Sold:
			rule = &inventory.ActivityRuleError{SKUID: k.SKUID, Sold: a.Sold, Quota: q}
		}
		if rule != nil {
			return fmt.Errorf("%w: %v", ErrPromotionBadRequest, rule)
		}
	}
	return nil
}

// syncQuotas 整组把活动配额交给库存服务。违反已售规则翻成 422（与拆分前 checkSoldSkusKept 同一句话）。
func (s *AdminPromotionService) syncQuotas(ctx context.Context, id int64, skus []repository.PromotionSkuInput) error {
	items := make([]inventory.ActivityQuota, 0, len(skus))
	for _, sk := range skus {
		items = append(items, inventory.ActivityQuota{SKUID: sk.SKUID, Quota: sk.StockQty})
	}
	err := s.inv.SetActivityQuotas(ctx, id, items)
	var rule *inventory.ActivityRuleError
	if errors.As(err, &rule) {
		return fmt.Errorf("%w: %v", ErrPromotionBadRequest, rule)
	}
	return err
}

// PromotionPhase 是现算的阶段（契约 AdminPromotion.phase）。
type PromotionPhase string

const (
	PhaseOffline   PromotionPhase = "offline"
	PhaseScheduled PromotionPhase = "scheduled"
	PhaseRunning   PromotionPhase = "running"
	PhaseEnded     PromotionPhase = "ended"
)

func phaseOf(p repository.Promotion, now time.Time) PromotionPhase {
	switch {
	case p.Status != 1:
		return PhaseOffline
	case now.Before(p.StartsAt):
		return PhaseScheduled
	case !now.Before(p.EndsAt):
		return PhaseEnded
	default:
		return PhaseRunning
	}
}

// AdminPromotionView 是后台看到的一个活动：本体、阶梯、范围、活动商品、新人礼已发张数。
type AdminPromotionView struct {
	Promotion   repository.Promotion
	Phase       PromotionPhase
	Tiers       []repository.PromotionTier
	Scopes      []repository.CouponScope
	Skus        []repository.PromotionSku
	GiftGranted int32
}

// AdminPromotionPage 是 GET /admin/promotions 的一页。
type AdminPromotionPage struct {
	Items    []AdminPromotionView
	Total    int64
	Page     int
	PageSize int
}

func (s *AdminPromotionService) views(ctx context.Context, tx repository.Tx,
	ps []repository.Promotion) ([]AdminPromotionView, error) {
	ids := make([]int64, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	tiers, err := tx.ListPromotionTiers(ctx, ids)
	if err != nil {
		return nil, err
	}
	scopes, err := tx.ListPromotionScopes(ctx, ids)
	if err != nil {
		return nil, err
	}
	skus, err := tx.ListPromotionSkus(ctx, ids)
	if err != nil {
		return nil, err
	}
	granted, err := tx.CountGiftGrants(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]AdminPromotionView, 0, len(ps))
	for _, p := range ps {
		out = append(out, AdminPromotionView{
			Promotion: p, Phase: phaseOf(p, now), Tiers: tiers[p.ID], Scopes: scopes[p.ID],
			Skus: skus[p.ID], GiftGranted: granted[p.ID],
		})
	}
	return out, nil
}

func (s *AdminPromotionService) view(ctx context.Context, tx repository.Tx, id int64) (AdminPromotionView, error) {
	p, err := tx.AdminGetPromotion(ctx, id)
	if errors.Is(err, repository.ErrPromotionNotFound) {
		return AdminPromotionView{}, fmt.Errorf("%w: promotion_id=%d", ErrPromotionNotFound, id)
	}
	if err != nil {
		return AdminPromotionView{}, err
	}
	vs, err := s.views(ctx, tx, []repository.Promotion{p})
	if err != nil {
		return AdminPromotionView{}, err
	}
	return vs[0], nil
}

// List 实现 GET /admin/promotions。
func (s *AdminPromotionService) List(ctx context.Context, status, promoType *int16,
	page, pageSize int) (AdminPromotionPage, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return AdminPromotionPage{}, err
	}
	if status != nil && *status != 0 && *status != 1 {
		return AdminPromotionPage{}, fmt.Errorf("%w: status 只能是 0 或 1", ErrPromotionBadRequest)
	}
	if promoType != nil && (*promoType < 1 || *promoType > 5) {
		return AdminPromotionPage{}, fmt.Errorf("%w: promotion_type 只能是 1 到 5", ErrPromotionBadRequest)
	}
	page, pageSize = clampPaging(page, pageSize)
	out := AdminPromotionPage{Items: []AdminPromotionView{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		ps, total, err := tx.AdminListPromotions(ctx, status, promoType,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if err != nil {
			return err
		}
		out.Total = total
		out.Items, err = s.views(ctx, tx, ps)
		return err
	})
	if err != nil {
		return AdminPromotionPage{}, err
	}
	if err := s.fillActivity(ctx, out.Items); err != nil {
		return AdminPromotionPage{}, err
	}
	return out, nil
}

// Get 实现 GET /admin/promotions/{promotion_id}。
func (s *AdminPromotionService) Get(ctx context.Context, id int64) (AdminPromotionView, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return AdminPromotionView{}, err
	}
	var out AdminPromotionView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		out, err = s.view(ctx, tx, id)
		return err
	})
	if err != nil {
		return AdminPromotionView{}, err
	}
	vs := []AdminPromotionView{out}
	if err := s.fillActivity(ctx, vs); err != nil {
		return AdminPromotionView{}, err
	}
	return vs[0], nil
}

// PromotionRules 是活动的规则部分：新建时全给，修改时 nil 表示不改。
type PromotionRules struct {
	Tiers  *[]repository.PromotionTier
	Scopes *[]repository.CouponScopeInput
	Skus   *[]repository.PromotionSkuInput
}

// PromotionInput 是新建请求。
type PromotionInput struct {
	Name            string
	Type            int16
	ThresholdUnit   int16
	StackWithCoupon bool
	StartsAt        time.Time
	EndsAt          time.Time
	GiftTemplateID  *int64
	Rules           PromotionRules
}

// Create 实现 POST /admin/promotions。新建的活动一律是下线的。
func (s *AdminPromotionService) Create(ctx context.Context, in PromotionInput,
	idemKey string) (AdminPromotionView, bool, error) {
	staff, err := requireMerchantWide(ctx)
	if err != nil {
		return AdminPromotionView{}, false, err
	}
	f := repository.PromotionFields{
		Name: strings.TrimSpace(in.Name), Type: in.Type, ThresholdUnit: in.ThresholdUnit,
		StackWithCoupon: in.StackWithCoupon, GiftTemplateID: in.GiftTemplateID,
		StartsAt: in.StartsAt, EndsAt: in.EndsAt, Status: 0,
	}
	tiers, scopes, skus := derefRules(in.Rules)
	if err := validatePromotion(f, tiers, scopes, skus); err != nil {
		return AdminPromotionView{}, false, err
	}
	hash, err := adminRequestHash(nil, in)
	if err != nil {
		return AdminPromotionView{}, false, err
	}
	var gid string
	var lost bool
	merchantID, _ := tenant.FromContext(ctx) // 走到这里 ctx 一定带租户（requireMerchantWide 过了）
	out, replayed, err := idempotentTenantWrite(ctx, s.repo, scopeAdminPromotionCreate, repository.StaffSubject(staff.StaffID),
		idemKey, hash, archivedCreated, func(tx repository.Tx) (AdminPromotionView, error) {
			if err := checkPromotionTargets(ctx, tx, f, scopes, skus); err != nil {
				return AdminPromotionView{}, err
			}
			id, err := tx.AdminCreatePromotion(ctx, f)
			if err != nil {
				return AdminPromotionView{}, mapPromotionRepoErr(err)
			}
			if err := writePromotionRules(ctx, tx, id, in.Rules); err != nil {
				return AdminPromotionView{}, err
			}
			// 配额同步的消息与活动同一个事务（文件头）。
			if len(skus) > 0 && s.quota.ready() {
				if gid, lost, err = s.quota.prepare(ctx, tx, merchantID, id); err != nil {
					return AdminPromotionView{}, err
				}
			}
			v, err := s.view(ctx, tx, id)
			if err != nil {
				return AdminPromotionView{}, err
			}
			// 回显（也是幂等存档）里的配额取自请求：新活动在库存服务里还没有配额行，已售恒为 0。
			quota := map[int64]int32{}
			for _, sk := range skus {
				quota[sk.SKUID] = sk.StockQty
			}
			for i := range v.Skus {
				v.Skus[i].StockQty, v.Skus[i].SoldQty = quota[v.Skus[i].SKUID], 0
			}
			return v, nil
		})
	s.quota.finish(ctx, gid, err == nil)
	if err == nil && lost {
		s.quota.resync(ctx, merchantID, out.Promotion.ID)
	}
	if err != nil || len(out.Skus) == 0 || s.quota.ready() {
		return out, replayed, err
	}
	// 以下是没接协调器时的老路：活动行提交之后再设配额（新建的活动还没有 id 可给库存服务）。新活动一律是下线的，
	// 配额没设上之前它不会被任何一单命中；设不上回 503，同一把幂等键重试会走重放、再设一次。
	// 重放时库存服务里已经有这个活动的配额行（第一次其实设上了、或之后被修改同步过）就不再设 ——
	// 存档里是新建那一刻的配额，拿它覆盖会把之后的修改抹掉。
	if replayed {
		cur, err := s.inv.ActivityStock(ctx, inventory.ActivityQuery{PromotionIDs: []int64{out.Promotion.ID}})
		if err != nil {
			return AdminPromotionView{}, false, err
		}
		if len(cur) > 0 {
			return out, replayed, nil
		}
	}
	if err := s.syncQuotas(ctx, out.Promotion.ID, skuInputsOf(out.Skus)); err != nil {
		return AdminPromotionView{}, false, err
	}
	return out, replayed, nil
}

// PromotionPatch 是 PATCH 请求：nil 表示不改。
type PromotionPatch struct {
	Name            *string
	ThresholdUnit   *int16
	StackWithCoupon *bool
	StartsAt        *time.Time
	EndsAt          *time.Time
	GiftTemplateID  *int64
	Status          *int16
	Rules           PromotionRules
}

// ruleChanges：这次 PATCH 除了 name / status 之外有没有碰规则。
func (p PromotionPatch) touchesRules() bool {
	return p.ThresholdUnit != nil || p.StackWithCoupon != nil || p.StartsAt != nil ||
		p.EndsAt != nil || p.GiftTemplateID != nil || p.Rules.Tiers != nil ||
		p.Rules.Scopes != nil || p.Rules.Skus != nil
}

// Delete 实现 DELETE /admin/promotions/{promotion_id}。见文件头「删除」那一段。
func (s *AdminPromotionService) Delete(ctx context.Context, id int64) error {
	if _, err := requireMerchantWide(ctx); err != nil {
		return err
	}
	act, err := s.inv.ActivityStock(ctx, inventory.ActivityQuery{PromotionIDs: []int64{id}})
	if err != nil {
		return err
	}
	for k, a := range act {
		if a.Sold > 0 {
			return fmt.Errorf("%w: 活动已有已售（sku=%d sold=%d）", ErrPromotionInUse, k.SKUID, a.Sold)
		}
	}

	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		status, err := tx.AdminLockPromotion(ctx, id)
		if errors.Is(err, repository.ErrPromotionNotFound) {
			return fmt.Errorf("%w: promotion_id=%d", ErrPromotionNotFound, id)
		}
		if err != nil {
			return err
		}
		if status != 0 {
			return fmt.Errorf("%w: 上线中的活动请先下线再删", ErrPromotionOnline)
		}
		inUse, err := tx.AdminPromotionInUse(ctx, id)
		if err != nil {
			return err
		}
		if inUse {
			return fmt.Errorf("%w: 订单 / 新人礼 / 限购累计仍引用这场活动", ErrPromotionInUse)
		}
		if err := tx.AdminDeletePromotion(ctx, id); err != nil {
			if errors.Is(err, repository.ErrPromotionNotFound) {
				return fmt.Errorf("%w: promotion_id=%d", ErrPromotionNotFound, id)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 尽力清库存侧配额行（sold 已为 0）。失败只记日志：core 行已没了，对账会报孤儿。
	if err := s.inv.SetActivityQuotas(ctx, id, nil); err != nil {
		slog.WarnContext(ctx, "删除活动后清配额失败（库存对账会报出孤儿行）", "promotion_id", id, "err", err)
	}
	return nil
}

// Update 实现 PATCH /admin/promotions/{promotion_id}。
//
// 先锁活动行、再判「上线中不许改规则」、再合并与校验、最后整行写回与整组替换 ——
// 判定与写必须看到同一个 status（AdminLockPromotion 上的注释）。
func (s *AdminPromotionService) Update(ctx context.Context, id int64, p PromotionPatch) (AdminPromotionView, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return AdminPromotionView{}, err
	}
	if p.Status != nil && *p.Status != 0 && *p.Status != 1 {
		return AdminPromotionView{}, fmt.Errorf("%w: status 只能是 0 或 1", ErrPromotionBadRequest)
	}
	// 现有配额与已售（库存服务）：预检已售规则、回显要用，事务之外先问。走消息时它不在也照样保存（文件头）。
	act, err := s.inv.ActivityStock(ctx, inventory.ActivityQuery{PromotionIDs: []int64{id}})
	actKnown := err == nil
	if err != nil {
		if !s.quota.ready() || !inventory.IsUnavailable(err) {
			return AdminPromotionView{}, err
		}
		slog.WarnContext(ctx, "改活动时库存服务不在：跳过已售预检照样保存，配额由消息在它回来之后同步",
			"promotion_id", id, "err", err)
		act = nil
	}
	// 第一遍：只判不写（锁、上线中不许改规则、合并、校验、目标归属），算出要不要同步配额。
	var plan quotaPlan
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		plan, _, err = s.updateTx(ctx, tx, id, p, act, false)
		return err
	})
	if err != nil {
		return AdminPromotionView{}, err
	}
	direct := false
	if plan.sync != nil {
		switch {
		case plan.goingLive || !s.quota.ready():
			// 上线（或没接协调器）：先直接设配额，被拒就什么都没写（文件头「各条路径」）。
			if h := beforeDirectQuotaSync.Load(); h != nil {
				(*h)(ctx, id)
			}
			if err := s.syncQuotas(ctx, id, plan.sync); err != nil {
				return AdminPromotionView{}, err
			}
			direct = true
			if act, err = s.inv.ActivityStock(ctx, inventory.ActivityQuery{PromotionIDs: []int64{id}}); err != nil {
				return AdminPromotionView{}, err
			}
			actKnown = true
		case actKnown:
			if err := checkActivityRules(id, act, plan.sync); err != nil {
				return AdminPromotionView{}, err
			}
		}
	}
	// 第二遍：同一套判定再走一次（两遍之间状态可能变了），然后写；要同步的（直接同步过的也算，文件头「上线」），
	// 消息同一个事务登记。
	var out AdminPromotionView
	var gid string
	var lost bool
	merchantID, _ := tenant.FromContext(ctx)
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		var plan2 quotaPlan
		plan2, out, err = s.updateTx(ctx, tx, id, p, act, true)
		if err != nil || plan2.sync == nil || !s.quota.ready() {
			return err
		}
		gid, lost, err = s.quota.prepare(ctx, tx, merchantID, id)
		return err
	})
	s.quota.finish(ctx, gid, err == nil)
	if err != nil {
		if direct {
			// 配额已经按这次的定义直接设过了，而定义没写进去：补一条消息，把库存服务拉回 core 的当前定义。
			s.quota.resync(ctx, merchantID, id)
		}
		return AdminPromotionView{}, err
	}
	if lost {
		s.quota.resync(ctx, merchantID, id)
	}
	if gid != "" {
		// 等过投递了：回显取同步之后的那一份；取不到用定义。
		if a, e := s.inv.ActivityStock(ctx, inventory.ActivityQuery{PromotionIDs: []int64{id}}); e == nil {
			act, actKnown = a, true
		}
	}
	applyActivity(out.Skus, act)
	if !actKnown {
		applyDefinition(out.Skus)
	}
	return out, nil
}

// beforeDirectQuotaSync 是测试钩子：Update 第一遍提交之后、直接同步配额之前调用，好确定性地插入一次并发修改
// （上线与改配额交错，文件头「上线」）。生产代码里恒为 nil。
var beforeDirectQuotaSync atomic.Pointer[func(ctx context.Context, promotionID int64)]

// SetBeforeDirectQuotaSyncHook 装上测试钩子（nil 卸下），回一个恢复原值的函数。只给测试用。
func SetBeforeDirectQuotaSyncHook(f func(ctx context.Context, promotionID int64)) (restore func()) {
	var p *func(ctx context.Context, promotionID int64)
	if f != nil {
		p = &f
	}
	old := beforeDirectQuotaSync.Swap(p)
	return func() { beforeDirectQuotaSync.Store(old) }
}

// quotaPlan 是 updateTx 算出来的「要不要同步配额」：sync 是要同步的那一组（nil = 不同步），goingLive 是这一次上线。
type quotaPlan struct {
	sync      []repository.PromotionSkuInput
	goingLive bool
}

// updateTx 是 Update 的一遍：锁活动行、判「上线中不许改规则」、合并与校验；write 为真时整行写回与
// 整组替换并回视图。另回「要不要先向库存服务同步配额、同步哪一组」：改了活动商品，或者这一次上线。
// act 是库存服务里现有的配额与已售，合并「没传的活动商品沿用现状」时用它补上配额。
func (s *AdminPromotionService) updateTx(ctx context.Context, tx repository.Tx, id int64, p PromotionPatch,
	act map[inventory.ActivityKey]inventory.Activity, write bool) (quotaPlan, AdminPromotionView, error) {
	status, err := tx.AdminLockPromotion(ctx, id)
	if errors.Is(err, repository.ErrPromotionNotFound) {
		return quotaPlan{}, AdminPromotionView{}, fmt.Errorf("%w: promotion_id=%d", ErrPromotionNotFound, id)
	}
	if err != nil {
		return quotaPlan{}, AdminPromotionView{}, err
	}
	if status == 1 && p.touchesRules() {
		return quotaPlan{}, AdminPromotionView{}, fmt.Errorf("%w: 上线中的活动只能改名或下线", ErrPromotionOnline)
	}
	cur, err := s.view(ctx, tx, id)
	if err != nil {
		return quotaPlan{}, AdminPromotionView{}, err
	}
	applyActivity(cur.Skus, act)
	applyDefinition(cur.Skus)
	f := promotionFieldsOf(cur.Promotion)
	if p.Name != nil {
		f.Name = strings.TrimSpace(*p.Name)
	}
	if p.ThresholdUnit != nil {
		f.ThresholdUnit = *p.ThresholdUnit
	}
	if p.StackWithCoupon != nil {
		f.StackWithCoupon = *p.StackWithCoupon
	}
	if p.StartsAt != nil {
		f.StartsAt = *p.StartsAt
	}
	if p.EndsAt != nil {
		f.EndsAt = *p.EndsAt
	}
	if p.GiftTemplateID != nil {
		f.GiftTemplateID = p.GiftTemplateID
	}
	if p.Status != nil {
		f.Status = *p.Status
	}

	// 合并之后的完整规则：没传的一组沿用现状。
	tiers, scopes, skus := cur.Tiers, scopeInputsOf(cur.Scopes), skuInputsOf(cur.Skus)
	if p.Rules.Tiers != nil {
		tiers = *p.Rules.Tiers
	}
	if p.Rules.Scopes != nil {
		scopes = *p.Rules.Scopes
	}
	if p.Rules.Skus != nil {
		skus = *p.Rules.Skus
	}
	if err := validatePromotion(f, tiers, scopes, skus); err != nil {
		return quotaPlan{}, AdminPromotionView{}, err
	}
	// 上线（0 → 1）时再核一遍「这个活动能不能真的生效」：规则不完整或已经过期的活动
	// 上线了也不会命中任何一单，而运营会以为它在跑。
	goingLive := status == 0 && f.Status == 1
	if goingLive {
		if err := validateGoLive(f, tiers, skus, s.now()); err != nil {
			return quotaPlan{}, AdminPromotionView{}, err
		}
	}
	if err := checkPromotionTargets(ctx, tx, f, scopes, skus); err != nil {
		return quotaPlan{}, AdminPromotionView{}, err
	}
	plan := quotaPlan{goingLive: goingLive}
	if p.Rules.Skus != nil || goingLive {
		plan.sync = append([]repository.PromotionSkuInput{}, skus...)
	}
	if !write {
		return plan, AdminPromotionView{}, nil
	}
	if err := tx.AdminUpdatePromotion(ctx, id, f); err != nil {
		return quotaPlan{}, AdminPromotionView{}, mapPromotionRepoErr(err)
	}
	if err := writePromotionRules(ctx, tx, id, p.Rules); err != nil {
		return quotaPlan{}, AdminPromotionView{}, err
	}
	out, err := s.view(ctx, tx, id)
	return plan, out, err
}

func derefRules(r PromotionRules) ([]repository.PromotionTier, []repository.CouponScopeInput,
	[]repository.PromotionSkuInput) {
	var tiers []repository.PromotionTier
	var scopes []repository.CouponScopeInput
	var skus []repository.PromotionSkuInput
	if r.Tiers != nil {
		tiers = *r.Tiers
	}
	if r.Scopes != nil {
		scopes = *r.Scopes
	}
	if r.Skus != nil {
		skus = *r.Skus
	}
	return tiers, scopes, skus
}

// writePromotionRules 把传了的那几组整组替换进库。没传的不动。
func writePromotionRules(ctx context.Context, tx repository.Tx, id int64, r PromotionRules) error {
	if r.Tiers != nil {
		tiers := append([]repository.PromotionTier(nil), (*r.Tiers)...)
		sort.Slice(tiers, func(i, j int) bool { return tiers[i].Threshold < tiers[j].Threshold })
		if err := tx.ReplacePromotionTiers(ctx, id, tiers); err != nil {
			return mapPromotionRepoErr(err)
		}
	}
	if r.Scopes != nil {
		if err := tx.ReplacePromotionScopes(ctx, id, *r.Scopes); err != nil {
			return mapPromotionRepoErr(err)
		}
	}
	if r.Skus != nil {
		if err := tx.ReplacePromotionSkus(ctx, id, *r.Skus); err != nil {
			return mapPromotionRepoErr(err)
		}
	}
	return nil
}

func promotionFieldsOf(p repository.Promotion) repository.PromotionFields {
	return repository.PromotionFields{
		Name: p.Name, Type: p.Type, ThresholdUnit: p.ThresholdUnit, StackWithCoupon: p.StackWithCoupon,
		GiftTemplateID: p.GiftTemplateID, StartsAt: p.StartsAt, EndsAt: p.EndsAt, Status: p.Status,
	}
}

func scopeInputsOf(cur []repository.CouponScope) []repository.CouponScopeInput {
	out := make([]repository.CouponScopeInput, 0, len(cur))
	for _, c := range cur {
		out = append(out, repository.CouponScopeInput{ScopeType: c.ScopeType, TargetID: c.TargetID, Include: c.Include})
	}
	return out
}

func skuInputsOf(cur []repository.PromotionSku) []repository.PromotionSkuInput {
	out := make([]repository.PromotionSkuInput, 0, len(cur))
	for _, c := range cur {
		out = append(out, repository.PromotionSkuInput{
			SKUID: c.SKUID, PromoPriceCents: c.PromoPriceCents, DiscountRate: c.DiscountRate,
			PerUserLimit: c.PerUserLimit, StockQty: c.StockQty,
		})
	}
	return out
}

// validatePromotion 在进库之前把 chk_promotion_* 与跨表的形状规则用人话说一遍。
// 数据库的 CHECK 是最后一道；这里先挡，是为了让 422 的 detail 说得出是哪一条。
// 阶梯与活动类型的对应（满减配减免额、满折配折扣率）是跨表的，CHECK 管不到，只在这里。
func validatePromotion(f repository.PromotionFields, tiers []repository.PromotionTier,
	scopes []repository.CouponScopeInput, skus []repository.PromotionSkuInput) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrPromotionBadRequest, fmt.Sprintf(format, args...))
	}
	if f.Name == "" || utf8.RuneCountInString(f.Name) > maxPromotionNameRunes {
		return bad("name 必须是 1 到 %d 个字", maxPromotionNameRunes)
	}
	if f.StartsAt.IsZero() || f.EndsAt.IsZero() || !f.EndsAt.After(f.StartsAt) {
		return bad("ends_at 必须晚于 starts_at")
	}
	if len(tiers) > maxPromotionTiers || len(scopes) > maxPromotionScopes || len(skus) > maxPromotionSkus {
		return bad("阶梯至多 %d 档、范围至多 %d 条、活动商品至多 %d 个",
			maxPromotionTiers, maxPromotionScopes, maxPromotionSkus)
	}
	switch f.Type {
	case repository.PromoFullReduction, repository.PromoFullDiscount:
		if f.ThresholdUnit != repository.ThresholdByAmount && f.ThresholdUnit != repository.ThresholdByQty {
			return bad("满减满折要给 threshold_unit：1 金额（分）或 2 件数")
		}
		if len(skus) > 0 || f.GiftTemplateID != nil {
			return bad("满减满折不带 skus / gift_coupon_template_id")
		}
		seen := map[int64]bool{}
		for i, t := range tiers {
			if t.Threshold <= 0 {
				return bad("第 %d 档：threshold 必须大于 0", i+1)
			}
			if seen[t.Threshold] {
				return bad("第 %d 档：门槛 %d 重复了", i+1, t.Threshold)
			}
			seen[t.Threshold] = true
			if f.Type == repository.PromoFullReduction {
				if t.DiscountCents <= 0 || t.DiscountRate != 0 {
					return bad("第 %d 档：满减填 discount_cents（> 0），discount_rate 为 0", i+1)
				}
				if f.ThresholdUnit == repository.ThresholdByAmount && t.Threshold < t.DiscountCents {
					return bad("第 %d 档：「满 %s 减 %s」减的比门槛还多，是配置错误",
						i+1, yuan(t.Threshold), yuan(t.DiscountCents))
				}
			} else if t.DiscountRate < 1 || t.DiscountRate > 999 || t.DiscountCents != 0 {
				return bad("第 %d 档：满折填 discount_rate（千分比 1 到 999，900 = 9 折），discount_cents 为 0", i+1)
			}
		}
		// 阶梯越高、优惠要越大：「满 100 减 20、满 200 减 10」会让多买的人少减钱。
		sorted := append([]repository.PromotionTier(nil), tiers...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Threshold < sorted[j].Threshold })
		for i := 1; i < len(sorted); i++ {
			a, b := sorted[i-1], sorted[i]
			if f.Type == repository.PromoFullReduction && b.DiscountCents <= a.DiscountCents {
				return bad("门槛更高的一档（%d）减免必须更多", b.Threshold)
			}
			if f.Type == repository.PromoFullDiscount && b.DiscountRate >= a.DiscountRate {
				return bad("门槛更高的一档（%d）折扣必须更低", b.Threshold)
			}
		}
	case repository.PromoLimitedPrice, repository.PromoFlashSale:
		if f.ThresholdUnit != 0 || len(tiers) > 0 || f.GiftTemplateID != nil {
			return bad("限时折扣 / 秒杀不带 threshold_unit / tiers / gift_coupon_template_id")
		}
		seen := map[int64]bool{}
		for i, s := range skus {
			if s.SKUID <= 0 {
				return bad("第 %d 个活动商品：sku_id 必须为正", i+1)
			}
			if seen[s.SKUID] {
				return bad("第 %d 个活动商品：sku %d 出现了两次", i+1, s.SKUID)
			}
			seen[s.SKUID] = true
			if (s.PromoPriceCents > 0) == (s.DiscountRate > 0) {
				return bad("sku %d：promo_price_cents 与 discount_rate 二选一（另一个为 0）", s.SKUID)
			}
			if s.PromoPriceCents < 0 || s.PromoPriceCents > catalogimport.MaxPriceCents || s.DiscountRate < 0 || s.DiscountRate > 999 {
				return bad("sku %d：特价不能为负，折扣是千分比 1 到 999", s.SKUID)
			}
			if s.PerUserLimit < 0 || s.PerUserLimit > maxLineQuantity {
				return bad("sku %d：per_user_limit 在 0 到 %d 之间（0 不限）", s.SKUID, maxLineQuantity)
			}
			if f.Type == repository.PromoFlashSale && s.StockQty <= 0 {
				return bad("sku %d：秒杀必须给活动配额 stock_qty（> 0）", s.SKUID)
			}
			if f.Type == repository.PromoLimitedPrice && s.StockQty != 0 {
				return bad("sku %d：限时折扣不限配额，stock_qty 为 0；要限量请建秒杀", s.SKUID)
			}
		}
		// 商品已由 skus 点名，商品维度的范围没有意义；只认门店维度（5 大区、6 门店）。
		for i, sc := range scopes {
			if sc.ScopeType != repository.ScopeRegion && sc.ScopeType != repository.ScopeStore {
				return bad("第 %d 条范围：限时折扣 / 秒杀只认大区（5）与门店（6）", i+1)
			}
		}
	case repository.PromoNewBuyerGift:
		if f.ThresholdUnit != 0 || len(tiers) > 0 || len(skus) > 0 || len(scopes) > 0 {
			return bad("新人礼只带 gift_coupon_template_id")
		}
		if f.GiftTemplateID == nil || *f.GiftTemplateID <= 0 {
			return bad("新人礼要给 gift_coupon_template_id")
		}
	default:
		return bad("promotion_type 只能是 1 满减 / 2 满折 / 3 限时折扣 / 4 秒杀 / 5 新人礼")
	}
	return validateScopeShapes(scopes)
}

// validateGoLive：上线时规则要完整、活动还没结束。
func validateGoLive(f repository.PromotionFields, tiers []repository.PromotionTier,
	skus []repository.PromotionSkuInput, now time.Time) error {
	switch {
	case (f.Type == repository.PromoFullReduction || f.Type == repository.PromoFullDiscount) && len(tiers) == 0:
		return fmt.Errorf("%w: 满减满折至少要有一档才能上线", ErrPromotionBadRequest)
	case (f.Type == repository.PromoLimitedPrice || f.Type == repository.PromoFlashSale) && len(skus) == 0:
		return fmt.Errorf("%w: 限时折扣 / 秒杀至少要有一个活动商品才能上线", ErrPromotionBadRequest)
	case !f.EndsAt.After(now):
		return fmt.Errorf("%w: 活动已经结束（ends_at 在过去），上线也不会生效", ErrPromotionBadRequest)
	}
	return nil
}

// validateScopeShapes 是范围的形状规则，与券的适用范围同一套（admin_coupon.go 的 SetScopes）。
func validateScopeShapes(in []repository.CouponScopeInput) error {
	type key struct {
		t  int16
		id int64
	}
	seen := map[key]bool{}
	for i, sc := range in {
		switch {
		case sc.ScopeType == repository.ScopeAll:
			if sc.TargetID != nil || !sc.Include {
				return fmt.Errorf("%w: 第 %d 条范围：全场规则不带 target_id，也不能是排除", ErrPromotionBadRequest, i+1)
			}
		case sc.ScopeType >= repository.ScopeCategory && sc.ScopeType <= repository.ScopeStore:
			if sc.TargetID == nil || *sc.TargetID <= 0 {
				return fmt.Errorf("%w: 第 %d 条范围：scope_type=%d 必须给正的 target_id",
					ErrPromotionBadRequest, i+1, sc.ScopeType)
			}
		default:
			return fmt.Errorf("%w: 第 %d 条范围：scope_type 只能是 1 到 6", ErrPromotionBadRequest, i+1)
		}
		k := key{t: sc.ScopeType}
		if sc.TargetID != nil {
			k.id = *sc.TargetID
		}
		if seen[k] {
			return fmt.Errorf("%w: 第 %d 条范围：同一个目标出现了两次", ErrPromotionBadRequest, i+1)
		}
		seen[k] = true
	}
	return nil
}

// checkPromotionTargets：范围目标、活动商品、新人礼的券模板都在当前租户里逐个查一遍
// （RLS 之下别家的 id 与不存在的同形）。多态的 target_id 与 promotion_skus 以外的引用，
// 外键管不到的那部分在这里。
func checkPromotionTargets(ctx context.Context, tx repository.Tx, f repository.PromotionFields,
	scopes []repository.CouponScopeInput, skus []repository.PromotionSkuInput) error {
	byKind := map[repository.ScopeTargetKind][]int64{}
	for _, sc := range scopes {
		if kind, ok := targetKindOf(sc.ScopeType); ok {
			byKind[kind] = append(byKind[kind], *sc.TargetID)
		}
	}
	for kind, ids := range byKind {
		live, err := tx.LiveTargetIDs(ctx, kind, ids)
		if err != nil {
			return err
		}
		if missing := missingIDs(ids, live); len(missing) > 0 {
			return fmt.Errorf("%w: %s %v 在本店查不到（不存在、已删除或属于别家店）",
				ErrPromotionBadRequest, targetKindName(kind), missing)
		}
	}
	if len(skus) > 0 {
		ids := make([]int64, 0, len(skus))
		for _, s := range skus {
			ids = append(ids, s.SKUID)
		}
		live, err := tx.LiveSkuIDs(ctx, ids)
		if err != nil {
			return err
		}
		if missing := missingIDs(ids, live); len(missing) > 0 {
			return fmt.Errorf("%w: SKU %v 在本店查不到", ErrPromotionBadRequest, missing)
		}
	}
	if f.GiftTemplateID != nil {
		live, err := tx.LiveCouponTemplateIDs(ctx, []int64{*f.GiftTemplateID})
		if err != nil {
			return err
		}
		if len(live) == 0 {
			return fmt.Errorf("%w: 券模板 %d 在本店查不到", ErrPromotionBadRequest, *f.GiftTemplateID)
		}
	}
	return nil
}

func missingIDs(want, live []int64) []int64 {
	ok := map[int64]bool{}
	for _, v := range live {
		ok[v] = true
	}
	var out []int64
	for _, v := range want {
		if !ok[v] {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func mapPromotionRepoErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrPromotionRuleViolation):
		return fmt.Errorf("%w: %v", ErrPromotionBadRequest, err)
	case errors.Is(err, repository.ErrPromotionNotFound):
		return fmt.Errorf("%w: %v", ErrPromotionNotFound, err)
	default:
		return err
	}
}
