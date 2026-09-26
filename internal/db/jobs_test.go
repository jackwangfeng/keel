package db_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/db"
)

// jobs 这张表**没有 RLS**，所以租户隔离全靠一个列默认值。这里盯的就是它。
//
// ===========================================================================
// 为什么这一条要单独在 internal/db 里测，而不是只在 repository 那一层
// ===========================================================================
//
// 00022 的文件头第一节把这张表的三道防线逐条列了出来，并说「①③ 是结构性的，
// ② 靠的是纪律」。第 ① 道 ——「入队写不出别人的租户」—— 是三道里最硬的一道，
// 而它的全部实现就是 DDL 里的一句
//
//	merchant_id BIGINT NOT NULL DEFAULT current_merchant() REFERENCES ...
//
// 别的表上这件事有 RLS 的 WITH CHECK 兜底：默认值填错了，策略还会把这一行挡回去。
// **这张表上没有第二道。** 把那个 DEFAULT 删掉之后，`NOT NULL` 会让入队
// 直接失败，看上去像是「坏得很响」—— 但那只在它真的被删掉时成立。
// 把它改成一个常量（`DEFAULT 1`）就不是了：入队照常成功，每一条任务都记在
// 1 号商家名下，而 repository 那一层的测试全部照绿，因为它们每次只看一家店。
//
// 这一条测的是**库的形状**（同一条 INSERT 在不同租户上下文里落到不同的租户，
// 在没有上下文时当场失败），所以它属于 internal/db —— 与 rls_test.go 里
// 「没设租户要报错而不是放行」是同一类断言，只是这张表走的是另一条路
// 达到同一个结论。00022 的文件头与 db/tenancy.json 的 jobs 条目都点名了它。

// enqueueSQL 是 db/queries/jobs.sql 的 EnqueueJob，逐字一致。
//
// 这里重抄一遍而不是调用 sqlc 的产物，是刻意的：产物挂在 tenantTx 上，
// 而这条测试要做的事情之一恰恰是**不进**租户事务地跑一次它
// （那是 tenantTx 表达不出来的状态）。两处必须一致这件事由
// TestEnqueueStatementMatchesTheGeneratedOne 钉住 —— 抄错了要有人喊。
const enqueueSQL = `INSERT INTO jobs (queue, job_key, payload, priority)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING`

// jobsTenants 造两家停用状态的店，并在测试结束时连它们的任务一起删掉。
//
// status = 2：理由与 rls_test.go 里那一段一字不差 —— 这两家是夹具不是店铺，
// 插一家活跃商家会在**别的包**里表现成一次随机的断言失败。
func jobsTenants(t *testing.T) (*pgx.Conn, int64, int64) {
	t.Helper()
	ctx := context.Background()

	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	suffix := fmt.Sprintf("jobsdef-%d", time.Now().UnixNano())
	var idA, idB int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'A',2), ($2,'B',2) RETURNING id`,
		suffix+"-a", suffix+"-b").Scan(&idA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM merchants WHERE code = $1`, suffix+"-b").Scan(&idB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		ids := []int64{idA, idB}
		// jobs 必须排在 merchants 前面：它有一个指向 merchants 的外键。
		if _, err := admin.Exec(c, `DELETE FROM jobs WHERE merchant_id = ANY($1)`, ids); err != nil {
			t.Errorf("清理 jobs 失败: %v", err)
		}
		if _, err := admin.Exec(c, `DELETE FROM merchants WHERE id = ANY($1)`, ids); err != nil {
			t.Errorf("清理 merchants 失败: %v", err)
		}
	})
	return admin, idA, idB
}

// 入队时的 merchant_id 只能是当前租户，而且那一列的值来自 current_merchant()。
//
// 判据是「同一条 INSERT 在两个不同的租户上下文里落到两个不同的租户」——
// 而不是「落到了某一个租户」。后者一个写死的 `DEFAULT 1` 也满足。
func TestEnqueueTakesItsTenantFromTheCurrentContext(t *testing.T) {
	ctx := context.Background()
	admin, idA, idB := jobsTenants(t)

	app, err := db.Connect(ctx) // Connect 已确认这条连接绕不过 RLS
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)

	// 两次入队用**同一个 job_key**：uk_jobs_pending 是租户内唯一的
	// （首列 merchant_id），所以两家店各入一条是合法的。它们要是落到同一个
	// 租户上，第二条会被 ON CONFLICT DO NOTHING 静默吃掉 —— 下面数出来是 1 条。
	for _, id := range []int64{idA, idB} {
		if _, err := app.Exec(ctx,
			`SELECT set_config('app.merchant_id', $1, false)`, fmt.Sprint(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx, enqueueSQL,
			"understanding", "product:1", []byte(`{"product_id":1}`), int16(0)); err != nil {
			t.Fatalf("租户 %d 入队失败: %v", id, err)
		}
	}

	var owners []int64
	rows, err := admin.Query(ctx,
		`SELECT merchant_id FROM jobs WHERE merchant_id = ANY($1) ORDER BY merchant_id`,
		[]int64{idA, idB})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var m int64
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		owners = append(owners, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(owners) != 2 {
		t.Fatalf("两个租户各入一条同名任务，库里只有 %d 条（%v）。"+
			"两条里有一条被吃掉了，而这只有两种成因，都是硬伤：\n"+
			"  ① merchant_id 不是由 current_merchant() 填的（两条落到了同一个租户）。"+
			"这张表**没有 RLS**（00022 第一节），那个列默认值是"+
			"「入队写不出别人的租户」这件事的唯一实现；\n"+
			"  ② uk_jobs_pending 不再以 merchant_id 打头，唯一键跨了租户 —— "+
			"于是 A 商家入过的 job_key 会让 B 商家的同名任务被 "+
			"ON CONFLICT DO NOTHING 静默丢掉，而 B 的商品再也不会被加工。\n"+
			"库里那几行的 merchant_id 是 %v，对照期望的 %v 能分辨是哪一种",
			len(owners), owners, owners, []int64{idA, idB})
	}
	if owners[0] != idA || owners[1] != idB {
		t.Fatalf("两条任务记在 %v 名下，期望 %v —— 入队的租户与入队时的"+
			"上下文对不上", owners, []int64{idA, idB})
	}
}

// 没有租户上下文时入队**当场失败**，不是写进一行猜出来的 merchant_id。
//
// 这是 00022 文件头第一节 ① 那句话的后半段：「没有租户上下文时入队不是
// 写进一行猜出来的 merchant_id，是当场 42501（current_merchant() 的 RAISE）」。
//
// 它为什么值得单独一条：worker 与触发点扫描都跑在任何 HTTP 请求之外，
// 「忘了进 WithTenant」在这条路径上是一个真实存在的写法。别的表上它的后果是
// RLS 把写挡回去；这张表上没有那道闸，所以后果完全取决于
// current_merchant() 在未设置时是 RAISE 还是 NULL —— 而如果哪天有人为了
// 「让平台级操作也能入队」把它改成 `COALESCE(current_merchant(), ...)`，
// 或者给那一列加一个兜底默认值，这条测试是唯一会喊的。
func TestEnqueueOutsideATenantTransactionFails(t *testing.T) {
	ctx := context.Background()
	jobsTenants(t) // 只为迁移与清理；这条测试刻意不设任何租户

	app, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)

	if _, err := app.Exec(ctx, `RESET app.merchant_id`); err != nil {
		t.Fatal(err)
	}

	_, err = app.Exec(ctx, enqueueSQL,
		"understanding", "product:orphan", []byte(`{"product_id":1}`), int16(0))
	if err == nil {
		t.Fatalf("没有租户上下文时入队**成功**了 —— jobs 没有 RLS，" +
			"那一行现在记在一个猜出来的租户名下（或者 NULL）。" +
			"00022 第一节说这一步该是当场 42501")
	}
	// 判的是 SQLSTATE 而不是错误文本：00002 里 current_merchant() 抛的是
	// insufficient_privilege(42501)，而文本是中文且会被改。
	var pge *pgconn.PgError
	if !errors.As(err, &pge) || pge.Code != "42501" {
		t.Fatalf("没有租户上下文时入队失败了，但错误是 %v —— "+
			"期望 SQLSTATE 42501（current_merchant() 的 RAISE）。"+
			"换成别的错误码通常意味着它是被 NOT NULL 或外键挡下的，"+
			"那是一条会随 DDL 变化而消失的防线", err)
	}
}

// 上面那个 enqueueSQL 常量与 db/queries/jobs.sql 生成出来的那条语句一致。
//
// 没有这一条的话，上面两条测试测的可能是一条**只存在于这个文件里**的 INSERT：
// 有人给真正的入队语句加一列 merchant_id，这个文件一个字不改、照样全绿，
// 而它们声称守护的那件事已经没人守了。
func TestEnqueueStatementMatchesTheGeneratedOne(t *testing.T) {
	// ../.. 是仓库根：这个包在 internal/db 下，migrate_test.go 里那个
	// `make -C ../..` 用的是同一个相对位置。
	raw, err := os.ReadFile(filepath.Join("..", "..",
		"internal", "repository", "internal", "db", "jobs.sql.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), enqueueSQL) {
		t.Fatalf("这个文件里的 enqueueSQL 在 sqlc 产物里找不到逐字一致的那一条 —— "+
			"入队语句改了而这里没跟上，上面两条测试从此测的是一条不存在的 SQL。\n"+
			"这里的是：\n%s", enqueueSQL)
	}
}
