package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 后台身份这一层的三个错误。与买家那两个（ErrUserNotFound / ErrSessionNotFound）
// 分开，理由同那两个之间分开的理由：处置不同。
var (
	// ErrStaffNotFound：这个作用域里没有这个操作员。
	//
	// **「不在这个作用域里」和「根本不存在」在这一层是同一件事，这是对的。**
	// 契约里 /admin/ 那一段的约定 3 写着：「查不到当前租户名下的那一个，
	// 一律 404，不是 403 —— 403 会让自增 id 空间变成一个跨租户的存在性探针」。
	ErrStaffNotFound = errors.New("操作员不存在")

	// ErrStaffTokenNotFound：这串 token 没命中、过期了、被用过了或被撤销了。
	// 对调用方是同一件事：这串 token 不能用。分辨它们只会造出一个
	// 「这串 token 存在但过期了」的探针。
	ErrStaffTokenNotFound = errors.New("后台 token 无效、已过期或已被使用")

	// ErrStaffEmailTaken：这个作用域里已经有人用这个邮箱了（契约里那个 409）。
	//
	// 按**约束名**而不是「凡是 23505 都当重复」挑出来，理由与 payment.go 里
	// 那一处一字不差：后者会把别的唯一冲突也报成「邮箱重复」，而在这张表上
	// 「别的唯一冲突」就是 staff_tokens.token_hash 撞车 —— 那是一次熵源故障，
	// 报成「邮箱已被占用」会让排查从第一步就走错方向。
	//
	// 两条约束都要认：uk_staff_email 是商家级那条（merchant_id, email），
	// uk_staff_email_platform 是平台级那条（email，WHERE merchant_id IS NULL）。
	// 只认前者的话，两个平台管理员用同一个邮箱会回一个 500。
	ErrStaffEmailTaken = errors.New("该作用域内邮箱已存在")
)

// staffEmailConstraints 是 staff 上那两条邮箱唯一索引的名字（00017）。
var staffEmailConstraints = map[string]bool{
	"uk_staff_email":          true,
	"uk_staff_email_platform": true,
}

// asEmailTaken 把邮箱唯一冲突挑成 ErrStaffEmailTaken，别的错误原样上浮。
func asEmailTaken(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		staffEmailConstraints[pgErr.ConstraintName] {
		return ErrStaffEmailTaken
	}
	return err
}

// 一次性 token 与会话 token 的 kind，取值与数据模型 §14 的注释逐值一致。
const (
	StaffTokenBootstrap int16 = 1 // 引导，24 小时，stdout
	StaffTokenEmailLink int16 = 2 // 邮件登录链接，15 分钟
	StaffTokenSession   int16 = 3 // 会话，7 天，可反复使用、可撤销
)

// Staff 是后台操作员在 repository 边界上的形状。
//
// MerchantID 是 *int64 而不是 int64：nil 表示**平台级操作员**（数据模型 §14），
// 而 0 不是。这里刻意不把它压成 0 —— 0 在别的每一处都表示「没设」，
// 而平台级是一个明确的、有意义的身份，两者混在一个零值里的后果是
// 「忘了设租户」和「这是平台管理员」长得一模一样。
//
// 与 User 不同，这里**没有**任何凭据字段：整个后台不存在密码（§14），
// token 只有 sha256 进库，而那个 hash 从不离开本层。
type Staff struct {
	ID          int64
	MerchantID  *int64
	Email       string
	Name        string
	Role        int16
	Status      int16
	LastLoginAt *time.Time
	CreatedAt   time.Time
}

// StaffSession 是一条会话校验通过之后，调用方需要知道的全部。
//
// Role 与 Status 从库里读，**不从令牌里读**：一个降级或停用的操作员必须在
// 下一个请求就失去权限，而会话默认 7 天。
type StaffSession struct {
	SessionID  int64
	StaffID    int64
	MerchantID *int64
	Role       int16
	Status     int16
}

// StaffOneTimeToken 是一串一次性 token 命中之后的结果：它是谁的、哪一种。
type StaffOneTimeToken struct {
	TokenID int64
	Kind    int16
	Staff   Staff
}

// BootstrapState 回答「引导通道开着没有」。两个数来自同一个快照，见 staff.sql。
type BootstrapState struct {
	// Admins 是本作用域里**真正能登录**的在岗管理员数 —— 不含那个还没兑换过的
	// 引导占位账号。平台作用域里调用它，数的就是平台级管理员。
	// 为什么不含占位账号，见 db/queries/staff.sql 的 BootstrapChannelState。
	Admins int64
	// PlaceholderID 是还没兑换过的引导占位账号，没有则为 0。
	PlaceholderID int64
	// LiveTokens 是还没被用掉的引导 token 数。它决定 token 对不上时
	// 回 401（窗口还开着，你这串是错的）还是 409（窗口关了）。
	LiveTokens int64
}

// StaffTx 是后台身份这一面。
//
// 单独一个接口而不是往 Tx 里平铺加方法，理由同 UserTx：Tx 那份组合定义同时被
// 好几条并发任务碰到，平铺等于所有人改同一批行。
//
// **它同时是 WithPlatform 交出去的全部能力。** 那不是巧合：平台作用域里
// 除了 staff 与 staff_tokens，别的表一碰就是一次 42501（00017 文件头），
// 所以那条路径能拿到的接口就该只有这些方法 —— 让错误的写法写不出来，
// 而不是让它跑起来才报错。
type StaffTx interface {
	// BootstrapChannelState 读引导通道的状态。只在平台作用域里有意义。
	// placeholderEmail 是引导占位账号的邮箱（service 里那个常量），用来把它
	// 从「能登录的管理员」里排除掉。
	BootstrapChannelState(ctx context.Context, placeholderEmail string) (BootstrapState, error)

	// CreateStaff 建一个操作员。**租户不在参数里** —— 那一列的默认值是
	// staff_scope_merchant()，也就是本事务的作用域（00017）。
	// createdBy 为 nil 表示引导账号（§14 的 DDL 注释）。
	CreateStaff(ctx context.Context, email, name string, role int16, createdBy *int64) (Staff, error)

	// FindStaff 按 id 取一个没被软删的操作员。查不到返回 ErrStaffNotFound。
	FindStaff(ctx context.Context, id int64) (Staff, error)

	// ListStaff / CountStaff 是员工列表那一页。
	// 「平台级看见平台操作员，商家级只看见自己店的」由作用域与 RLS 给出。
	ListStaff(ctx context.Context, limit, offset int64) ([]Staff, error)
	CountStaff(ctx context.Context) (int64, error)

	// SetStaffEmail 把占位邮箱换成本人的邮箱（引导流程）。
	// 邮箱已经被补过时返回 ErrStaffNotFound —— 这条路只走一次。
	SetStaffEmail(ctx context.Context, id int64, newEmail, oldEmail string) (Staff, error)

	// UpdateStaffRoleStatus 改角色 / 状态，两个都是「给了就改」。
	UpdateStaffRoleStatus(ctx context.Context, id int64, role, status *int16) (Staff, error)

	// CountOtherLiveAdmins 数除了这个人之外本作用域里还有几个在岗管理员。
	// 兑现 §14 那条「进不了数据库的约束」。
	CountOtherLiveAdmins(ctx context.Context, id int64) (int64, error)

	// TouchStaffLogin 记一次登录时间。
	TouchStaffLogin(ctx context.Context, id int64) error

	// CreateStaffToken 签发一串 token，返回 staff_tokens 那一行的 id。
	// tokenHash 是 sha256(明文) 的十六进制 —— 明文不进这一层。
	CreateStaffToken(ctx context.Context, staffID int64, tokenHash string, kind int16, expireAt time.Time) (int64, error)

	// FindLiveOneTimeToken 按 hash 取一串还活着的一次性 token（kind 1/2）。
	// 查不到返回 ErrStaffTokenNotFound。
	FindLiveOneTimeToken(ctx context.Context, tokenHash string, kind int16) (StaffOneTimeToken, error)

	// ConsumeStaffToken 把一次性 token 标记为已使用。
	// 并发的第二次会拿到 ErrStaffTokenNotFound —— 那一行已经被用掉了。
	ConsumeStaffToken(ctx context.Context, tokenID int64) error

	// TouchLiveStaffSession 校验一条会话 token 并记一次 last_seen_at。
	// 校验与续活是同一条语句，理由见 db/queries/staff.sql。
	TouchLiveStaffSession(ctx context.Context, tokenHash string) (StaffSession, error)
}

// WithPlatform 在一个**平台级作用域**的事务里执行 fn。
//
// ===========================================================================
// 它与 WithTenant 的差别只有一句 set_config，而那一句是整个两级身份模型的落点
// ===========================================================================
//
// 数据模型 §14 把 staff.merchant_id 设计成可空：NULL = 平台级操作员，
// 他不属于任何一家店。而 RLS 的谓词必须对这一行也成立，否则平台级操作员
// 对所有人不可见，包括他自己。00017 的解法是让「当前作用域的租户」本身
// 可以是 NULL：
//
//	merchant_id IS NOT DISTINCT FROM staff_scope_merchant()
//
// staff_scope_merchant() 在这里返回 NULL（因为 app.platform_scope = 'on'），
// 于是这个事务看得见、也只看得见 merchant_id IS NULL 的那些行。
//
// **它刻意不设 app.merchant_id。** 于是这个事务里一旦碰到别的租户表
// （orders、products…），那些表的策略会调 current_merchant() 而当场报
// 42501（00002 里那条会说人话的 RAISE）。平台路径误读业务表时是一条错误，
// 不是一个看上去很像「这家店没有数据」的空结果集。
//
// fn 收到的是 StaffTx 而不是 Tx：平台作用域里别的表一碰就是 42501，
// 所以这条路径能拿到的接口就该只有后台身份那几个方法。
//
// 它也不需要 ctx 里有租户 —— 平台级请求的 Host 可以是任何一家店，
// 而那个租户与这个事务无关。
func (r *Repo) WithPlatform(ctx context.Context, fn func(StaffTx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// is_local = true。会话级的话这条设置会留在连接上，被池交给下一个请求时
	// 就是一次**提权**：那个请求会以平台级作用域运行。比 WithTenant 里
	// 同一个问题更糟 —— 那边泄露的是某一家店，这边泄露的是「不属于任何店」
	// 那一抽屉的写权限。
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.platform_scope', 'on', true)`); err != nil {
		return err
	}

	// scope 传 nil：平台级作用域按定义不属于任何一家店（数据模型 §14），
	// 而这一层交出去的 Staff.MerchantID 正是从它来的。
	if err := fn(tenantTx{q: db.New(tx), scope: nil}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// tenantTx 上的实现。同一份实现同时服务两种作用域 —— 它们的差别全在
// set_config 那一句和 RLS 里，SQL 与 Go 都不知道自己跑在哪一种作用域里。
// 那正是想要的：作用域没有任何一处可以被调用方传错。
// ---------------------------------------------------------------------------

// staffOf 把一行 staff 装成领域类型。
//
// MerchantID **不来自那一行**，来自事务的作用域（见 tenantTx.scope）。
// 查询里因此一个 merchant_id 都没有，而这不只是为了迁就
// scripts/check_query_tenancy.py：RLS 保证查出来的行必然属于当前作用域，
// 所以那一列在这一层是推得出来的，从行里再读一遍只会多一个真相源。
func (t tenantTx) staffOf(id int64, email, name string, role, status int16,
	lastLogin, created pgtype.Timestamptz) Staff {
	return Staff{
		ID:          id,
		MerchantID:  t.scope,
		Email:       email,
		Name:        name,
		Role:        role,
		Status:      status,
		LastLoginAt: timePtr(lastLogin),
		CreatedAt:   created.Time,
	}
}

func (t tenantTx) BootstrapChannelState(ctx context.Context, placeholderEmail string) (BootstrapState, error) {
	row, err := t.q.BootstrapChannelState(ctx, placeholderEmail)
	if err != nil {
		return BootstrapState{}, err
	}
	return BootstrapState{
		Admins: row.Admins, PlaceholderID: row.PlaceholderID, LiveTokens: row.LiveTokens,
	}, nil
}

func (t tenantTx) CreateStaff(ctx context.Context, email, name string, role int16, createdBy *int64) (Staff, error) {
	row, err := t.q.CreateStaff(ctx, db.CreateStaffParams{
		Email: email, Name: name, Role: role, CreatedBy: createdBy,
	})
	if err != nil {
		return Staff{}, asEmailTaken(err)
	}
	return t.staffOf(row.ID, row.Email, row.Name, row.Role, row.Status,
		row.LastLoginAt, row.CreatedAt), nil
}

func (t tenantTx) FindStaff(ctx context.Context, id int64) (Staff, error) {
	row, err := t.q.GetStaffByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ErrStaffNotFound
	}
	if err != nil {
		return Staff{}, err
	}
	return t.staffOf(row.ID, row.Email, row.Name, row.Role, row.Status,
		row.LastLoginAt, row.CreatedAt), nil
}

func (t tenantTx) ListStaff(ctx context.Context, limit, offset int64) ([]Staff, error) {
	// 到这里还越界只可能是上游的钳制没生效。报错而不是截断，同 ListProducts。
	if limit < 0 || limit > math.MaxInt32 {
		return nil, fmt.Errorf("limit %d 超出范围 [0, %d]", limit, math.MaxInt32)
	}
	if offset < 0 || offset > math.MaxInt32 {
		return nil, fmt.Errorf("offset %d 超出范围 [0, %d]", offset, math.MaxInt32)
	}
	rows, err := t.q.ListStaff(ctx, db.ListStaffParams{
		Limit: int32(limit), Offset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Staff, 0, len(rows))
	for _, row := range rows {
		out = append(out, t.staffOf(row.ID, row.Email, row.Name,
			row.Role, row.Status, row.LastLoginAt, row.CreatedAt))
	}
	return out, nil
}

func (t tenantTx) CountStaff(ctx context.Context) (int64, error) {
	return t.q.CountStaff(ctx)
}

func (t tenantTx) SetStaffEmail(ctx context.Context, id int64, newEmail, oldEmail string) (Staff, error) {
	row, err := t.q.SetStaffEmail(ctx, db.SetStaffEmailParams{
		ID: id, Email: newEmail, Email_2: oldEmail,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ErrStaffNotFound
	}
	if err != nil {
		return Staff{}, asEmailTaken(err)
	}
	return t.staffOf(row.ID, row.Email, row.Name, row.Role, row.Status,
		row.LastLoginAt, row.CreatedAt), nil
}

func (t tenantTx) UpdateStaffRoleStatus(ctx context.Context, id int64, role, status *int16) (Staff, error) {
	row, err := t.q.UpdateStaffRoleStatus(ctx, db.UpdateStaffRoleStatusParams{
		ID: id, Role: role, Status: status,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ErrStaffNotFound
	}
	if err != nil {
		return Staff{}, err
	}
	return t.staffOf(row.ID, row.Email, row.Name, row.Role, row.Status,
		row.LastLoginAt, row.CreatedAt), nil
}

func (t tenantTx) CountOtherLiveAdmins(ctx context.Context, id int64) (int64, error) {
	return t.q.CountOtherLiveAdmins(ctx, id)
}

func (t tenantTx) TouchStaffLogin(ctx context.Context, id int64) error {
	return t.q.TouchStaffLogin(ctx, id)
}

func (t tenantTx) CreateStaffToken(ctx context.Context, staffID int64, tokenHash string,
	kind int16, expireAt time.Time) (int64, error) {
	return t.q.CreateStaffToken(ctx, db.CreateStaffTokenParams{
		StaffID:   staffID,
		TokenHash: tokenHash,
		Kind:      kind,
		ExpireAt:  pgtype.Timestamptz{Time: expireAt, Valid: true},
	})
}

func (t tenantTx) FindLiveOneTimeToken(ctx context.Context, tokenHash string, kind int16) (StaffOneTimeToken, error) {
	row, err := t.q.FindLiveOneTimeStaffToken(ctx, db.FindLiveOneTimeStaffTokenParams{
		TokenHash: tokenHash, Kind: kind,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffOneTimeToken{}, ErrStaffTokenNotFound
	}
	if err != nil {
		return StaffOneTimeToken{}, err
	}
	return StaffOneTimeToken{
		TokenID: row.ID,
		Kind:    row.Kind,
		Staff: t.staffOf(row.StaffID, row.Email, row.Name,
			row.Role, row.Status, row.LastLoginAt, row.CreatedAt),
	}, nil
}

func (t tenantTx) ConsumeStaffToken(ctx context.Context, tokenID int64) error {
	_, err := t.q.ConsumeStaffToken(ctx, tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStaffTokenNotFound
	}
	return err
}

func (t tenantTx) TouchLiveStaffSession(ctx context.Context, tokenHash string) (StaffSession, error) {
	row, err := t.q.TouchLiveStaffSession(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffSession{}, ErrStaffTokenNotFound
	}
	if err != nil {
		return StaffSession{}, err
	}
	return StaffSession{
		SessionID:  row.ID,
		StaffID:    row.StaffID,
		MerchantID: t.scope,
		Role:       row.Role,
		Status:     row.Status,
	}, nil
}
