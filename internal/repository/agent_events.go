package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// AI 员工的事件、游标与 webhook（00121，AI 经营 M10 §3）。什么时候写事件、谁看得到、怎么投递在 service/agent_event*.go。

// QueueAgentEventDelivery 是事件 webhook 投递的队列名：每个（事件，webhook）一条任务。
const QueueAgentEventDelivery = "agent.event.deliver"

var (
	// ErrAgentEventNotFound：本店没有这条事件。
	ErrAgentEventNotFound = errors.New("事件不存在")
	// ErrAgentWebhookNotFound：这名 AI 员工没有配 webhook（或 id 不是本店的）。
	ErrAgentWebhookNotFound = errors.New("webhook 不存在")
)

// AgentEvent 是一条事件。Payload 是原样的 JSON 对象。
type AgentEvent struct {
	ID        int64
	Type      string
	StoreID   *int64
	Payload   []byte
	CreatedAt time.Time
}

// NewAgentEvent 是一条要写的事件。DedupeKey 店内唯一：同一个 key 第二次写是空操作。
type NewAgentEvent struct {
	Type      string
	StoreID   *int64
	Payload   []byte
	DedupeKey string
}

// AgentEventScope 是拉事件时的范围：MerchantWide 看全部；否则只看 StoreIDs 里的门店、RegionIDs 里的大区下门店的。
type AgentEventScope struct {
	MerchantWide bool
	StoreIDs     []int64
	RegionIDs    []int64
}

// StoreSKU 是一个（门店，SKU）。
type StoreSKU struct {
	StoreID int64
	SKUID   int64
}

// ZeroSpike 是近 1 小时里一个无结果词与它的无结果次数。
type ZeroSpike struct {
	Term  string
	Count int64
}

// AgentWebhook 是一名 AI 员工的事件 webhook（含签名密钥明文 —— 只给投递与「刚建 / 刚轮换」的响应用）。
type AgentWebhook struct {
	ID           int64
	AgentStaffID int64
	URL          string
	Secret       string
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AgentWebhookDelivery 是一次投递尝试。StatusCode 为 nil 表示没拿到 HTTP 响应。
type AgentWebhookDelivery struct {
	ID          int64
	EventID     int64
	WebhookID   int64
	Attempt     int32
	StatusCode  *int32
	Error       string
	DeliveredAt time.Time
}

// AgentEventTx 是事件那一面。
type AgentEventTx interface {
	// InsertAgentEvent 写一条事件；dedupe_key 已有时 inserted = false。
	InsertAgentEvent(ctx context.Context, e NewAgentEvent) (id int64, inserted bool, err error)
	// EmitProposalDecidedEvent 给一条已有结果的提案写 proposal_decided，返回新写的事件 id（已有则为空）。
	EmitProposalDecidedEvent(ctx context.Context, proposalID int64) ([]int64, error)
	// EmitExpiredProposalEvents 给最近一天过期、还没有事件的提案补 proposal_decided。
	EmitExpiredProposalEvents(ctx context.Context) ([]int64, error)
	RecentStockLowEvents(ctx context.Context) (map[StoreSKU]bool, error)
	SearchZeroSpikes(ctx context.Context, minCount int64, limit int32) ([]ZeroSpike, error)
	ListAgentEvents(ctx context.Context, afterID int64, scope AgentEventScope, limit int32) ([]AgentEvent, error)
	FindAgentEvent(ctx context.Context, id int64) (AgentEvent, error)
	// AgentEventCursor 是这名 AI 员工确认到的事件 id；从没确认过是 0。
	AgentEventCursor(ctx context.Context, agentStaffID int64) (int64, error)
	// AckAgentEvents 把游标推到 upTo（只进不退），返回推完之后的游标。
	AckAgentEvents(ctx context.Context, agentStaffID, upTo int64) (int64, error)

	ListEnabledAgentWebhookIDs(ctx context.Context) ([]int64, error)
	FindAgentWebhookByAgent(ctx context.Context, agentStaffID int64) (AgentWebhook, error)
	FindAgentWebhook(ctx context.Context, id int64) (AgentWebhook, error)
	InsertAgentWebhook(ctx context.Context, agentStaffID int64, url, secret string, enabled bool) (AgentWebhook, error)
	// UpdateAgentWebhook 改地址与开关；secret 非 nil 时换密钥。
	UpdateAgentWebhook(ctx context.Context, agentStaffID int64, url string, enabled bool, secret *string) (AgentWebhook, error)
	DeleteAgentWebhook(ctx context.Context, agentStaffID int64) error
	InsertAgentWebhookDelivery(ctx context.Context, d AgentWebhookDelivery) error
	ListAgentWebhookDeliveries(ctx context.Context, webhookID int64, limit int32) ([]AgentWebhookDelivery, error)
}

func (t tenantTx) InsertAgentEvent(ctx context.Context, e NewAgentEvent) (int64, bool, error) {
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	id, err := t.q.InsertAgentEvent(ctx, db.InsertAgentEventParams{Type: e.Type, StoreID: e.StoreID,
		Payload: payload, DedupeKey: e.DedupeKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (t tenantTx) EmitProposalDecidedEvent(ctx context.Context, proposalID int64) ([]int64, error) {
	return t.q.EmitProposalDecidedEvent(ctx, proposalID)
}

func (t tenantTx) EmitExpiredProposalEvents(ctx context.Context) ([]int64, error) {
	return t.q.EmitExpiredProposalEvents(ctx)
}

func (t tenantTx) RecentStockLowEvents(ctx context.Context) (map[StoreSKU]bool, error) {
	rows, err := t.q.RecentStockLowEvents(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[StoreSKU]bool, len(rows))
	for _, r := range rows {
		out[StoreSKU{StoreID: r.StoreID, SKUID: r.SkuID}] = true
	}
	return out, nil
}

func (t tenantTx) SearchZeroSpikes(ctx context.Context, minCount int64, limit int32) ([]ZeroSpike, error) {
	rows, err := t.q.SearchZeroSpikes(ctx, db.SearchZeroSpikesParams{MinCount: minCount, RowLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]ZeroSpike, 0, len(rows))
	for _, r := range rows {
		out = append(out, ZeroSpike{Term: r.Term, Count: r.ZeroCount})
	}
	return out, nil
}

func (t tenantTx) ListAgentEvents(ctx context.Context, afterID int64, scope AgentEventScope, limit int32) ([]AgentEvent, error) {
	rows, err := t.q.ListAgentEvents(ctx, db.ListAgentEventsParams{AfterID: afterID, MerchantWide: scope.MerchantWide,
		StoreIds: emptyIfNil(scope.StoreIDs), RegionIds: emptyIfNil(scope.RegionIDs), RowLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]AgentEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, AgentEvent{ID: r.ID, Type: r.Type, StoreID: r.StoreID, Payload: r.Payload, CreatedAt: r.CreatedAt.Time})
	}
	return out, nil
}

// emptyIfNil：nil 切片在 pgx 里是 NULL 数组，= ANY(NULL) 是 NULL —— 结果同样是「不匹配」，
// 但给一个空数组让语义不靠 NULL 的三值逻辑。
func emptyIfNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

func (t tenantTx) FindAgentEvent(ctx context.Context, id int64) (AgentEvent, error) {
	r, err := t.q.GetAgentEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEvent{}, fmt.Errorf("event %d: %w", id, ErrAgentEventNotFound)
	}
	if err != nil {
		return AgentEvent{}, err
	}
	return AgentEvent{ID: r.ID, Type: r.Type, StoreID: r.StoreID, Payload: r.Payload, CreatedAt: r.CreatedAt.Time}, nil
}

func (t tenantTx) AgentEventCursor(ctx context.Context, agentStaffID int64) (int64, error) {
	c, err := t.q.GetAgentEventCursor(ctx, agentStaffID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return c, err
}

func (t tenantTx) AckAgentEvents(ctx context.Context, agentStaffID, upTo int64) (int64, error) {
	return t.q.AckAgentEvents(ctx, db.AckAgentEventsParams{AgentStaffID: agentStaffID, UpToID: upTo})
}

func (t tenantTx) ListEnabledAgentWebhookIDs(ctx context.Context) ([]int64, error) {
	return t.q.ListEnabledAgentWebhookIDs(ctx)
}

func webhookOf(id, staffID int64, url, secret string, enabled bool, created, updated pgtype.Timestamptz) AgentWebhook {
	return AgentWebhook{ID: id, AgentStaffID: staffID, URL: url, Secret: secret, Enabled: enabled,
		CreatedAt: created.Time, UpdatedAt: updated.Time}
}

func (t tenantTx) FindAgentWebhookByAgent(ctx context.Context, agentStaffID int64) (AgentWebhook, error) {
	r, err := t.q.GetAgentWebhookByAgent(ctx, agentStaffID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentWebhook{}, fmt.Errorf("agent %d: %w", agentStaffID, ErrAgentWebhookNotFound)
	}
	if err != nil {
		return AgentWebhook{}, err
	}
	return webhookOf(r.ID, r.AgentStaffID, r.Url, r.Secret, r.Enabled, r.CreatedAt, r.UpdatedAt), nil
}

func (t tenantTx) FindAgentWebhook(ctx context.Context, id int64) (AgentWebhook, error) {
	r, err := t.q.GetAgentWebhook(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentWebhook{}, fmt.Errorf("webhook %d: %w", id, ErrAgentWebhookNotFound)
	}
	if err != nil {
		return AgentWebhook{}, err
	}
	return webhookOf(r.ID, r.AgentStaffID, r.Url, r.Secret, r.Enabled, r.CreatedAt, r.UpdatedAt), nil
}

func (t tenantTx) InsertAgentWebhook(ctx context.Context, agentStaffID int64, url, secret string, enabled bool) (AgentWebhook, error) {
	r, err := t.q.InsertAgentWebhook(ctx, db.InsertAgentWebhookParams{AgentStaffID: agentStaffID, Url: url,
		Secret: secret, Enabled: enabled})
	if err != nil {
		return AgentWebhook{}, err
	}
	return webhookOf(r.ID, r.AgentStaffID, r.Url, r.Secret, r.Enabled, r.CreatedAt, r.UpdatedAt), nil
}

func (t tenantTx) UpdateAgentWebhook(ctx context.Context, agentStaffID int64, url string, enabled bool,
	secret *string) (AgentWebhook, error) {
	r, err := t.q.UpdateAgentWebhook(ctx, db.UpdateAgentWebhookParams{Url: url, Enabled: enabled, Secret: secret,
		AgentStaffID: agentStaffID})
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentWebhook{}, fmt.Errorf("agent %d: %w", agentStaffID, ErrAgentWebhookNotFound)
	}
	if err != nil {
		return AgentWebhook{}, err
	}
	return webhookOf(r.ID, r.AgentStaffID, r.Url, r.Secret, r.Enabled, r.CreatedAt, r.UpdatedAt), nil
}

func (t tenantTx) DeleteAgentWebhook(ctx context.Context, agentStaffID int64) error {
	n, err := t.q.DeleteAgentWebhook(ctx, agentStaffID)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("agent %d: %w", agentStaffID, ErrAgentWebhookNotFound)
	}
	return nil
}

func (t tenantTx) InsertAgentWebhookDelivery(ctx context.Context, d AgentWebhookDelivery) error {
	return t.q.InsertAgentWebhookDelivery(ctx, db.InsertAgentWebhookDeliveryParams{EventID: d.EventID,
		WebhookID: d.WebhookID, Attempt: d.Attempt, StatusCode: d.StatusCode, Error: d.Error})
}

func (t tenantTx) ListAgentWebhookDeliveries(ctx context.Context, webhookID int64, limit int32) ([]AgentWebhookDelivery, error) {
	rows, err := t.q.ListAgentWebhookDeliveries(ctx, db.ListAgentWebhookDeliveriesParams{WebhookID: webhookID, RowLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]AgentWebhookDelivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, AgentWebhookDelivery{ID: r.ID, EventID: r.EventID, WebhookID: webhookID, Attempt: r.Attempt,
			StatusCode: r.StatusCode, Error: r.Error, DeliveredAt: r.DeliveredAt.Time})
	}
	return out, nil
}
