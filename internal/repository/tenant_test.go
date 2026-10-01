package repository_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/testdb"
)

// 本文件在 internal/repository/ 目录下，所以它 import 得到 internal/db 那个
// sqlc 产物 —— internal 规则按 import 方所在目录判定，与包名无关。
// 这正是要守的边界本身：只有 repository 这一层看得见生成代码。
// internal/handler/isolation_test.go 从另一侧钉住「其余任何地方都看不见」。
//
// 它也刻意不再用那个产物：WithTenant 交给 fn 的是 repository.Tx（接口），
// 断言写在领域类型上。测试若还拿着 sqlc 的 params 结构体，就等于替业务层
// 演练了一遍「我其实够得着生成代码」，而那正是这一层要消灭的姿势。

// TestMain 给本包一个只属于它的库（keel_test_repository），在上面从空库
// 跑一遍迁移。每次运行都是新建的库，所以「可变状态跨轮次累积」和「改了已应用
// 的迁移、暖库假绿」这两件事都不存在 —— 以前靠 DROP SCHEMA 保证，现在靠
// 库本身是新的。见 internal/testdb。
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, testdb.Package{Name: "repository"}))
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
	err := r.WithTenant(context.Background(), func(q repository.Tx) error {
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
			func(q repository.Tx) error {
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
	if err := r.WithTenant(ctx, func(q repository.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}

	err := r.WithTenant(context.Background(), func(q repository.Tx) error {
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
		func(q repository.Tx) error { return nil }); err != nil {
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
		// 作用域取这家自己的默认门店：00020 之后 ListProducts 按门店取价、
		// 按门店算 in_stock、按门店与大区排除不卖的款。这条测试要的仍然是
		// 「RLS 拦不拦得住别家」，所以门店必须是**本家**那一家 ——
		// 传别家的店，失败的原因就换成了门店不匹配，测的不再是 RLS。
		sc := defaultScope(t, tc.id)
		err := r.WithTenant(tenant.NewContext(ctx, tc.id), func(q repository.Tx) error {
			rows, err := q.ListProducts(ctx, sc, repository.ListingFilter{}, 100, 0)
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
	// status = 2（停用）不是随手写的：这两家是为了测隔离而存在的夹具，不是
	// 对外营业的店铺，而 tenant 包的 Preflight 断言的是**整个库**的形态
	// （「配了默认商家时活跃商家只能有一家」）。这段写下时各包共用同一个库，
	// 这里插一家活跃商家会在另一个包里表现为一次随机的断言失败，凶手名字
	// 还出现在别人的错误信息里。现在各包各有自己的库（internal/testdb），
	// 跨包这条路断了；但同一个包里的测试仍共用一个库，所以照旧停用。
	// 隔离测试本身不关心 status —— RLS 的谓词只看 merchant_id。
	rows, err := admin.Query(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'A', 2), ($2,'B', 2) RETURNING id`,
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
			// stores → regions 排在 merchants 之前（00020 加的两条外键）。
			// 漏掉它们不会让这条测试红，会让**下一轮**那句
			// `DELETE FROM merchants` 以 23503 失败，而那条错误出现在别处。
			`DELETE FROM stores     WHERE merchant_id = ANY($1)`,
			`DELETE FROM regions    WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants  WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, all); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	// 每家一个大区 + 一家默认门店。00020 之后买家侧的读路径都带 StoreScope
	// （价格按门店取、in_stock 按门店算、两层可见性排除按门店与大区查），
	// 而迁移里那段回填只覆盖迁移那一刻库里已有的商家 —— 这两家是之后插的。
	// is_default = TRUE，不画围栏：默认店靠「全国兜底」接单（「非默认 且
	// 「非默认 且 无围栏」，这里没有一条断言碰地理。
	for _, m := range []int64{idA, idB} {
		var regionID int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO regions (merchant_id, code, name) VALUES ($1,'default','默认大区')
			 RETURNING id`, m).Scan(&regionID); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx,
			`INSERT INTO stores (merchant_id, region_id, code, name, is_default)
			 VALUES ($1,$2,'default','默认门店',TRUE)`, m, regionID); err != nil {
			t.Fatal(err)
		}
	}

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

// defaultScope 取这家商家那家默认门店的作用域。
//
// 现查而不是让 seedTwoTenants 多返回两个 id：那个函数被三个文件调，
// 改签名等于把「门店」这件事摊到每一个不关心它的调用点上。
// 现查还顺带钉住一件事 —— 夹具里那家默认店真的存在；查不到直接 Fatal，
// 而不是让下游拿着一个 0 去查询（那会让按门店取价、按门店算 in_stock
// 全部退化成恒空，且没有任何东西会红）。
func defaultScope(t *testing.T, merchantID int64) repository.StoreScope {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var sc repository.StoreScope
	if err := admin.QueryRow(ctx,
		`SELECT id, region_id FROM stores
		  WHERE merchant_id = $1 AND is_default AND deleted_at IS NULL`,
		merchantID).Scan(&sc.StoreID, &sc.RegionID); err != nil {
		t.Fatalf("商家 %d 没有默认门店（夹具漏了）: %v", merchantID, err)
	}
	return sc
}
