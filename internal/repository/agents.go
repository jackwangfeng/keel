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

// AI 员工与接入密钥（00090，AI 经营 M9）。AI 员工是 kind = 2 的 staff；人的员工查询只认 kind = 1，
// 这里只认 kind = 2，两条路互不相通。

// ErrAgentNotFound：这个 id 不是本店的 AI 员工（可能是人、可能不存在）。
var ErrAgentNotFound = errors.New("AI 员工不存在")

// ErrAgentKeyNotFound：密钥查不到、已吊销或已过期 —— 对外一律同一个 401。
var ErrAgentKeyNotFound = errors.New("接入密钥无效")

// Agent 是一名 AI 员工。
type Agent struct {
	ID         int64
	Name       string
	Role       int16
	Status     int16
	CreatedAt  time.Time
	LiveKeys   int64
	LastUsedAt *time.Time
}

// AgentKey 是一把接入密钥的元数据（不含明文，明文不落库）。
type AgentKey struct {
	ID         int64
	StaffID    int64
	Name       string
	Prefix     string
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// AgentKeyAuth 是密钥解析出的身份：给 MCP 鉴权中间件拼 StaffIdentity 用。
type AgentKeyAuth struct {
	KeyID   int64
	StaffID int64
	Role    int16
	Status  int16
}

// AgentTx 是 AI 员工那一面。嵌在 StaffTx 里：AI 员工的管辖范围复用 staff_scopes 的读写。
type AgentTx interface {
	CreateAgentStaff(ctx context.Context, email, name string, role int16, createdBy int64) (Agent, error)
	FindAgent(ctx context.Context, id int64) (Agent, error)
	ListAgents(ctx context.Context) ([]Agent, error)
	UpdateAgent(ctx context.Context, id int64, name *string, role, status *int16) (Agent, error)
	CreateAgentKey(ctx context.Context, staffID int64, name, prefix, secretHash string,
		expiresAt *time.Time, createdBy int64) (AgentKey, error)
	ListAgentKeys(ctx context.Context, staffID int64) ([]AgentKey, error)
	// RevokeAgentKey 吊销；已吊销或不属于这名 AI 员工返回 ErrAgentKeyNotFound。
	RevokeAgentKey(ctx context.Context, staffID, keyID int64) error
	LoadAgentKey(ctx context.Context, secretHash string) (AgentKeyAuth, error)
	TouchAgentKey(ctx context.Context, keyID int64) error
}

func tsPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

func (t tenantTx) CreateAgentStaff(ctx context.Context, email, name string, role int16, createdBy int64) (Agent, error) {
	r, err := t.q.CreateAgentStaff(ctx, db.CreateAgentStaffParams{Email: email, Name: name, Role: role, CreatedBy: &createdBy})
	if err != nil {
		return Agent{}, err
	}
	return Agent{ID: r.ID, Name: r.Name, Role: r.Role, Status: r.Status, CreatedAt: r.CreatedAt.Time}, nil
}

func (t tenantTx) FindAgent(ctx context.Context, id int64) (Agent, error) {
	r, err := t.q.GetAgent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, fmt.Errorf("agent %d: %w", id, ErrAgentNotFound)
	}
	if err != nil {
		return Agent{}, err
	}
	return Agent{ID: r.ID, Name: r.Name, Role: r.Role, Status: r.Status, CreatedAt: r.CreatedAt.Time}, nil
}

func (t tenantTx) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := t.q.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(rows))
	for _, r := range rows {
		out = append(out, Agent{ID: r.ID, Name: r.Name, Role: r.Role, Status: r.Status,
			CreatedAt: r.CreatedAt.Time, LiveKeys: r.LiveKeys, LastUsedAt: tsPtr(r.LastUsedAt)})
	}
	return out, nil
}

func (t tenantTx) UpdateAgent(ctx context.Context, id int64, name *string, role, status *int16) (Agent, error) {
	r, err := t.q.UpdateAgent(ctx, db.UpdateAgentParams{ID: id, Name: name, Role: role, Status: status})
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, fmt.Errorf("agent %d: %w", id, ErrAgentNotFound)
	}
	if err != nil {
		return Agent{}, err
	}
	return Agent{ID: r.ID, Name: r.Name, Role: r.Role, Status: r.Status, CreatedAt: r.CreatedAt.Time}, nil
}

func (t tenantTx) CreateAgentKey(ctx context.Context, staffID int64, name, prefix, secretHash string,
	expiresAt *time.Time, createdBy int64) (AgentKey, error) {
	exp := pgtype.Timestamptz{}
	if expiresAt != nil {
		exp = pgtype.Timestamptz{Time: *expiresAt, Valid: true}
	}
	r, err := t.q.CreateAgentKey(ctx, db.CreateAgentKeyParams{StaffID: staffID, Name: name, Prefix: prefix,
		SecretHash: secretHash, ExpiresAt: exp, CreatedBy: createdBy})
	if err != nil {
		return AgentKey{}, err
	}
	return AgentKey{ID: r.ID, StaffID: r.StaffID, Name: r.Name, Prefix: r.Prefix, ExpiresAt: tsPtr(r.ExpiresAt),
		RevokedAt: tsPtr(r.RevokedAt), LastUsedAt: tsPtr(r.LastUsedAt), CreatedAt: r.CreatedAt.Time}, nil
}

func (t tenantTx) ListAgentKeys(ctx context.Context, staffID int64) ([]AgentKey, error) {
	rows, err := t.q.ListAgentKeys(ctx, staffID)
	if err != nil {
		return nil, err
	}
	out := make([]AgentKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, AgentKey{ID: r.ID, StaffID: r.StaffID, Name: r.Name, Prefix: r.Prefix,
			ExpiresAt: tsPtr(r.ExpiresAt), RevokedAt: tsPtr(r.RevokedAt), LastUsedAt: tsPtr(r.LastUsedAt),
			CreatedAt: r.CreatedAt.Time})
	}
	return out, nil
}

func (t tenantTx) RevokeAgentKey(ctx context.Context, staffID, keyID int64) error {
	n, err := t.q.RevokeAgentKey(ctx, db.RevokeAgentKeyParams{ID: keyID, StaffID: staffID})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("agent key %d: %w", keyID, ErrAgentKeyNotFound)
	}
	return nil
}

func (t tenantTx) LoadAgentKey(ctx context.Context, secretHash string) (AgentKeyAuth, error) {
	r, err := t.q.LoadAgentKey(ctx, secretHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentKeyAuth{}, ErrAgentKeyNotFound
	}
	if err != nil {
		return AgentKeyAuth{}, err
	}
	return AgentKeyAuth{KeyID: r.KeyID, StaffID: r.StaffID, Role: r.Role, Status: r.Status}, nil
}

func (t tenantTx) TouchAgentKey(ctx context.Context, keyID int64) error {
	return t.q.TouchAgentKey(ctx, keyID)
}
