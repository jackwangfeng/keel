package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/outcome"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// AI 员工的提案（AI 经营 M9 任务 4，docs/AI经营-M9设计.md §4）。
//
// 写操作默认不直接执行：AI 员工提「证据 + 动作 + 预计影响」，人批准后由 Keel **以 AI 员工的身份**执行。
// 判权两道：批准的人对这件事要有权（按人判）；执行时再按 AI 员工的身份判一次（它提案之后范围可能被收窄、
// 人可能已经把它停用）。执行复用后台接口同一个函数（adjustInventory），幂等键固定为提案 id ——
// 「执行成功但结果没写回」时再点一次批准，库存也只加一次。
//
// M9 只有一种提案：inventory_adjust（加库存）；M10 加了四种（agent_proposal_kinds.go）。

const (
	ProposalKindInventoryAdjust = "inventory_adjust"
	// proposalTTL 是提案的有效期：过期没处理的由过期任务置 50，不能再批。
	proposalTTL = 48 * time.Hour
	// proposalMaxDelta 是一条补货提案的上限（与 restock_plan 的建议上限一致）。
	proposalMaxDelta = 1000
)

var (
	// ErrProposalBadRequest：提案参数不合法。契约 422。
	ErrProposalBadRequest = errors.New("提案参数不合法")
	// ErrProposalNotFound / ErrProposalNotOpen / ErrProposalDuplicate 与 repository 同名错误一一对应。
	ErrProposalNotFound  = errors.New("提案不存在")
	ErrProposalNotOpen   = errors.New("提案已经处理过或已过期")
	ErrProposalDuplicate = errors.New("已有一条同样的待处理提案")
	// ErrAgentOnly / ErrHumanOnly：提案只能由 AI 员工提；批准 / 驳回只能由人来做。契约 403 role-forbidden。
	ErrAgentOnly = fmt.Errorf("%w: 只有 AI 员工能提提案", ErrRoleForbidden)
	ErrHumanOnly = fmt.Errorf("%w: 提案只能由人批准或驳回，AI 员工不行", ErrRoleForbidden)
)

// InventoryAdjustPayload 是 inventory_adjust 提案的执行参数。
type InventoryAdjustPayload struct {
	StoreID int64  `json:"store_id"`
	SKUID   int64  `json:"sku_id"`
	Delta   int32  `json:"delta"`
	Reason  string `json:"reason"`
}

// ProposalResult 是执行结果快照（写进 agent_proposals.result）。
type ProposalResult struct {
	BeforeAvailable *int32 `json:"before_available,omitempty"`
	AfterAvailable  *int32 `json:"after_available,omitempty"`
	// Detail 是 M10 各种提案的执行结果：flash_price → promotion_id；coupon → coupon_template_id；
	// product_copy → before / after 的标题；refund_decision → refund 的新状态。
	Detail    map[string]any `json:"detail,omitempty"`
	ErrorType string         `json:"error_type,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// ProposalInput 是 AI 员工提一条补货提案的输入。
type ProposalInput struct {
	StoreID        int64
	SKUID          int64
	Delta          int32
	Reason         string
	Evidence       string
	ExpectedImpact string
}

// AgentProposalService 实现提案的全部。
type AgentProposalService struct {
	repo   tenantRunner
	inv    inventory.Service
	stores *AdminStoreService
	log    *slog.Logger
	// M10 各种提案的执行者（SetExecutors）：批准后以 AI 员工身份调它们，判权与校验与后台接口逐字相同。
	promos  *AdminPromotionService
	coupons *AdminCouponService
	catalog *AdminCatalogService
	refunds *RefundService
}

// SetExecutors 接上 M10 提案的执行者（装配时调用；没接上的种类提得出、批准时执行失败并说明原因）。
func (s *AgentProposalService) SetExecutors(promos *AdminPromotionService, coupons *AdminCouponService,
	catalog *AdminCatalogService, refunds *RefundService) {
	s.promos, s.coupons, s.catalog, s.refunds = promos, coupons, catalog, refunds
}

func NewAgentProposalService(repo tenantRunner, inv inventory.Service, stores *AdminStoreService,
	log *slog.Logger) *AgentProposalService {
	if log == nil {
		log = slog.Default()
	}
	return &AgentProposalService{repo: repo, inv: inv, stores: stores, log: log}
}

func mapProposalErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrProposalNotFound):
		return fmt.Errorf("%w: %v", ErrProposalNotFound, err)
	case errors.Is(err, repository.ErrProposalNotOpen):
		return fmt.Errorf("%w: %v", ErrProposalNotOpen, err)
	}
	return err
}

func checkProposalText(field, v string, min, max int) error {
	n := len([]rune(strings.TrimSpace(v)))
	if n < min || n > max {
		return fmt.Errorf("%w: %s 长度要在 %d–%d 字", ErrProposalBadRequest, field, min, max)
	}
	return nil
}

// ProposeInventoryAdjust 是 MCP 工具 propose_inventory_adjust：AI 员工提一条补货提案（不执行）。
// 判权与后台「加减库存」相同（storeOperate + 这家店卖这个 SKU）：它提不了它无权做的事。
func (s *AgentProposalService) ProposeInventoryAdjust(ctx context.Context, in ProposalInput) (repository.AgentProposal, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	if !id.IsAgent() {
		return repository.AgentProposal{}, ErrAgentOnly
	}
	if in.Delta < 1 || in.Delta > proposalMaxDelta {
		return repository.AgentProposal{}, fmt.Errorf("%w: delta 取 1–%d（只提加库存）", ErrProposalBadRequest, proposalMaxDelta)
	}
	if err := checkProposalText("reason", in.Reason, 1, 100); err != nil {
		return repository.AgentProposal{}, err
	}
	if err := checkProposalText("evidence", in.Evidence, 10, 8000); err != nil {
		return repository.AgentProposal{}, err
	}
	if err := checkProposalText("expected_impact", in.ExpectedImpact, 0, 2000); err != nil {
		return repository.AgentProposal{}, err
	}
	payload, err := json.Marshal(InventoryAdjustPayload{StoreID: in.StoreID, SKUID: in.SKUID, Delta: in.Delta,
		Reason: strings.TrimSpace(in.Reason)})
	if err != nil {
		return repository.AgentProposal{}, err
	}
	var out repository.AgentProposal
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := authorizeStore(ctx, tx, in.StoreID, storeOperate); err != nil {
			return err
		}
		if err := requireSellable(ctx, tx, in.StoreID, in.SKUID); err != nil {
			return err
		}
		st, err := tx.FindStore(ctx, in.StoreID)
		if err != nil {
			return err
		}
		sku, err := tx.AdminFindSKU(ctx, in.SKUID)
		if err != nil {
			return err
		}
		prod, err := tx.AdminFindProduct(ctx, sku.ProductID)
		if err != nil {
			return err
		}
		// 标题要让店长一眼认出是哪件货：商品名 + 规格（只写规格的话，「尺码：M」认不出是哪件衣服）。
		title := fmt.Sprintf("%s：%s（%s）补 %d 件", st.Name, prod.Title, skuLabel(sku.SKUCode, string(sku.SpecValues)),
			in.Delta)
		skuID, storeID := in.SKUID, in.StoreID
		out, err = insertProposal(ctx, tx, repository.NewAgentProposal{AgentStaffID: id.StaffID,
			Kind: ProposalKindInventoryAdjust, StoreID: &storeID, SKUID: &skuID,
			TargetKey: fmt.Sprintf("store:%d:sku:%d", in.StoreID, in.SKUID), Payload: payload, Title: title,
			Evidence: strings.TrimSpace(in.Evidence), ExpectedImpact: strings.TrimSpace(in.ExpectedImpact)})
		return err
	})
	return s.afterPropose(ctx, out, mapProposalErr(err))
}

// insertProposal 写一条提案并读回：先查同一个（kind，作用对象）有没有待处理的，好告诉 agent 是哪一条 ——
// 插入撞唯一索引会让整个事务失效（之后什么都查不了），所以不能「先插、撞了再查」；插入时仍可能撞上
// （两个请求并发），那时只能回一句不带编号的。ExpiresAt 为零值时取 proposalTTL。
func insertProposal(ctx context.Context, tx repository.Tx, n repository.NewAgentProposal) (repository.AgentProposal, error) {
	if open, err := tx.OpenAgentProposalFor(ctx, n.Kind, n.TargetKey); err == nil {
		return repository.AgentProposal{}, fmt.Errorf("%w：#%d（%s）还没处理，不要重复提", ErrProposalDuplicate, open, n.TargetKey)
	} else if !errors.Is(err, repository.ErrProposalNotFound) {
		return repository.AgentProposal{}, err
	}
	if n.ExpiresAt.IsZero() {
		n.ExpiresAt = time.Now().Add(proposalTTL)
	}
	pid, err := tx.InsertAgentProposal(ctx, n)
	if errors.Is(err, repository.ErrProposalDuplicate) {
		return repository.AgentProposal{}, fmt.Errorf("%w（%s）", ErrProposalDuplicate, n.TargetKey)
	}
	if err != nil {
		return repository.AgentProposal{}, err
	}
	return tx.FindAgentProposal(ctx, pid)
}

// authorizeProposal：看 / 批 / 驳一条提案的人对它要有权 —— 门店类按门店（storeOperate），
// 全店类（营销、商品）要全店范围。
func authorizeProposal(ctx context.Context, tx repository.Tx, p repository.AgentProposal) error {
	if p.StoreID != nil {
		_, err := authorizeStore(ctx, tx, *p.StoreID, storeOperate)
		return err
	}
	_, err := requireMerchantWide(ctx)
	return err
}

// ProposalPage 是一页提案。
type ProposalPage struct {
	Items    []repository.AgentProposal
	Total    int64
	Page     int
	PageSize int
}

// ListMine 是 MCP 工具 list_my_proposals：AI 员工看自己提过的提案与结果（避免重复提、看驳回理由）。
func (s *AgentProposalService) ListMine(ctx context.Context, status *int16, page, pageSize int) (ProposalPage, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return ProposalPage{}, err
	}
	if !id.IsAgent() {
		return ProposalPage{}, ErrAgentOnly
	}
	staffID := id.StaffID
	return s.list(ctx, repository.ProposalFilter{Status: status, AgentStaffID: &staffID}, page, pageSize)
}

// List 实现 GET /admin/agent-proposals：按人的管辖范围收窄到它管的门店（与门店列表同一个收窄）。
func (s *AgentProposalService) List(ctx context.Context, status, agentStaffID *int64, kind *string,
	page, pageSize int) (ProposalPage, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return ProposalPage{}, err
	}
	f := repository.ProposalFilter{AgentStaffID: agentStaffID, Kind: kind}
	if status != nil {
		st := int16(*status)
		f.Status = &st
	}
	if !id.MerchantWide() {
		ids := []int64{}
		for p := 1; ; p++ {
			pg, err := s.stores.ListStores(ctx, nil, false, p, 100)
			if err != nil {
				return ProposalPage{}, err
			}
			for _, st := range pg.Items {
				ids = append(ids, st.ID)
			}
			if int64(p*pg.PageSize) >= pg.Total || len(pg.Items) == 0 {
				break
			}
		}
		f.StoreIDs = ids
	}
	return s.list(ctx, f, page, pageSize)
}

func (s *AgentProposalService) list(ctx context.Context, f repository.ProposalFilter, page, pageSize int) (ProposalPage, error) {
	page, pageSize = clampPaging(page, pageSize)
	out := ProposalPage{Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out.Items, out.Total, e = tx.ListAgentProposals(ctx, f, int32(pageSize), int32(offsetOf(page, pageSize)))
		return e
	})
	return out, err
}

// Get 实现 GET /admin/agent-proposals/{id}：对这家店有 storeOperate 的人才看得到。
func (s *AgentProposalService) Get(ctx context.Context, proposalID int64) (repository.AgentProposal, error) {
	if _, err := requireStaff(ctx); err != nil {
		return repository.AgentProposal{}, err
	}
	var out repository.AgentProposal
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		p, err := tx.FindAgentProposal(ctx, proposalID)
		if err != nil {
			return err
		}
		if err := authorizeProposal(ctx, tx, p); err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, mapProposalErr(err)
}

// Approve 实现 POST /admin/agent-proposals/{id}/approve：批准并执行。
//
// 返回的提案状态说明结果：20 已执行（result 里是执行前后的可售）或 40 执行失败（result 里是原因）。
// 执行失败不是这条接口的失败 —— 批准这个动作成功了，是执行被业务规则拒了（比如 AI 员工已被停用、
// 这家店不再卖这个 SKU），人要看到的是那条原因。
func (s *AgentProposalService) Approve(ctx context.Context, proposalID int64) (repository.AgentProposal, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	if id.IsAgent() {
		return repository.AgentProposal{}, ErrHumanOnly
	}
	var p repository.AgentProposal
	var agentID auth.StaffIdentity
	agentActive := false
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		if p, err = tx.FindAgentProposal(ctx, proposalID); err != nil {
			return err
		}
		if err := authorizeProposal(ctx, tx, p); err != nil {
			return err
		}
		if err := tx.ClaimAgentProposal(ctx, proposalID, id.StaffID); err != nil {
			return err
		}
		a, err := tx.FindAgent(ctx, p.AgentStaffID)
		if err != nil && !errors.Is(err, repository.ErrAgentNotFound) {
			return err
		}
		if err == nil {
			sc, err := tx.ListStaffScopes(ctx, a.ID)
			if err != nil {
				return err
			}
			m, _ := tenant.FromContext(ctx)
			agentID = auth.StaffIdentity{StaffID: a.ID, MerchantID: &m, Role: a.Role, Status: a.Status,
				RegionIDs: sc.RegionIDs, StoreIDs: sc.StoreIDs}
			agentActive = a.Status == auth.StaffStatusActive
		}
		return nil
	})
	if err != nil {
		return repository.AgentProposal{}, mapProposalErr(err)
	}

	return s.runClaimed(ctx, p, agentID, agentActive)
}

// runClaimed 执行一条已认领（15）的提案并写回结果：人批准与按策略自动执行（agent_auto_policy.go）走同一段。
// agentID 是提案的 AI 员工身份；agentActive 为假时不执行、记成执行失败。
func (s *AgentProposalService) runClaimed(ctx context.Context, p repository.AgentProposal, agentID auth.StaffIdentity,
	agentActive bool) (repository.AgentProposal, error) {
	proposalID := p.ID
	status, result := repository.ProposalExecuted, ProposalResult{}
	if !agentActive {
		status, result = repository.ProposalFailed, ProposalResult{ErrorType: "agent-disabled",
			Error: "提这条提案的 AI 员工已被停用或删除，不以它的身份执行"}
	} else {
		res, err := s.execute(auth.NewStaffContext(ctx, agentID), p)
		if err != nil {
			// 业务规则拒绝（范围、可售、库存服务 4xx、活动 / 券 / 商品的校验）记成执行失败；基础设施错误
			// （库存服务不在、库挂了）原样上抛，提案停在 15，稍后再点批准会以同一个幂等键重试。
			if isInfraError(err) {
				return repository.AgentProposal{}, err
			}
			status, result = repository.ProposalFailed, ProposalResult{ErrorType: proposalErrorType(err), Error: err.Error()}
		} else {
			result = res
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	var out repository.AgentProposal
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.FinishAgentProposal(ctx, proposalID, status, raw); err != nil {
			return err
		}
		if status == repository.ProposalExecuted {
			due, now := outcomePlan(p.Kind, p.Payload, time.Now())
			if err := tx.SetAgentProposalExecuted(ctx, proposalID, due, now); err != nil {
				return err
			}
		}
		if err := emitProposalDecided(ctx, tx, proposalID); err != nil { // AI 员工事件（agent_event.go）
			return err
		}
		var e error
		out, e = tx.FindAgentProposal(ctx, proposalID)
		return e
	})
	return out, mapProposalErr(err)
}

// isInfraError：库存服务不在、超时、数据库连接 / 资源 / 并发冲突这类「重试可能成功」的错误 —— 它们让批准整体失败
// （提案留在 15，稍后再点批准以同一个幂等键重试）。其余一律是业务规则的拒绝（范围、可售、库存不够、目标已变、
// 参数不合规、约束冲突……），记成执行失败（40）并写明原因。
//
// 之前反过来写（列出业务错误、其余当基础设施）：白名单漏了售后单已被人工处理、券参数被后台拒、商品已删，
// 于是批准 500、提案永久卡在 15 —— 过期任务只扫 10、人工驳回回 409、同目标去重又把它算作待处理，
// 那个目标从此再也提不了（2026-09-28 破坏性测试）。
func isInfraError(err error) bool {
	if inventory.IsUnavailable(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// 等锁超时（55P03 —— 不在下面那几个类里）与库存进程回的 busy（拆分形态，rpc.ErrBusy）：
	// 确定没生效、过一会儿就好，与语句超时（57014）同一类。
	if outcome.IsDBBusy(err) {
		return true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code[:2] {
		case "08", "40", "53", "57", "58": // 连接、事务回滚（死锁 / 序列化失败）、资源不足、运维干预、系统错误
			return true
		}
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return false
}

func proposalErrorType(err error) string {
	switch {
	case errors.Is(err, ErrOutOfScope):
		return "out-of-scope"
	case errors.Is(err, ErrRoleForbidden):
		return "role-forbidden"
	case errors.Is(err, repository.ErrSKUNotSoldInStore), errors.Is(err, repository.ErrCatalogNotFound):
		return "sku-not-sold-in-store"
	default:
		return "rejected"
	}
}

// Reject 实现 POST /admin/agent-proposals/{id}/reject：驳回，理由必填（AI 员工用 list_my_proposals 看得到）。
func (s *AgentProposalService) Reject(ctx context.Context, proposalID int64, reason string) (repository.AgentProposal, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	if id.IsAgent() {
		return repository.AgentProposal{}, ErrHumanOnly
	}
	if err := checkProposalText("reason", reason, 1, 500); err != nil {
		return repository.AgentProposal{}, err
	}
	var out repository.AgentProposal
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		p, err := tx.FindAgentProposal(ctx, proposalID)
		if err != nil {
			return err
		}
		if err := authorizeProposal(ctx, tx, p); err != nil {
			return err
		}
		if err := tx.RejectAgentProposal(ctx, proposalID, id.StaffID, strings.TrimSpace(reason)); err != nil {
			return err
		}
		if err := emitProposalDecided(ctx, tx, proposalID); err != nil { // AI 员工事件（agent_event.go）
			return err
		}
		out, err = tx.FindAgentProposal(ctx, proposalID)
		return err
	})
	return out, mapProposalErr(err)
}
