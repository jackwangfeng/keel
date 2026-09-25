package repository_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	sqlcdb "github.com/keel/keel/internal/repository/internal/db"
	"github.com/keel/keel/internal/tenant"
)

// 本文件在 internal/repository/ 目录下，所以它 import 得到 internal/db 那个
// sqlc 产物 —— internal 规则按 import 方所在目录判定，与包名无关。
// 这正是要守的边界本身：只有 repository 这一层看得见生成代码。
// internal/handler/isolation_test.go 从另一侧钉住「其余任何地方都看不见」。

// TestMain 保证库里有 schema。
//
// 只在缺 schema 时才跑迁移：internal/db 的测试也在跑 goose，而 `go test ./...`
// 会并行跑多个包。在已迁好的库上，这里只是一次读版本表的空转；在全新库上，
// 两个包仍可能同时 goose up，那时一方会带着明确的冲突报错红掉，不会静默。
func TestMain(m *testing.M) {
	if err := ensureSchema(); err != nil {
		fmt.Fprintf(os.Stderr, "准备 schema 失败: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func ensureSchema() error {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return err
	}
	defer admin.Close(ctx)

	var ok bool
	if err := admin.QueryRow(ctx,
		`SELECT to_regclass('public.products') IS NOT NULL`).Scan(&ok); err != nil {
		return err
	}
	if ok {
		return nil
	}
	// 与 internal/db 的迁移测试同一个惯例：调 make 这个稳定入口，
	// 而不是把 `go -C tools run ...` 抄一份进测试。
	out, err := exec.Command("make", "-C", "../..", "migrate",
		"GOOSE_DBSTRING="+db.AdminDSN()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

// pool 走 db.NewPool 而不是 pgxpool.New：前者把「这条连接能不能绕过 RLS」
// 的自检挂在了每一条物理连接上。裸 pgxpool.New 建出来的池没有任何东西
// 强迫它做这个检查，于是「测试跑在超级用户连接上、所有隔离断言假绿」
// 这个洞会从池这一侧原样回来。
func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := db.NewPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// 没有租户上下文时必须直接失败，不能退化成「查全部」，也不能回落到某个默认租户。
func TestWithTenantRefusesMissingTenant(t *testing.T) {
	r := repository.New(pool(t))
	err := r.WithTenant(context.Background(), func(q *repository.Queries) error {
		t.Error("不应该执行到这里：没有租户上下文时 fn 不该被调用")
		return nil
	})
	if !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("期望 ErrNoTenant，实得 %v", err)
	}
}

// 租户为 0 或负数同样要被拒：0 会被当成一个合法 merchant_id 送进 SET LOCAL，
// RLS 随后安静地过滤出空结果集 —— 症状是「查不到数据」，真因是中间件没挂。
func TestWithTenantRefusesZeroTenant(t *testing.T) {
	r := repository.New(pool(t))
	for _, id := range []int64{0, -1} {
		err := r.WithTenant(tenant.NewContext(context.Background(), id),
			func(q *repository.Queries) error {
				t.Errorf("租户 %d 不应该被接受", id)
				return nil
			})
		if !errors.Is(err, tenant.ErrNoTenant) {
			t.Fatalf("租户 %d：期望 ErrNoTenant，实得 %v", id, err)
		}
	}
}

// 上一次调用的租户不能被下一次调用继承（Go 这一侧）。
func TestTenantDoesNotLeakToNextCall(t *testing.T) {
	r := repository.New(pool(t))

	ctx := tenant.NewContext(context.Background(), 1)
	if err := r.WithTenant(ctx, func(q *repository.Queries) error { return nil }); err != nil {
		t.Fatal(err)
	}

	err := r.WithTenant(context.Background(), func(q *repository.Queries) error {
		t.Error("不应该执行到这里")
		return nil
	})
	if !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("上一次调用的租户可能泄漏了：期望 ErrNoTenant，实得 %v", err)
	}
}

// 上一条只测了 Go 这一侧的检查，测不到真正的泄露路径：
// 如果 set_config 的第三个参数写成 false（= 会话级 SET 而非 SET LOCAL），
// 上面那个测试照样全绿 —— 它在 fn 被调用之前就返回了，根本没碰数据库。
// 而此时 app.merchant_id 已经留在那条物理连接上，池把它交给下一个请求，
// 那个请求会以上一个租户的身份读数据，且一切正常，没有任何报错。
//
// 所以这里直接查连接上的残留值。把 tenant.go 里的 true 改成 false，这条会红。
func TestTenantSettingDiesWithTheTransaction(t *testing.T) {
	ctx := context.Background()
	p := pool(t)
	r := repository.New(p)

	if err := r.WithTenant(tenant.NewContext(ctx, 4242),
		func(q *repository.Queries) error { return nil }); err != nil {
		t.Fatal(err)
	}

	// 断言池自始至终只建过一条物理连接：这样下面那条查询必定跑在刚才那条
	// 连接上，读到的残留值才是刚才那个事务留下的。不然「读到空」也可能
	// 只是因为换了条干净的连接。
	if n := p.Stat().TotalConns(); n != 1 {
		t.Fatalf("池里有 %d 条连接，无法确定下面这条查询跑在刚才那条上", n)
	}

	var setting string
	if err := p.QueryRow(ctx,
		`SELECT coalesce(current_setting('app.merchant_id', true), '')`).
		Scan(&setting); err != nil {
		t.Fatal(err)
	}
	if setting != "" {
		t.Fatalf("事务结束后连接上还留着 app.merchant_id=%q —— "+
			"这是会话级 SET 而不是 SET LOCAL，池把它交给下一个请求就是跨租户泄露", setting)
	}
}

// 到这里为止的测试都没有真正查过数据。这个测试走完整条路：
// WithTenant 设租户 → 生成的 ListProducts（刻意不带 WHERE merchant_id）
// → RLS 在库里过滤。它同时证明两件事：租户上下文确实送到了数据库，
// 以及「不在应用层重复过滤」这个决定是安全的。
func TestWithTenantScopesGeneratedQueryToTheTenant(t *testing.T) {
	ctx := context.Background()
	idA, idB := seedTwoTenants(t)

	r := repository.New(pool(t))

	for _, tc := range []struct {
		id   int64
		want string
	}{{idA, "product-A"}, {idB, "product-B"}} {
		var titles []string
		err := r.WithTenant(tenant.NewContext(ctx, tc.id), func(q *repository.Queries) error {
			rows, err := q.ListProducts(ctx, sqlcdb.ListProductsParams{Limit: 100, Offset: 0})
			if err != nil {
				return err
			}
			for _, row := range rows {
				titles = append(titles, row.Title)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("租户 %d: %v", tc.id, err)
		}
		if len(titles) != 1 || titles[0] != tc.want {
			t.Fatalf("租户 %d 查到 %v，期望只有 [%s] —— "+
				"查询里没有 WHERE merchant_id，拦住别家数据的只能是 RLS",
				tc.id, titles, tc.want)
		}
	}
}

// seedTwoTenants 用管理员连接（绕过 RLS）播两个租户各一件在售商品，返回两个商家 ID。
func seedTwoTenants(t *testing.T) (int64, int64) {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	// 用 t.Cleanup 而不是 defer 关连接：清理数据还要用这条连接，
	// 而 defer 在测试函数返回时就跑，早于所有 t.Cleanup（Cleanup 是后进先出，
	// 所以这条先注册、最后执行）。写成 defer 的话删除会在一条已关闭的连接上
	// 静默失败，把测试数据留在库里。
	t.Cleanup(func() { admin.Close(context.Background()) })

	suffix := fmt.Sprintf("repotest-%d", time.Now().UnixNano())
	var ids []int64
	rows, err := admin.Query(ctx,
		`INSERT INTO merchants (code, name) VALUES ($1,'A'), ($2,'B') RETURNING id`,
		suffix+"-a", suffix+"-b")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("期望插入 2 个商家，实得 %d 个", len(ids))
	}
	idA, idB := ids[0], ids[1]

	t.Cleanup(func() {
		c := context.Background()
		all := []int64{idA, idB}
		// 删不掉要出声：悄悄失败的清理会把测试数据一直攒在开发库里，
		// 而下一次「只应看到 1 件商品」的断言会因此漂移。
		for _, stmt := range []string{
			`DELETE FROM products   WHERE merchant_id = ANY($1)`,
			`DELETE FROM categories WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants  WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, all); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	for _, s := range []struct {
		merchant int64
		title    string
	}{{idA, "product-A"}, {idB, "product-B"}} {
		var catID int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO categories (merchant_id, name, path) VALUES ($1, 'c', '/c/')
			 RETURNING id`, s.merchant).Scan(&catID); err != nil {
			t.Fatal(err)
		}
		// status = 1（在售）且 deleted_at IS NULL，才落进 ListProducts 的条件里。
		if _, err := admin.Exec(ctx,
			`INSERT INTO products (merchant_id, category_id, title, status, published_at)
			 VALUES ($1, $2, $3, 1, now())`, s.merchant, catID, s.title); err != nil {
			t.Fatal(err)
		}
	}
	return idA, idB
}
