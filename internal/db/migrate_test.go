package db_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

// migrate 通过根 Makefile 的 migrate 目标跑迁移。
//
// 不直接 exec "goose"：goose 钉在 tools/go.mod 里（见 Makefile 顶部关于工具
// 子模块的说明），PATH 上没有、也不该有一个 goose 二进制。测试调 make 这个
// 稳定入口，而不是把 `go -C tools run ...` 这条长命令抄一份到测试里——抄一份
// 就意味着以后改调用方式要改两个地方，而漏改的那个会以「本地能过 CI 红」的
// 形式暴露。
func migrate(t *testing.T) ([]byte, error) {
	t.Helper()
	resetSchema(t)
	cmd := exec.Command("make", "-C", "../..", "migrate", "GOOSE_DBSTRING="+db.AdminDSN())
	return cmd.CombinedOutput()
}

// resetSchema 在每次迁移前把 public 清空。
//
// 不清的话，goose 看到版本已是最新就什么都不做 —— 于是**改了一份已应用的迁移，
// 在暖库上是假绿**。实测过：删掉 orders.user_id 的复合外键，暖库上
// TestForeignKeysAreNotSilentlyMissing 照样 ok，换空库才红。
//
// 这条对本包尤其要命：本包的测试全部是「拿系统目录核对迁移写了什么」，
// 而它们读的是**库**不是**文件**。库不跟着文件走的时候，这些断言守的是
// 上一次跑过的那份迁移，不是工作区里这份。
//
// 代价是每个测试都要重跑一遍全部迁移（约一秒）。这个仓库在这类取舍上一贯
// 选正确性：-count=1 是为了不让缓存假绿，-p 1 是为了不让并发迁移互撞，
// 这一条是同一类。
func resetSchema(t *testing.T) {
	t.Helper()
	admin, err := pgx.Connect(context.Background(), db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(context.Background(),
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
}

// migratedConn 跑一次迁移并返回一条应用角色连接。
// 走 db.Connect 而不是 pgx.Connect：它会当场确认这条连接不能绕过 RLS，
// 否则下面每一条断言都可能在一条超级用户连接上假绿。
func migratedConn(t *testing.T) *pgx.Conn {
	t.Helper()
	if out, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v\n%s", err, out)
	}
	conn, err := db.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// allTables 枚举 public 下的全部普通表。
//
// 清单从系统目录里枚举，不写死。原先这里是 []string{"categories", "products",
// "skus"} 这样的字面量，出现在两个测试里。它的问题不在今天对不对，而在明天：
// M2 会新增 inventories、orders、order_items……新表只要没人记得往那两个字面量里
// 补名字，就不在任何断言的视野里，而「忘了给新表挂 RLS」恰恰是最可能发生、
// 后果最重的那种疏忽。
func allTables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT c.relname
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relkind = 'r'
		  ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("一张表都没枚举到——这个检查本身失效了")
	}
	return out
}

// 迁移必须能在已经迁过的库上再跑一次而不报错。
// goose 靠版本表保证这一点，但 RLS 策略与函数的 CREATE 是最容易踩的地方——
// 如果有人把它们写进了会重复执行的位置，这里会红。
func TestMigrateIsIdempotent(t *testing.T) {
	for i := 0; i < 2; i++ {
		out, err := migrate(t)
		if err != nil {
			t.Fatalf("第 %d 次迁移失败: %v\n%s", i+1, err, out)
		}
	}

	conn, err := db.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	// current_merchant() 必须是 STABLE —— IMMUTABLE 会被折进缓存计划，
	// 导致同一条预编译语句对所有租户返回第一个租户的数据。
	var volatility string
	err = conn.QueryRow(context.Background(),
		`SELECT provolatile FROM pg_proc WHERE proname = 'current_merchant'`).
		Scan(&volatility)
	if err != nil {
		t.Fatal(err)
	}
	if volatility != "s" {
		t.Fatalf("current_merchant() 的 volatility 是 %q，必须是 \"s\"(STABLE)", volatility)
	}

	// 凡是该有租户策略的表，都必须同时 ENABLE 且 FORCE —— 只 ENABLE 的话
	// 表属主绕过 RLS，而迁移工具跑出来的属主通常就是应用自己。
	man := loadManifest(t)
	for _, tbl := range allTables(t, conn) {
		spec, _ := man.class(tbl)
		if spec.Policy == "none" {
			continue
		}
		var enabled, forced bool
		err := conn.QueryRow(context.Background(),
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`,
			tbl).Scan(&enabled, &forced)
		if err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Fatalf("%s: ENABLE=%v FORCE=%v，两者都必须为 true", tbl, enabled, forced)
		}
	}
}

type policy struct{ name, permissive, cmd, qual, withCheck string }

func policiesOf(t *testing.T, conn *pgx.Conn, tbl string) []policy {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT policyname, permissive, cmd,
		        coalesce(qual, ''), coalesce(with_check, '')
		   FROM pg_policies
		  WHERE schemaname = 'public' AND tablename = $1
		  ORDER BY policyname`, tbl)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []policy
	for rows.Next() {
		var p policy
		if err := rows.Scan(&p.name, &p.permissive, &p.cmd, &p.qual, &p.withCheck); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// RLS 由三个元素共同成立：①current_merchant() ②表上的 ENABLE+FORCE ③策略本身。
// 上面那个测试覆盖了 ①②，但 ③ 漏了——而 ③ 恰恰是最容易被「优化」掉的那个：
// 将来有人为了修一个「查不到数据」的 bug，会去放宽谓词或直接 DROP POLICY，
// 前两条断言对此一声不吭，库照样漏。
//
// 这里逐表钉死：有且只有一条名为 tenant 的策略，作用于 ALL 而不只是 SELECT，
// 且写谓词没有被单独放开。
//
// 读侧和写侧要分别断言，它们是两件事：
//
//   - cmd='ALL' 只保证策略「作用于」写入。改成 FOR SELECT 的话写入侧就没人管了，
//     这条断言会红。
//   - 但作用于写入不等于写入用的是租户谓词。写入实际套用的是 with_check，
//     省略时才回退到 using。所以 `ALTER POLICY ... WITH CHECK (true)` 能在
//     policyname / qual / cmd / permissive 四项全部不变的情况下，把写入侧整个放开——
//     租户 A 可以往租户 B 名下插行，而读侧一切正常。
//
// 因此 with_check 要么为空（回退到上面已经钉死的 qual），要么必须等于同一个谓词。
//
// **谓词的形状由 db/tenancy.json 的类别决定，不是所有表共用一个常量。**
// 留出按类别不同的余地是必需的：inventories 按规矩一豁免了 merchant_id
// （见数据模型 §4），它的策略谓词是一个对 skus 的 EXISTS 子查询，
// 和 tenant 类各表的列比较不是一个形状。而没在清单里登记的表一律按最严的
// tenant 类查——M2 新建一张表时，测试会红在「谓词对不上」或「没有策略」上，
// 逼作者去清单里把这张表的隔离方式**写下来**。
func TestTenantPoliciesArePresentAndExact(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	const columnQual = "(merchant_id = current_merchant())"
	checked := 0

	for _, tbl := range allTables(t, conn) {
		spec, entry := man.class(tbl)
		got := policiesOf(t, conn, tbl)

		if spec.Policy == "none" {
			// 这些类别（tenant-root / shared-reference / cross-tenant-infra）
			// 没有租户维度，挂上策略会让所有人都读不到。挂了要出声。
			if len(got) != 0 {
				t.Errorf("%s 在清单里是 %q 类，不该有任何租户策略，实际有 %d 条: %+v",
					tbl, entry.Class, len(got), got)
			}
			continue
		}
		checked++

		// 不是「至少有一条」而是「有且只有一条」：permissive 策略之间是 OR，
		// 多加一条 USING (true) 就能把隔离整个抵消掉，且不动原策略一个字。
		if len(got) != 1 {
			t.Errorf("%s: 期望恰好 1 条策略，实际 %d 条: %+v", tbl, len(got), got)
			continue
		}
		p := got[0]
		if p.name != man.PolicyName {
			t.Errorf("%s: 策略名是 %q，期望 %q——命名约定见 db/tenancy.json 的 policy_name",
				tbl, p.name, man.PolicyName)
		}
		if p.cmd != "ALL" {
			t.Errorf("%s: 策略作用于 %q，期望 \"ALL\"——只保 SELECT 的话写入侧没人管", tbl, p.cmd)
		}
		if p.permissive != "PERMISSIVE" {
			t.Errorf("%s: 策略是 %q", tbl, p.permissive)
		}

		switch spec.Policy {
		case "column":
			if p.qual != columnQual {
				t.Errorf("%s: 策略谓词是 %q，期望 %q", tbl, p.qual, columnQual)
			}
		case "parent":
			// 父表定租户的表（inventories / staff_tokens）谓词是 EXISTS 子查询。
			// 子查询的文本会被 PostgreSQL 规范化，逐字比对太脆；改为断言它的
			// 两个要害：确实去父表里查了，且确实用了 current_merchant()。
			// 少任何一个，隔离就不成立。
			if entry.Parent == "" {
				t.Errorf("%s 是 parent 类却没在清单里写 parent 字段", tbl)
				continue
			}
			for _, needle := range []string{entry.Parent, "current_merchant()", entry.Via} {
				if !strings.Contains(p.qual, needle) {
					t.Errorf("%s: 策略谓词 %q 里没有 %q——"+
						"父表定租户的表，谓词必须真的去 %s 里按 current_merchant() 查",
						tbl, p.qual, needle, entry.Parent)
				}
			}
		default:
			t.Errorf("%s: 清单里的 policy 形态 %q 无法理解", tbl, spec.Policy)
		}

		// 空表示省略 WITH CHECK，PostgreSQL 回退到 qual，而 qual 上面刚断言过。
		// 非空就必须是同一个谓词：WITH CHECK (true) 时上面各项全部照旧通过，
		// 但租户 A 能往租户 B 名下插行。
		if p.withCheck != "" && p.withCheck != p.qual {
			t.Errorf("%s: 策略的写谓词是 %q，期望为空（回退到读谓词）或与读谓词相同——"+
				"写谓词被单独放开时，读侧看不出任何异常", tbl, p.withCheck)
		}
	}

	if checked == 0 {
		t.Fatal("一张需要租户策略的表都没查到——这个检查本身失效了")
	}
}

type foreignKey struct {
	name, from, to string
	cols           []string
}

func foreignKeys(t *testing.T, conn *pgx.Conn) []foreignKey {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT c.conname,
		       c.conrelid::regclass::text  AS from_tbl,
		       c.confrelid::regclass::text AS to_tbl,
		       (SELECT array_agg(a.attname ORDER BY k.ord)
		          FROM unnest(c.conkey) WITH ORDINALITY k(att, ord)
		          JOIN pg_attribute a
		            ON a.attrelid = c.conrelid AND a.attnum = k.att) AS cols
		  FROM pg_constraint c
		  JOIN pg_namespace n ON n.oid = c.connamespace
		 WHERE c.contype = 'f' AND n.nspname = 'public'
		 ORDER BY 2, 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []foreignKey
	for rows.Next() {
		var fk foreignKey
		if err := rows.Scan(&fk.name, &fk.from, &fk.to, &fk.cols); err != nil {
			t.Fatal(err)
		}
		out = append(out, fk)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// 规矩二（父子关系用复合外键钉死）。
//
// 规矩一有 scripts/check_tenancy.py 守着，但那个脚本读的是**设计文档**，
// 不是迁移。于是存在一种没人会发现的脱节：文档完全正确，而库是错的。
// 这正是这条测试写出来时抓到的第一件事——设计文档里 categories.parent_id
// 早就是 FOREIGN KEY (parent_id, merchant_id)，迁移里却是单列的
// REFERENCES categories(id)，A 商家的分类可以挂到 B 商家的分类下面。
//
// 判据直接从系统目录反查，不解析 SQL：凡是**两端都带 merchant_id** 的外键，
// 约束列里就必须有 merchant_id。指向 merchants 自己的那一条除外——
// 那条外键的单列就是租户列本身。
//
// **豁免按「表(列)」写在 db/tenancy.json 的 fk_single_column_ok 里。**
// 这条测试原先是全有全无的：没有豁免机制，唯一出路是改弱或删掉它。
// 而 M3 会撞上它——数据模型 §14 明写 staff 是全文唯一一张 merchant_id 可空的表，
// 复合外键在它身上会让「平台管理员创建某家店的管理员」写不进去，
// 所以 staff.created_by / shipments.created_by / uploads.staff_id 三处
// **设计上就该是单列外键**。三处已经预先登记在清单里。
func TestCrossTenantForeignKeysAreComposite(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	hasMerchantID := map[string]bool{}
	for _, tbl := range allTables(t, conn) {
		var n int
		if err := conn.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_attribute a
			   JOIN pg_class c ON c.oid = a.attrelid
			   JOIN pg_namespace ns ON ns.oid = c.relnamespace
			  WHERE ns.nspname = 'public' AND c.relname = $1
			    AND a.attname = 'merchant_id' AND a.attnum > 0 AND NOT a.attisdropped`,
			tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		hasMerchantID[tbl] = n > 0
	}

	used := map[string]bool{}
	checked := 0
	for _, fk := range foreignKeys(t, conn) {
		// 两端都带 merchant_id 才是「跨租户可错挂」的形状。
		if !hasMerchantID[fk.from] || !hasMerchantID[fk.to] {
			continue
		}
		checked++
		if slices.Contains(fk.cols, "merchant_id") {
			continue
		}
		key := fmt.Sprintf("%s(%s)", fk.from, strings.Join(fk.cols, ","))
		if exempt(man.FKSingleOK, key) {
			used[key] = true
			continue
		}
		t.Errorf("外键 %s（%s → %s）只引用了 %v，没带 merchant_id——"+
			"A 商家的行可以挂到 B 商家的行上，数据库不会拒绝。"+
			"改成 FOREIGN KEY (..., merchant_id) REFERENCES %s(id, merchant_id)，"+
			"或把 %s 登记进 db/tenancy.json 的 fk_single_column_ok 并写明理由",
			fk.name, fk.from, fk.to, fk.cols, fk.to, key)
	}
	if checked == 0 {
		t.Fatal("一条跨租户外键都没查到——这个检查本身失效了")
	}

	// 清单里写着一条库里根本没有的豁免，说明它过期了——它会静默地豁免掉
	// 一条将来同名的外键。只对**库里已有的表**做这个检查：清单是前瞻的，
	// M2/M3 才会建的表现在就登记在册，那不是过期。
	live := map[string]bool{}
	for _, tbl := range allTables(t, conn) {
		live[tbl] = true
	}
	for key := range man.FKSingleOK {
		if strings.HasPrefix(key, "_") || used[key] {
			continue
		}
		if tbl, _, ok := strings.Cut(key, "("); ok && live[tbl] {
			t.Errorf("fk_single_column_ok 里的 %s 已经不是一条单列跨租户外键了，该清理了", key)
		}
	}
}

// 规矩二的**对偶**：看起来是外键、却根本没有外键的列。
//
// 上面那条测试有一个盲区，是实测出来的，不是推断：
//
//	把 products 的复合外键降级成单列 → 变红 ✅
//	把这条外键整个删掉               → 仍然全绿 ❌
//
// **违规做得更彻底反而绕过了闸门。** 它只看「已存在的外键够不够复合」，
// 而「压根没有外键」这件事在它的查询里根本不产生一行。
//
// 代价具体：数据模型 §5 的 order_items 按原样实现时，sku_id / product_id
// 都没有外键约束，于是 A 商家的订单行可以引用 B 商家的 SKU——数据库不拒、
// 闸门不红、测试全绿。而订单行带着价格与规格快照，错挂之后展示、对账、退款
// 看起来全都正常，只有拿 sku_id 回查库存时才露馅，而那正是 M2 下单 SAGA 的主链路。
//
// 所以这里做对偶断言：业务表上名为 <x>_id、存在同名父表（sku_id → skus）、
// 却没有任何外键引用它的列，要么补外键，要么进 db/tenancy.json 的
// fk_missing_ok 并写明理由。
func TestForeignKeysAreNotSilentlyMissing(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	tables := allTables(t, conn)
	exists := map[string]bool{}
	for _, tbl := range tables {
		exists[tbl] = true
	}

	// 被任意一条外键引用到的 表.列。
	covered := map[string]bool{}
	for _, fk := range foreignKeys(t, conn) {
		for _, c := range fk.cols {
			covered[fk.from+"."+c] = true
		}
	}

	rows, err := conn.Query(context.Background(), `
		SELECT c.relname, a.attname
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		   AND a.attnum > 0 AND NOT a.attisdropped
		 ORDER BY 1, a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	resolved := 0
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatal(err)
		}
		parent := parentTableOf(col, exists)
		if parent == "" || parent == tbl {
			continue
		}
		resolved++
		if covered[tbl+"."+col] {
			continue
		}
		key := tbl + "." + col
		if exempt(man.FKMissingOK, key) {
			continue
		}
		t.Errorf("%s 指向 %s 却没有任何外键约束——违规做得更彻底反而绕过了规矩二："+
			"A 商家的行可以引用 B 商家的行，数据库不拒、闸门不红。"+
			"补成 FOREIGN KEY (%s, merchant_id) REFERENCES %s(id, merchant_id)，"+
			"或把 %s 登记进 db/tenancy.json 的 fk_missing_ok 并写明理由",
			key, parent, col, parent, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// 一条 <x>_id → 父表 都没解析出来，说明解析规则跟命名约定脱钩了，
	// 这个检查从此对任何缺失外键都视而不见。
	if resolved == 0 {
		t.Fatal("一列都没解析到父表——这个检查本身失效了")
	}
}

// 规矩三：唯一约束一律收进租户内。
//
// 规矩一有 check_tenancy.py、规矩二有上面两条测试，规矩三在设计文档里被引用
// 七八次却一直没有任何闸门。今天库里合规，但 M2 的 orders.order_no、
// payments.channel_txn_id、idempotency_keys 全靠人记得——而它们恰好都是
// 「全局唯一是错的」的典型：两个商家各自有一个 sku_code = "A001" 完全正常，
// 而一条全局唯一约束会让**后建的那家店建不了商品**，报错还指向一个
// 自己根本看不见的行。
//
// 两条自动放行，见 db/tenancy.json 里的说明：首列是 merchant_id（规矩三本身），
// 首列是本表的代理主键 id（按构造就全局唯一，`UNIQUE (id, merchant_id)` 这种
// 给子表复合外键当落点的索引一律属于这一类）。
func TestUniqueConstraintsAreTenantScoped(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	rows, err := conn.Query(context.Background(), `
		SELECT c.relname AS tbl, i.relname AS idx,
		       (SELECT array_agg(a.attname ORDER BY k.ord)
		          FROM unnest(ix.indkey::int2[]) WITH ORDINALITY k(att, ord)
		          JOIN pg_attribute a
		            ON a.attrelid = c.oid AND a.attnum = k.att) AS cols
		  FROM pg_index ix
		  JOIN pg_class i ON i.oid = ix.indexrelid
		  JOIN pg_class c ON c.oid = ix.indrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r' AND ix.indisunique
		 ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	used := map[string]bool{}
	checked := 0
	for rows.Next() {
		var tbl, idx string
		var cols []string
		if err := rows.Scan(&tbl, &idx, &cols); err != nil {
			t.Fatal(err)
		}
		spec, _ := man.class(tbl)
		if !spec.UniqueScoped {
			continue // 没有租户维度的类别，「收进租户内」无从谈起
		}
		if len(cols) == 0 {
			t.Errorf("%s 上的唯一索引 %s 解析不出列（表达式索引？）——"+
				"这条规矩对它失效了，请手工判断并在清单里登记", tbl, idx)
			continue
		}
		checked++
		if cols[0] == "merchant_id" || cols[0] == "id" {
			continue
		}
		key := fmt.Sprintf("%s(%s)", tbl, strings.Join(cols, ","))
		if exempt(man.UniqueOK, key) {
			used[key] = true
			continue
		}
		t.Errorf("唯一索引 %s 是 %s，首列不是 merchant_id——"+
			"全局唯一的东西在多租户下几乎都是错的（两家店各有一个 "+
			"sku_code = \"A001\" 是正常的）。收进租户内，"+
			"或把 %s 登记进 db/tenancy.json 的 unique_global_ok 并写明理由",
			idx, key, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("一条唯一索引都没查到——这个检查本身失效了")
	}

	live := map[string]bool{}
	for _, tbl := range allTables(t, conn) {
		live[tbl] = true
	}
	for key := range man.UniqueOK {
		if strings.HasPrefix(key, "_") || used[key] {
			continue
		}
		if tbl, _, ok := strings.Cut(key, "("); ok && live[tbl] {
			t.Errorf("unique_global_ok 里的 %s 在库里不存在，该清理了", key)
		}
	}
}

// keel_app 的 GRANT 面必须和清单里的类别一一对上。
//
// migrate_test.go 原先把 merchants / shop_settings 豁免出 RLS 检查，理由写着
// 「它们的防护靠的是 keel_app 的 GRANT 面与解析层自身」。**实测那个 GRANT 面
// 是 SELECT/INSERT/UPDATE/DELETE 全给**，也就是说那句注释是一句没有兑现的话：
//
//	租户 1 的上下文里改租户 2 的 domain（域名劫持）→ UPDATE 1
//	租户 1 的上下文里把租户 2 停用（全站 DoS）     → UPDATE 1
//	完全没有租户上下文时改 merchants               → UPDATE 1
//
// M1 不可利用（一个写接口都没有），但 M2 之后一旦有「商家改自己的店铺设置」
// 这类接口，它就是域名劫持加一键停掉别家店。
//
// 00005 把写权限收掉了。而修复本身也需要闸门守着——否则下一个人一句
// `GRANT ALL ON merchants TO keel_app` 就能把它退回去，且没有任何测试会红。
// 顺带钉死的还有 ALTER DEFAULT PRIVILEGES 的收窄：它原先让**每一张将来新建的表**
// 自动拿到四权，包括 order_status_transitions 这类全租户共用的静态参考数据
// ——任何租户都能改状态机，而那比跨租户读取更难发现。
func TestAppRoleGrantSurface(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	const appRole = "keel_app"
	all := []string{"SELECT", "INSERT", "UPDATE", "DELETE"}

	for _, tbl := range allTables(t, conn) {
		want := man.grants(tbl)
		var got []string
		for _, priv := range all {
			var ok bool
			if err := conn.QueryRow(context.Background(),
				`SELECT has_table_privilege($1, $2, $3)`, appRole, tbl, priv).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				got = append(got, priv)
			}
		}
		slices.Sort(got)
		w := slices.Clone(want)
		slices.Sort(w)
		if !slices.Equal(got, w) {
			_, entry := man.class(tbl)
			cls := entry.Class
			if cls == "" {
				cls = "tenant（未登记，按默认类别）"
			}
			t.Errorf("%s 上 %s 的权限是 %v，清单说应该是 %v（类别 %s）——"+
				"多给的每一项都是一条可写的路径，少给的每一项都是一次运行期 42501",
				tbl, appRole, got, w, cls)
		}
	}
}

// 带 updated_at 的表必须挂上 touch_updated_at 触发器。
//
// 在 00007 之前，七张带这一列的表都只有 DEFAULT now()：那一列记的是**创建
// 时间**，改一行不会动它。这种错不报警，只会让「这条记录最后什么时候变过」
// 在半年后得到一个自信而错误的答案。
//
// 清单从系统目录枚举，判据是「有没有 updated_at 这一列」——不需要豁免机制，
// 因为没有这一列的表天然不在范围内。新表加了这一列却忘了挂触发器就会红。
func TestUpdatedAtIsMaintainedByTrigger(t *testing.T) {
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	conn, err := db.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(context.Background(), `
		SELECT c.relname,
		       EXISTS (SELECT 1
		                 FROM pg_trigger tg
		                 JOIN pg_proc p ON p.oid = tg.tgfoid
		                WHERE tg.tgrelid = c.oid
		                  AND NOT tg.tgisinternal
		                  AND p.proname = 'touch_updated_at')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		   AND EXISTS (SELECT 1 FROM pg_attribute a
		                WHERE a.attrelid = c.oid
		                  AND a.attname = 'updated_at' AND a.attnum > 0)
		 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var name string
		var hasTrigger bool
		if err := rows.Scan(&name, &hasTrigger); err != nil {
			t.Fatal(err)
		}
		checked++
		if !hasTrigger {
			t.Errorf("表 %s 有 updated_at 列但没挂 touch_updated_at 触发器——"+
				"那一列会一直停在创建时间上，而且不会有任何报错", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("一张带 updated_at 的表都没枚举到——这个检查本身失效了")
	}

	// 上面只证明触发器**挂着**，不证明它**干活**。
	//
	// 教训来自同一轮的另一处：策略谓词的形状断言对 `... OR true` 完全失明，
	// 因为它要的那几个词一个不少。触发器这里同样存在形状与行为的缝隙——
	// 一个 RETURN NEW 却不改 updated_at 的函数，上面全部断言照样绿。
	// 探针走管理员连接：keel_app 对 merchants 只有 SELECT（00005 收窄的
	// GRANT 面），而这里要验的是触发器，不是权限。
	admin, err := pgx.Connect(context.Background(), db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())

	var before, after time.Time
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO merchants (code, name) VALUES ('touch-probe', '触发器探针')
		 ON CONFLICT (code) DO UPDATE SET name = excluded.name
		 RETURNING updated_at`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(context.Background(),
		`UPDATE merchants SET name = '触发器探针（改过）' WHERE code = 'touch-probe'
		 RETURNING updated_at`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(),
		`DELETE FROM merchants WHERE code = 'touch-probe'`); err != nil {
		t.Fatal(err)
	}
	if !after.After(before) {
		t.Errorf("改了一行之后 updated_at 没有前进：%v → %v——"+
			"触发器挂着但没干活", before, after)
	}
}

// 库里的每一张表都必须在设计文档里有 DDL。
//
// CONTRIBUTING 的硬规矩二写着「数据库表结构的唯一真相源是数据模型文档」，
// 而此前没有任何东西在守这一条的**这个方向**：
//
//   - check_tenancy.py 只读文档，库里多出一张表它一无所知
//   - Go 侧的闸门只读系统目录，对未登记的表按默认类别查——查得很严，
//     但从不问「这张表凭什么在这里」
//
// 于是「建了一张没写进设计文档的表」是一个全绿的状态。这不是假想：
// 买家会话表 user_tokens 就是这么来的（契约要求服务端吊销 refresh_token，
// 而数据模型 §9 当时没有任何买家侧的会话表），它在库里存在了一整个任务的
// 时间而没有任何测试提过一句。
//
// 刻意不在文档里的表（goose 自己的迁移记录表）在 db/tenancy.json 里写
// "documented": false 并附理由——和这个仓库其余豁免一样，是个需要解释的动作。
func TestEveryTableInTheDatabaseIsDocumented(t *testing.T) {
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	conn, err := db.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	manifest := loadManifest(t)

	const schemaDoc = "../../docs/电商系统-数据模型设计.md"
	doc, err := os.ReadFile(schemaDoc)
	if err != nil {
		t.Fatal(err)
	}
	// 只认 DDL，不认散文里提到的表名 —— 「§14 用一段散文补丁修改 §13 的 DDL」
	// 那种写法躲过了所有机械检查，正是这里不该重蹈的。
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(
		`(?m)^CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)\s*\(`).FindAllSubmatch(doc, -1) {
		documented[string(m[1])] = true
	}
	if len(documented) < 30 {
		t.Fatalf("设计文档里只解析出 %d 张表的 DDL，这个检查本身失效了", len(documented))
	}

	rows, err := conn.Query(context.Background(),
		`SELECT c.relname FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	live := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		live[name] = true

		entry := manifest.Tables[name]
		exempt := entry.Documented != nil && !*entry.Documented
		if !documented[name] && !exempt {
			t.Errorf("表 %s 在库里，但设计文档里没有它的 CREATE TABLE——"+
				"要么把 DDL 补进文档（CONTRIBUTING 硬规矩二：表结构的唯一真相源是"+
				"数据模型文档），要么在 db/tenancy.json 里写 \"documented\": false 并说明理由",
				name)
		}
		if exempt && entry.Reason == "" {
			t.Errorf("表 %s 标了 documented:false 却没写理由", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// documented_only 是「文档里有、库里还没有」。表真的建出来之后这个标记就过期了，
	// 留着会让下一个人以为它还没落地。
	for name, entry := range manifest.Tables {
		if entry.DocumentedOnly && live[name] {
			t.Errorf("表 %s 在 db/tenancy.json 里还标着 documented_only，"+
				"但它已经建出来了——把这个标记摘掉", name)
		}
	}
}
