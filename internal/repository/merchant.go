package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/repository/internal/db"
)

// ErrMerchantCodeTaken：这个 code 已经有店在用了（契约 POST /admin/merchants
// 的 409）。
//
// 按**约束名**而不是「凡是 23505 都当 code 重复」挑出来，理由与 staff.go 里
// asEmailTaken 那一处一字不差：后者会把别的唯一冲突也报成「code 被占用」，
// 而在这条链路上「别的唯一冲突」就是新店第一个管理员的邮箱撞车 ——
// 报成「店名已存在」会让调用者去改 code，改多少次都没用。
var ErrMerchantCodeTaken = errors.New("商家 code 已被占用")

// merchantsCodeConstraint 是 merchants.code 上那条唯一约束的名字。
// 它由 00001 的 `code TEXT NOT NULL UNIQUE` 隐式生成，所以名字是 PostgreSQL
// 的默认拼法而不是我们起的。
const merchantsCodeConstraint = "merchants_code_key"

// Merchant 是 merchants 那一行在 repository 边界上的形状。
//
// 没有 Domain：那一列在 shop_settings 上，而开店这条路不写它
// （00021 文件头「为什么不连 shop_settings 一起给」）。
type Merchant struct {
	ID        int64
	Code      string
	Name      string
	Status    int16
	CreatedAt time.Time

	// Domain 是 shop_settings.domain（自定义域名），没绑时为 nil。
	// 只有商家目录的读接口（merchant_directory.go）填它；开店这条路不写 shop_settings。
	Domain *string
	// RevisedAt 是最新一行 merchant_revisions 的时间；从没改过名 / 状态时为 nil。
	RevisedAt *time.Time
}

// WithNewTenant 在**一个**事务里建一家新店，把作用域切到这家新店，再执行 fn。
//
// ===========================================================================
// 它就是那个缺失的第三个入口：「在指定租户里开一个事务」
// ===========================================================================
//
// 此前 repository 只有两个入口，而它们都**拿不到**开店要的那个作用域：
//
//	WithTenant   租户从 ctx 取。开店时 ctx 里那个租户是请求 Host 解析出来的
//	             **某一家已有的店** —— 用它建管理员，新管理员会落到别人家里。
//	WithPlatform 平台作用域（app.platform_scope = 'on'）。在它里面
//	             staff_scope_merchant() 返回 NULL，建出来的是一个**平台级**
//	             管理员，拥有跨租户运维权，而不是这家店的老板。
//
// 缺的那一个是「租户由参数给定，而那个参数是这个事务自己刚刚造出来的」。
// 它不能拆成「先建店、提交，再 WithTenant 建管理员」两步：那两步之间崩一次，
// 留下的是一家谁也进不去的店 —— 没有 staff 就没有后台会话，而应用侧在
// merchants 上没有 DELETE 权，连收拾都收拾不了。
//
// ===========================================================================
// 形状照 WithSagaBranch，不是照 WithTenant
// ===========================================================================
//
// 也就是说：**事务边界由 repository 自己持有，pgx.Tx 一步都不出本包。**
// 交出去的是 StaffTx —— 新店里此刻只有「建第一个管理员、给他签一串一次性
// 登录 token」这一件事可做，而那两个方法正好都在 StaffTx 上。给 fn 一个完整
// 的 Tx 等于让「开店的同时顺手建一件商品」成为一句写得出来的代码，而那件事
// 没有任何业务需要，只会让这个入口慢慢长成第二个 WithTenant。
//
// 两次作用域切换，顺序是硬的：
//
//	① 平台作用域   建 merchants 那一行。这一段里碰任何业务表都是 42501
//	              （那些表的策略要调 current_merchant()，而它此时未设 → RAISE）。
//	              merchants 自己没有 RLS，所以它在这一段里是可写的
//	              —— 前提是 keel_app 有 INSERT，那是 00021 的事。
//	② 新店的租户作用域  enterTenantScope 把 app.merchant_id 设成刚拿到的 id，
//	              **并把 app.platform_scope 显式关掉**。第二件事不是冗余：
//	              不关的话 staff_scope_merchant() 仍然返回 NULL，
//	              新店的第一个管理员会被建成一个平台级管理员。
//	              那一条不报错、也没有任何约束拦得住，所以它写在
//	              enterTenantScope 里，而不是写在这里 —— 让下一个用这个入口
//	              的人不必知道有这回事。
//
// 它**不读 ctx 里的租户**，和 WithPlatform 一样：开店请求的 Host 可以是任何
// 一家店，那家店与这个事务无关。
func (r *Repo) WithNewTenant(ctx context.Context, code, name string,
	fn func(Merchant, StaffTx) error) (Merchant, error) {

	if fn == nil {
		// 与 WithSagaBranch 同一条：在开事务之前拒绝。一个 nil 的 fn 意味着
		// 「建了店但没建管理员」，而那正是这个入口存在的全部理由。
		return Merchant{}, errors.New("开店缺少「在新店作用域里做什么」的那一段")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Merchant{}, err
	}
	// Commit 之后再 Rollback 是无害的 no-op（pgx 返回 ErrTxClosed）。
	defer tx.Rollback(ctx)

	// ① 平台作用域。is_local = true 的理由见 WithPlatform 上那一段：
	// 会话级的话这条设置会留在连接上，被池交给下一个请求就是一次提权。
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.platform_scope', 'on', true)`); err != nil {
		return Merchant{}, err
	}

	q := db.New(tx)
	row, err := q.CreateMerchant(ctx, db.CreateMerchantParams{Code: code, Name: name})
	if err != nil {
		return Merchant{}, asMerchantCodeTaken(err)
	}
	m := Merchant{
		ID: row.ID, Code: row.Code, Name: row.Name,
		Status: row.Status, CreatedAt: row.CreatedAt.Time,
	}

	// ② 切到新店的租户作用域。
	if err := enterTenantScope(ctx, tx, m.ID); err != nil {
		return Merchant{}, err
	}

	// scope 取一份副本的地址，理由同 withTenantTx 里那一处。
	scope := m.ID
	if err := fn(m, tenantTx{q: q, scope: &scope}); err != nil {
		return Merchant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Merchant{}, err
	}
	return m, nil
}

// asMerchantCodeTaken 把 code 唯一冲突挑成 ErrMerchantCodeTaken，
// 别的错误原样上浮。
func asMerchantCodeTaken(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == merchantsCodeConstraint {
		return ErrMerchantCodeTaken
	}
	return err
}
