package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// ProfilePatch 是 PATCH /me 的三个字段。nil 表示「不改」。
//
// AvatarURL 多一层：SetAvatar 为真时才动那一列，此时 AvatarURL 为 nil 表示清空。
// 「清空头像」与「不改头像」在一个可空字段上分不开，所以要这个开关。
type ProfilePatch struct {
	Nickname  *string
	Gender    *int16
	SetAvatar bool
	AvatarURL *string
}

// Identity 是 user_identities 的对外可见部分。
//
// **没有 ExternalID、没有 UnionID**：契约写明它们是渠道敏感标识，一律不对外返回。
// 这一层不把它们取出来（db/queries/users.sql 的 ListUserIdentities 只 SELECT
// union_id 是否为空），上层就没有机会漏出去。
type Identity struct {
	ID         int64
	Provider   int16
	HasUnionID bool
	CreatedAt  time.Time
}

// ErrIdentityNotFound：这个买家在该 provider 下没有绑定。
var ErrIdentityNotFound = errors.New("没有这个第三方身份")

// ProfileTx 是 /me 这一面。每个方法都按 userID 走，没有一个能指名别的买家。
type ProfileTx interface {
	// UpdateProfile 改资料，返回改后的买家。买家已注销返回 ErrUserNotFound。
	UpdateProfile(ctx context.Context, userID int64, p ProfilePatch) (User, error)
	// ListIdentities 已绑定的第三方身份，按绑定先后。
	ListIdentities(ctx context.Context, userID int64) ([]Identity, error)
	// CountIdentitiesExcept 这个买家在 provider 之外还有几条身份。
	CountIdentitiesExcept(ctx context.Context, userID int64, provider int16) (int64, error)
	// DeleteIdentities 解绑 provider 下的全部身份。一条都没有返回 ErrIdentityNotFound。
	DeleteIdentities(ctx context.Context, userID int64, provider int16) error
}

func (t tenantTx) UpdateProfile(ctx context.Context, userID int64, p ProfilePatch) (User, error) {
	row, err := t.q.UpdateUserProfile(ctx, db.UpdateUserProfileParams{
		Nickname:  p.Nickname,
		Gender:    p.Gender,
		SetAvatar: p.SetAvatar,
		AvatarUrl: p.AvatarURL,
		ID:        userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, err
	}
	return User{
		ID:           row.ID,
		Phone:        row.Phone,
		PasswordHash: row.PasswordHash,
		Nickname:     row.Nickname,
		AvatarURL:    row.AvatarUrl,
		Gender:       row.Gender,
		Status:       row.Status,
		LastLoginAt:  timePtr(row.LastLoginAt),
		CreatedAt:    row.CreatedAt.Time,
	}, nil
}

func (t tenantTx) ListIdentities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := t.q.ListUserIdentities(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Identity, 0, len(rows))
	for _, r := range rows {
		out = append(out, Identity{
			ID: r.ID, Provider: r.Provider, HasUnionID: r.HasUnionID, CreatedAt: r.CreatedAt.Time,
		})
	}
	return out, nil
}

func (t tenantTx) CountIdentitiesExcept(ctx context.Context, userID int64, provider int16) (int64, error) {
	return t.q.CountUserIdentitiesExcept(ctx, db.CountUserIdentitiesExceptParams{UserID: userID, Provider: provider})
}

func (t tenantTx) DeleteIdentities(ctx context.Context, userID int64, provider int16) error {
	n, err := t.q.DeleteUserIdentities(ctx, db.DeleteUserIdentitiesParams{UserID: userID, Provider: provider})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrIdentityNotFound
	}
	return nil
}
