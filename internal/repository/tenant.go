// Package repository 是数据访问层，也是业务代码进入数据库的唯一入口。
//
// sqlc 产物在 internal/db 之下，Go 的 internal 规则让 internal/repository/ 之外的
// 包 import 不到它。于是 handler 与 service 只能经由 WithTenant 拿到 Tx，
// 而 WithTenant 保证每一次访问都发生在一个设过 app.merchant_id 的事务里。
//
// 这条约束必须靠编译器而不是靠自觉：sqlc 的 DBTX 接口同时被 *pgxpool.Pool 和
// pgx.Tx 满足，所以 db.New(pool).ListProducts(ctx, ...) 是能编译过的一句话，
// 而它不在事务里，也就没有 SET LOCAL —— 真实症状不是「越权读到别家数据」
// （RLS 会挡住），而是线上偶发 42501，比越权更难定位到根因。
package repository

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/repository/internal/db"
	"github.com/keel/keel/internal/tenant"
)

// Repo 持有连接池。池请用 db.NewPool 建：它把 RLS 自检挂在每条物理连接上。
type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// WithTenant 在一个设好租户上下文的事务里执行 fn。
//
// 租户从 ctx 取，不从参数传 —— 调用方没有那个参数可以传错。
//
// fn 收到的是 Tx（接口），不是 *db.Queries。理由见 product.go 里 Tx 的注释：
// 生成代码上的导出方法 WithTx(pgx.Tx) 会让「自己 Begin 一个没设租户的事务」
// 重新变成一句能编译的话，而接口让那个方法在业务层根本不存在。
func (r *Repo) WithTenant(ctx context.Context, fn func(Tx) error) error {
	return r.withTenantTx(ctx, func(_ pgx.Tx, q Tx) error { return fn(q) })
}

// withTenantTx 是「开事务 → 设租户 → 跑 fn → 提交」这一串的**唯一**实现。
//
// 包内的 fn 除了 Tx 还拿得到 pgx.Tx，因为子事务屏障要在同一个事务里发自己那条
// INSERT（见 saga.go）。它是包内的：pgx.Tx 到不了本包之外，Tx 才是交出去的东西。
//
// 合成一处而不是让 WithSagaBranch 自己再写一遍 Begin + set_config：那句
// set_config 是整个租户隔离的落点，两份实现意味着将来有人只改对其中一份，
// 而漏掉的那一份的症状是线上偶发 42501。
func (r *Repo) withTenantTx(ctx context.Context, fn func(pgx.Tx, Tx) error) error {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	// Commit 之后再 Rollback 是无害的 no-op（pgx 返回 ErrTxClosed），
	// 所以这条 defer 只在提前 return 的路径上真正起作用。
	defer tx.Rollback(ctx)

	// 用 set_config(..., is_local => true) 而不是 SET LOCAL 拼字符串。
	//
	// 两者语义等价——第三个参数 true 就是 LOCAL，作用域到事务结束为止——
	// 但 SET LOCAL 不吃绑定参数，只能 fmt.Sprintf 拼进 SQL。这里的 merchantID
	// 是 int64 且来自中间件，眼下确实没有注入面，可那是个随时会失效的前提：
	// 哪天租户标识从 int64 变成 code（TEXT），拼接就地变成漏洞，而那次改动
	// 看上去只是换了个类型。set_config 让这个前提根本不必存在。
	//
	// 必须是 local 而不是会话级：会话级的设置会留在连接上，被池交给下一个
	// 请求时就是一次跨租户泄露。internal/repository/tenant_test.go 里的
	// TestTenantSettingDiesWithTheTransaction 钉住这一点（把 true 改成 false 会红）。
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.merchant_id', $1, true)`,
		strconv.FormatInt(merchantID, 10)); err != nil {
		return err
	}

	if err := fn(tx, tenantTx{q: db.New(tx)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
