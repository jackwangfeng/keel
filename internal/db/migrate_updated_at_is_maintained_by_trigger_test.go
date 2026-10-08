package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

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
