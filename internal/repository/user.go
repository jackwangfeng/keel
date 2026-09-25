package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// ErrUserNotFound / ErrSessionNotFound 把 pgx.ErrNoRows 挡在本包里。
//
// 不让 service 去 errors.Is(err, pgx.ErrNoRows)：那会让业务层 import pgx，
// 而 pgx 上有 Begin —— 「业务层拿不到事务」这条编译期约束（见 tenant.go 的
// 包注释）就多了一个不必要的缺口。
//
// 两个错误分开，因为它们的处置不同：找不到用户是一次普通的登录失败（401），
// 找不到会话在 /auth/refresh 上是 401、在 /auth/logout 上却是 204
// （退出登录必须幂等）。
var (
	ErrUserNotFound    = errors.New("用户不存在")
	ErrSessionNotFound = errors.New("会话不存在、已过期或已吊销")
)

// User 是买家在 repository 边界上的形状。
//
// 与 Product 同理，它不是 sqlc 产物的别名：那会把「查询多 SELECT 一列」
// 直接变成 service 与 handler 能看到的类型变化，而 sqlc 的重生成是一条
// 不经人眼的自动化路径。
//
// **这里带着 PasswordHash。** 它是本层唯一一处「把凭据交给上层」的地方，
// 而且是必须的：口令比对要在 service 里做（业务规则），repository 不该知道
// argon2 是什么。代价是这个字段会顺着 User 一路往上，所以 handler 那边
// 组装响应时只能用 api.User（契约类型），它根本没有这个字段。
//
// 没有 MerchantID：这一层每一次读写都发生在一个设好 app.merchant_id 的事务里，
// 查出来的行必然属于当前租户。把它带上来只会制造第二个可能与 ctx 对不上的真相。
type User struct {
	ID           int64
	Phone        *string
	PasswordHash *string
	Nickname     string
	AvatarURL    *string
	Gender       int16
	Status       int16
	LastLoginAt  *time.Time
	CreatedAt    time.Time
}

// Session 是 user_tokens 里那一行的可用部分。
type Session struct {
	ID     int64
	UserID int64
}

// UserTx 是买家身份这一面。
//
// 单独一个接口而不是往 Tx 里平铺加方法：Tx 那份组合定义同时被好几条并发任务
// 碰到，平铺等于所有人改同一批行（见 product.go 里 Tx 的注释）。
type UserTx interface {
	// FindUserByPhone 按手机号取买家，用于密码登录。
	// 查不到返回 ErrUserNotFound —— 手机号不属于本店时 RLS 会挡掉，
	// 表现与「本店没有这个号码」完全一样，这是对的：登录接口不该能被
	// 用来探测「某个号码是不是别家店的用户」。
	FindUserByPhone(ctx context.Context, phone string) (User, error)

	// FindUserByID 按 id 取买家，用于刷新令牌时重新填响应里的 user。
	FindUserByID(ctx context.Context, id int64) (User, error)

	// TouchUserLogin 记一次登录时间。
	TouchUserLogin(ctx context.Context, userID int64) error

	// CreateSession 建一条会话，返回它的 id。tokenHash 是 sha256(refresh_token)。
	// 租户不在参数里 —— 那一列的默认值是 current_merchant()（见 00010）。
	CreateSession(ctx context.Context, userID int64, tokenHash []byte, expireAt time.Time) (int64, error)

	// FindLiveSession 按 hash 取一条**还活着**的会话（未吊销、未过期）。
	// 查不到返回 ErrSessionNotFound。
	FindLiveSession(ctx context.Context, tokenHash []byte) (Session, error)

	// RotateSession 把会话上的 hash 与到期时间换成新的（刷新时轮换）。
	// 并发的第二次刷新会拿到 ErrSessionNotFound —— 那一条的 hash 已经不在了。
	RotateSession(ctx context.Context, sessionID int64, tokenHash []byte, expireAt time.Time) error

	// RevokeSession 吊销一条会话（退出登录）。
	// 会话不存在或已被吊销过时返回 ErrSessionNotFound，由上层决定那算不算失败。
	RevokeSession(ctx context.Context, sessionID int64) error
}

func (t tenantTx) FindUserByPhone(ctx context.Context, phone string) (User, error) {
	// 传指针是因为 users.phone 可空，sqlc 于是把参数也做成了 *string。
	// 这里永远传一个非 nil 的指针：nil 会变成 `phone = NULL`，
	// 那个条件对任何行都不成立，于是「按空手机号登录」会安静地查不到人——
	// 与「查不到」同形，但原因完全不同。空手机号在 service 那一层就被挡掉了。
	row, err := t.q.GetUserByPhone(ctx, &phone)
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

func (t tenantTx) FindUserByID(ctx context.Context, id int64) (User, error) {
	row, err := t.q.GetUserByID(ctx, id)
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

func (t tenantTx) TouchUserLogin(ctx context.Context, userID int64) error {
	return t.q.TouchUserLogin(ctx, userID)
}

func (t tenantTx) CreateSession(ctx context.Context, userID int64, tokenHash []byte, expireAt time.Time) (int64, error) {
	return t.q.CreateUserToken(ctx, db.CreateUserTokenParams{
		UserID:    userID,
		TokenHash: tokenHash,
		ExpireAt:  pgtype.Timestamptz{Time: expireAt, Valid: true},
	})
}

func (t tenantTx) FindLiveSession(ctx context.Context, tokenHash []byte) (Session, error) {
	row, err := t.q.FindLiveUserToken(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return Session{ID: row.ID, UserID: row.UserID}, nil
}

func (t tenantTx) RotateSession(ctx context.Context, sessionID int64, tokenHash []byte, expireAt time.Time) error {
	_, err := t.q.RotateUserToken(ctx, db.RotateUserTokenParams{
		ID:        sessionID,
		TokenHash: tokenHash,
		ExpireAt:  pgtype.Timestamptz{Time: expireAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSessionNotFound
	}
	return err
}

func (t tenantTx) RevokeSession(ctx context.Context, sessionID int64) error {
	_, err := t.q.RevokeUserToken(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSessionNotFound
	}
	return err
}

// timePtr 把可空时间戳变成 *time.Time。
//
// 不返回零值 time.Time：那会让「从没登录过」在 JSON 里变成 0001-01-01T00:00:00Z，
// 一个看上去很像真实数据的假值。契约里 last_login_at 是可选字段，
// 没有就该整个不出现。
func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
