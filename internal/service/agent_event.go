package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// AI 员工的事件（AI 经营 M10 §3，docs/AI经营-M10M11设计.md，00121）。
//
// Keel 在四种时刻写一条 agent_events：
//
//	stock_low          一个（门店，SKU）的可售不高于预警线（库存预警同一口径），每对每 24 小时至多一条  —— 扫描（agent_event_sweep.go）
//	refund_created     买家提交售后申请                                                                  —— 与退款单同一个事务（refund.go Create）
//	search_zero_spike  一个无结果词近 1 小时出现 ≥ 5 次，每词每天至多一条（全店口径，store_id 为空）        —— 扫描
//	proposal_decided   提案被批准执行 / 执行失败 / 驳回 / 过期                                             —— 与写结果同一个事务
//
// 消费两种：
//   - 拉：MCP 工具 list_events / ack_events（handler/mcp_tools_events.go）。每名 AI 员工一个游标；
//     事件按它的管辖范围过滤 —— 门店级事件只给能操作那家店的，全店事件（store_id 为空）只给全店范围的。
//   - 推：后台给 AI 员工配 webhook（本文件后半），事件写入时给每个启用的 webhook 入一条投递任务，
//     投递时再按 AI 员工**当时**的身份判范围（agent_webhook_delivery.go）。
//
// 事件写在业务事务里：售后单没建成就没有事件，事件写失败售后单也不成立 —— 两者不会一个在一个不在。

// 四种事件类型（与 00121 的 CHECK 约束一致）。
const (
	AgentEventStockLow        = "stock_low"
	AgentEventRefundCreated   = "refund_created"
	AgentEventSearchZeroSpike = "search_zero_spike"
	AgentEventProposalDecided = "proposal_decided"
)

const (
	// agentEventListDefault / agentEventListMax 是 list_events 一次返回的默认 / 最多条数。
	agentEventListDefault = 50
	agentEventListMax     = 100
	// agentWebhookMaxAttempts 是一次投递至多尝试几次（jobs 的指数退避：2、4、8、16、32 秒）。
	agentWebhookMaxAttempts = 6
	// agentWebhookSecretPrefix 是签名密钥明文的前缀（一眼认得出，也方便密钥扫描按前缀告警）。
	agentWebhookSecretPrefix = "kwhs_"
	// agentWebhookRecentDeliveries 是后台 GET webhook 时带回的最近投递条数。
	agentWebhookRecentDeliveries = 20
)

var (
	// ErrAgentEventBadRequest：list_events / ack_events 的参数不合法。契约 422。
	ErrAgentEventBadRequest = errors.New("事件参数不合法")
	// ErrAgentWebhookNotFound：这名 AI 员工没有配 webhook。契约 404。
	ErrAgentWebhookNotFound = errors.New("这名 AI 员工没有配 webhook")
	// ErrAgentWebhookBadURL：webhook 地址不合法。包着 ErrStaffBadRequest，走 AI 员工接口同一个 422 出口。
	ErrAgentWebhookBadURL = fmt.Errorf("%w: webhook 地址不合法", ErrStaffBadRequest)
)

// ---------------------------------------------------------------------------
// 写事件（各业务事务里调）
// ---------------------------------------------------------------------------

// emitAgentEvent 写一条事件；真的写进去了（不是重复的 dedupe_key）就给每个启用的 webhook 入一条投递任务。
func emitAgentEvent(ctx context.Context, tx repository.Tx, typ string, storeID *int64, payload any, dedupeKey string) (bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	id, inserted, err := tx.InsertAgentEvent(ctx, repository.NewAgentEvent{Type: typ, StoreID: storeID, Payload: raw,
		DedupeKey: dedupeKey})
	if err != nil || !inserted {
		return false, err
	}
	return true, enqueueAgentEventDeliveries(ctx, tx, []int64{id})
}

// enqueueAgentEventDeliveries 给每条新事件 × 每个启用的 webhook 入一条投递任务（至多 6 次）。
// 范围不在这里判：AI 员工的范围可能在投递之前被改，按投递那一刻的身份判（agent_webhook_delivery.go）。
func enqueueAgentEventDeliveries(ctx context.Context, tx repository.Tx, eventIDs []int64) error {
	if len(eventIDs) == 0 {
		return nil
	}
	hooks, err := tx.ListEnabledAgentWebhookIDs(ctx)
	if err != nil || len(hooks) == 0 {
		return err
	}
	for _, e := range eventIDs {
		for _, w := range hooks {
			p, _ := json.Marshal(agentEventJob{EventID: e, WebhookID: w})
			if _, err := tx.EnqueueJob(ctx, repository.NewJob{Queue: repository.QueueAgentEventDelivery,
				JobKey: fmt.Sprintf("%d:%d", e, w), Payload: p, MaxAttempts: agentWebhookMaxAttempts}); err != nil {
				return err
			}
		}
	}
	return nil
}

// agentEventJob 是投递任务的 payload：只放两个 id（jobs 没有 RLS，payload 越薄越好）。
type agentEventJob struct {
	EventID   int64 `json:"event_id"`
	WebhookID int64 `json:"webhook_id"`
}

// emitRefundCreated：买家提交了售后申请（refund.go Create，同一个事务）。
func emitRefundCreated(ctx context.Context, tx repository.Tx, r repository.Refund) error {
	store := r.StoreID
	_, err := emitAgentEvent(ctx, tx, AgentEventRefundCreated, &store, map[string]any{
		"refund_no": r.RefundNo, "order_no": r.OrderNo, "store_id": r.StoreID,
		"amount_cents": r.AmountCents, "refund_type": r.RefundType,
	}, "refund_created:"+r.RefundNo)
	return err
}

// emitProposalDecided：一条提案有了结果（agent_proposal.go 的 Approve / Reject，写结果的同一个事务）。
// 状态名由 SQL 按 agent_proposals 当前的行翻（EmitProposalDecidedEvent），这里只给 id。
func emitProposalDecided(ctx context.Context, tx repository.Tx, proposalID int64) error {
	ids, err := tx.EmitProposalDecidedEvent(ctx, proposalID)
	if err != nil {
		return err
	}
	return enqueueAgentEventDeliveries(ctx, tx, ids)
}

// emitExpiredProposalEvents：过期扫描（agent_proposal_expiry.go）在同一个事务里给刚过期的提案补事件。
func emitExpiredProposalEvents(ctx context.Context, tx repository.Tx) error {
	ids, err := tx.EmitExpiredProposalEvents(ctx)
	if err != nil {
		return err
	}
	return enqueueAgentEventDeliveries(ctx, tx, ids)
}

// ---------------------------------------------------------------------------
// 拉（MCP list_events / ack_events）
// ---------------------------------------------------------------------------

// agentEventScope 把一名员工的身份翻成事件的可见范围：全店范围的看全部；大区管理员看本大区门店的；
// 门店管理员看自己门店的。门店级之外的全店事件只给全店范围的（与搜索概况只给全店范围的人同一个理由）。
func agentEventScope(id auth.StaffIdentity) repository.AgentEventScope {
	switch {
	case id.MerchantWide():
		return repository.AgentEventScope{MerchantWide: true}
	case id.Role == auth.StaffRoleRegionManager:
		return repository.AgentEventScope{RegionIDs: nonNil(id.RegionIDs)}
	case id.Role == auth.StaffRoleStoreManager:
		return repository.AgentEventScope{StoreIDs: nonNil(id.StoreIDs)}
	default:
		return repository.AgentEventScope{}
	}
}

// AgentEventService 是事件的拉取与 webhook 配置。
type AgentEventService struct {
	repo tenantRunner
}

func NewAgentEventService(repo tenantRunner) *AgentEventService {
	return &AgentEventService{repo: repo}
}

// AgentEventPage 是 list_events 的一页。Cursor 是这名 AI 员工确认到的位置；NextAfterID 是这一页最后一条的 id
// （没有新事件时等于这次的 after_id）——处理完这一页就 ack_events(NextAfterID)。
type AgentEventPage struct {
	Items       []repository.AgentEvent
	Cursor      int64
	NextAfterID int64
	HasMore     bool
}

func requireAgentStaff(ctx context.Context) (auth.StaffIdentity, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if !id.IsAgent() {
		return auth.StaffIdentity{}, fmt.Errorf("%w: 事件只给 AI 员工拉取", ErrRoleForbidden)
	}
	return id, nil
}

// ListForAgent 实现 MCP list_events：afterID 为 nil 时从这名 AI 员工的游标接着读。
func (s *AgentEventService) ListForAgent(ctx context.Context, afterID *int64, limit int) (AgentEventPage, error) {
	id, err := requireAgentStaff(ctx)
	if err != nil {
		return AgentEventPage{}, err
	}
	if afterID != nil && *afterID < 0 {
		return AgentEventPage{}, fmt.Errorf("%w: after_id 不能是负数", ErrAgentEventBadRequest)
	}
	if limit < 0 || limit > agentEventListMax {
		return AgentEventPage{}, fmt.Errorf("%w: limit 取 1–%d", ErrAgentEventBadRequest, agentEventListMax)
	}
	if limit == 0 {
		limit = agentEventListDefault
	}
	var out AgentEventPage
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		cur, err := tx.AgentEventCursor(ctx, id.StaffID)
		if err != nil {
			return err
		}
		out.Cursor = cur
		after := cur
		if afterID != nil {
			after = *afterID
		}
		// 多取一条判断还有没有下一页。
		items, err := tx.ListAgentEvents(ctx, after, agentEventScope(id), int32(limit+1))
		if err != nil {
			return err
		}
		if len(items) > limit {
			out.HasMore = true
			items = items[:limit]
		}
		out.Items = items
		out.NextAfterID = after
		if len(items) > 0 {
			out.NextAfterID = items[len(items)-1].ID
		}
		return nil
	})
	return out, err
}

// Ack 实现 MCP ack_events：把这名 AI 员工的游标推到 upTo（只进不退），返回推完的游标。
// upTo 必须是本店一条存在的事件 —— 否则一个手滑的大数会让它之后的全部事件都被跳过。
func (s *AgentEventService) Ack(ctx context.Context, upTo int64) (int64, error) {
	id, err := requireAgentStaff(ctx)
	if err != nil {
		return 0, err
	}
	if upTo <= 0 {
		return 0, fmt.Errorf("%w: up_to_id 必须是正整数（list_events 返回的 next_after_id）", ErrAgentEventBadRequest)
	}
	var cur int64
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := tx.FindAgentEvent(ctx, upTo); errors.Is(err, repository.ErrAgentEventNotFound) {
			return fmt.Errorf("%w: 没有 id 为 %d 的事件", ErrAgentEventBadRequest, upTo)
		} else if err != nil {
			return err
		}
		var err error
		cur, err = tx.AckAgentEvents(ctx, id.StaffID, upTo)
		return err
	})
	return cur, err
}

// ---------------------------------------------------------------------------
// webhook 配置（后台 /admin/agents/{staff_id}/webhook，本店管理员）
// ---------------------------------------------------------------------------

// AgentWebhookView 是后台看到的 webhook。Secret 只在新建或 rotate_secret 时非空（明文只给这一次）。
type AgentWebhookView struct {
	Webhook    repository.AgentWebhook
	Secret     string
	Deliveries []repository.AgentWebhookDelivery
}

// AgentWebhookInput 是 PUT 的请求体。Enabled 为 nil：新建时默认启用，改时不动。
type AgentWebhookInput struct {
	URL          string
	Enabled      *bool
	RotateSecret bool
}

// checkWebhookURL：https；为了本机联调与测试，http 只放 127.0.0.1 / localhost / ::1。不许带账号密码。
//
// 不防内网 https 地址（SSRF 的那一半）：配 webhook 的是本店管理员，他本来就能决定 AI 员工连到哪里；
// 投递不跟随重定向、不把响应体给任何人看，能拿到的只有一个状态码。
func checkWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 500 {
		return "", fmt.Errorf("%w：url 必填，不超过 500 字符", ErrAgentWebhookBadURL)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("%w：%q 不是一个完整的 URL", ErrAgentWebhookBadURL, raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w：URL 里不能带账号密码", ErrAgentWebhookBadURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); !(h == "localhost" || (ip != nil && ip.IsLoopback())) {
			return "", fmt.Errorf("%w：只接受 https（http 只允许 127.0.0.1 / localhost，给本机联调用）", ErrAgentWebhookBadURL)
		}
	default:
		return "", fmt.Errorf("%w：只接受 https", ErrAgentWebhookBadURL)
	}
	return raw, nil
}

func newWebhookSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return agentWebhookSecretPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func findAgentFor(ctx context.Context, tx repository.Tx, staffID int64) error {
	if _, err := tx.FindAgent(ctx, staffID); errors.Is(err, repository.ErrAgentNotFound) {
		return fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, staffID)
	} else if err != nil {
		return err
	}
	return nil
}

func mapWebhookErr(err error) error {
	if errors.Is(err, repository.ErrAgentWebhookNotFound) {
		return fmt.Errorf("%w: %v", ErrAgentWebhookNotFound, err)
	}
	return err
}

// GetWebhook 实现 GET /admin/agents/{staff_id}/webhook：不含密钥，带最近 20 次投递。
func (s *AgentEventService) GetWebhook(ctx context.Context, staffID int64) (AgentWebhookView, error) {
	if _, err := requireShopAdmin(ctx); err != nil {
		return AgentWebhookView{}, err
	}
	var out AgentWebhookView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := findAgentFor(ctx, tx, staffID); err != nil {
			return err
		}
		w, err := tx.FindAgentWebhookByAgent(ctx, staffID)
		if err != nil {
			return err
		}
		ds, err := tx.ListAgentWebhookDeliveries(ctx, w.ID, agentWebhookRecentDeliveries)
		if err != nil {
			return err
		}
		out = AgentWebhookView{Webhook: w, Deliveries: ds}
		return nil
	})
	return out, mapWebhookErr(err)
}

// PutWebhook 实现 PUT /admin/agents/{staff_id}/webhook：没有就建（生成密钥），有就改地址 / 开关；
// RotateSecret 时换一把新密钥。密钥明文只在新建与轮换时回。
func (s *AgentEventService) PutWebhook(ctx context.Context, staffID int64, in AgentWebhookInput) (AgentWebhookView, error) {
	if _, err := requireShopAdmin(ctx); err != nil {
		return AgentWebhookView{}, err
	}
	u, err := checkWebhookURL(in.URL)
	if err != nil {
		return AgentWebhookView{}, err
	}
	var out AgentWebhookView
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := findAgentFor(ctx, tx, staffID); err != nil {
			return err
		}
		cur, err := tx.FindAgentWebhookByAgent(ctx, staffID)
		switch {
		case errors.Is(err, repository.ErrAgentWebhookNotFound):
			secret, err := newWebhookSecret()
			if err != nil {
				return err
			}
			enabled := in.Enabled == nil || *in.Enabled
			w, err := tx.InsertAgentWebhook(ctx, staffID, u, secret, enabled)
			if err != nil {
				return err
			}
			out = AgentWebhookView{Webhook: w, Secret: secret}
			return nil
		case err != nil:
			return err
		}
		enabled := cur.Enabled
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		var secret *string
		if in.RotateSecret {
			s, err := newWebhookSecret()
			if err != nil {
				return err
			}
			secret = &s
		}
		w, err := tx.UpdateAgentWebhook(ctx, staffID, u, enabled, secret)
		if err != nil {
			return err
		}
		out = AgentWebhookView{Webhook: w}
		if secret != nil {
			out.Secret = *secret
		}
		return nil
	})
	return out, mapWebhookErr(err)
}

// DeleteWebhook 实现 DELETE /admin/agents/{staff_id}/webhook。还在队列里的投递任务到时发现 webhook 不在了，直接结束。
func (s *AgentEventService) DeleteWebhook(ctx context.Context, staffID int64) error {
	if _, err := requireShopAdmin(ctx); err != nil {
		return err
	}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := findAgentFor(ctx, tx, staffID); err != nil {
			return err
		}
		return tx.DeleteAgentWebhook(ctx, staffID)
	})
	return mapWebhookErr(err)
}
