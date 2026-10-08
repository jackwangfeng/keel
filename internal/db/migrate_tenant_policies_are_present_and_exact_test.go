package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

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

		if spec.Policy == "directory-log" {
			// merchant_revisions 一张表（00024）。它是唯一一类**两条**策略的表，
			// 所以不走下面「恰好一条 ALL」那套，而是把两条逐字钉死：
			//
			//   directory_read  FOR SELECT  USING (true)
			//   platform_write  FOR INSERT  WITH CHECK (platform_scope())
			//
			// 要盯的是写侧：把 platform_write 的 WITH CHECK 放宽成 true，或者把
			// directory_read 改成 FOR ALL（USING (true) 于是同时充当写谓词），
			// 「租户 1 的上下文里停用租户 2」就重新写得进去——而读侧一切照旧。
			// 多出第三条 permissive 策略同理（策略之间是 OR）。
			checkDirectoryLogPolicies(t, tbl, got)
			continue
		}

		if spec.Policy == "platform-current" {
			// merchant_domains 一张表（00340）。与 directory-log 同一类形状，
			// 三条策略逐字钉死，理由写在检查函数头上。
			checkPlatformCurrentPolicies(t, tbl, got)
			continue
		}

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
		case "column-scope":
			// staff 一张表。它的 merchant_id **可空**（NULL = 平台级操作员），
			// 于是标准的列比较对那一行恒为假 —— 平台级操作员对所有人不可见，
			// 包括他自己。谓词改成对一个可以是 NULL 的作用域值做
			// IS NOT DISTINCT FROM，完整论证在 00017 的文件头与清单里这一类的说明。
			//
			// 逐字比对，期望值从清单里取：这个谓词是隔离本身，
			// 「租户作用域下逐行等价于标准策略」这条性质靠的就是它的确切形状，
			// 换成 OR 写法（`= current_merchant() OR merchant_id IS NULL`）之后
			// 读侧看不出差别，而写侧变成一个提权入口。
			if entry.PolicyQual == "" {
				t.Errorf("%s 是 column-scope 类，但清单里没写 policy_qual —— "+
					"这一类的谓词不是通用形状，没有期望值就等于没在检查", tbl)
				continue
			}
			if p.qual != entry.PolicyQual {
				t.Errorf("%s: 策略谓词是 %q，清单说应该是 %q", tbl, p.qual, entry.PolicyQual)
			}
		case "parent-scope":
			// 形状与 parent 完全一样，只差子查询里比的是清单声明的那个作用域函数
			// 而不是 current_merchant()（父表的租户列可空，current_merchant()
			// 在平台作用域里是 RAISE 而不是 NULL）。
			//
			// 不把它并进 parent 去「两者任一」：那会让一张真正的 parent-scoped
			// 表漏掉租户比较也照样绿。
			if entry.Parent == "" || entry.ScopeFn == "" {
				t.Errorf("%s 是 parent-scope 类却没在清单里写 parent / scope_fn", tbl)
				continue
			}
			if strings.Contains(p.qual, "current_merchant()") {
				t.Errorf("%s: 策略谓词里出现了 current_merchant()（%q）—— "+
					"这一类的父表租户列可空，current_merchant() 在平台作用域里会报错，"+
					"要的是 %s", tbl, p.qual, entry.ScopeFn)
			}
			for _, needle := range []string{entry.Parent, entry.ScopeFn, entry.Via} {
				if !strings.Contains(p.qual, needle) {
					t.Errorf("%s: 策略谓词 %q 里没有 %q——"+
						"父表定租户的表，谓词必须真的去 %s 里按 %s 查",
						tbl, p.qual, needle, entry.Parent, entry.ScopeFn)
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
