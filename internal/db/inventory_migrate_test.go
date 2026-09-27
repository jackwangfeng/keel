package db_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/testdb"
)

// 库存迁移目录（db/migrations-inventory，微服务拆分阶段 1b）的两条承诺：
//
//  1. 在单体库上（core 迁移已经跑过）是**空操作**：库存四张表的列、约束、索引、策略、授权一个字不变，
//     只多一张 goose_db_version_inventory；
//  2. 在空库上建出来的库存四张表与单体库里的**逐列一致**（同一份目录，两种库）。
//
// 两库集成测试（internal/handler 的 two_db_test.go）在第二种库上把整条链路跑了一遍；
// 这一条比的是结构本身，一个默认值或一条策略写岔了也逃不掉。
func TestInventoryMigrationsMatchTheSingleDatabase(t *testing.T) {
	ctx := context.Background()
	if out, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v\n%s", err, out)
	}
	single := inventorySchema(t, db.AdminDSN())

	// 1. 单体库上跑一遍库存目录。
	out, err := exec.CommandContext(ctx, "make", "-s", "-C", filepath.Join("..", ".."), "migrate-inventory",
		"INVENTORY_GOOSE_DBSTRING="+db.AdminDSN()).CombinedOutput()
	if err != nil {
		t.Fatalf("单体库上跑库存迁移失败: %v\n%s", err, out)
	}
	if after := inventorySchema(t, db.AdminDSN()); after != single {
		t.Fatalf("库存迁移在单体库上不是空操作：\n之前：\n%s\n之后：\n%s", single, after)
	}

	// 2. 空库上建出来的与单体库一致。
	_, adminDSN, cleanup, err := testdb.NewInventoryDB(ctx, "invschema")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if fresh := inventorySchema(t, adminDSN); fresh != single {
		t.Fatalf("库存库的结构与单体库不一致：\n单体：\n%s\n库存库：\n%s", single, fresh)
	}
}

// inventorySchema 把库存四张表的结构读成一段可比较的文本（列、约束、索引、策略、授权、RLS 开关、触发器）。
// 约束比的是定义本身（pg_get_constraintdef），不比 NOT VALID 这类只关乎历史数据的修饰。
func inventorySchema(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var b strings.Builder
	for _, q := range []string{
		`SELECT table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name IN ('inventories','inventory_logs','activity_stocks','barrier')
		  ORDER BY table_name, column_name`,
		`SELECT conrelid::regclass || ' ' || conname || ' ' || replace(pg_get_constraintdef(oid), ' NOT VALID', '')
		   FROM pg_constraint
		  WHERE conrelid::regclass::text IN ('inventories','inventory_logs','activity_stocks','barrier')
		  ORDER BY 1`,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND tablename IN ('inventories','inventory_logs','activity_stocks','barrier')
		  ORDER BY indexname`,
		`SELECT tablename || ' ' || policyname || ' ' || cmd || ' ' || COALESCE(qual, '') || ' ' || COALESCE(with_check, '')
		   FROM pg_policies WHERE tablename IN ('inventories','inventory_logs','activity_stocks','barrier') ORDER BY 1`,
		`SELECT table_name || ' ' || grantee || ' ' || privilege_type FROM information_schema.role_table_grants
		  WHERE table_schema = 'public' AND grantee = 'keel_app'
		    AND table_name IN ('inventories','inventory_logs','activity_stocks','barrier') ORDER BY 1`,
		`SELECT relname || ' ' || relrowsecurity || ' ' || relforcerowsecurity FROM pg_class
		  WHERE relname IN ('inventories','inventory_logs','activity_stocks','barrier') ORDER BY 1`,
		`SELECT tgname FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid::regclass::text IN ('inventories','inventory_logs','activity_stocks','barrier') ORDER BY 1`,
	} {
		rows, err := conn.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			b.WriteString(s)
			b.WriteString("\n")
		}
		rows.Close()
	}
	return b.String()
}
