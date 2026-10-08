package db_test

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/testdb"
)

// migrate 给本条测试换一个刚迁完的库，并返回 make migrate 的输出。
//
// 两步：
//
//  1. testdb.Reset —— 删掉本包的库，从本次运行现建的模板克隆一个新的。
//     模板是 TestMain 里对一个空库跑 make migrate 得到的，读的是工作区里
//     此刻的 db/migrations；它从不跨运行复用。为什么克隆而不是每条重迁、
//     以及这样为什么不会「暖库假绿」，写在 testdb.Reset 上。
//
//  2. 克隆已经停在 db/migrations 里最新的版本号上时，不再为每条测试起一次
//     make migrate。goose 看到版本已是最新本来也什么都不做，但进程启动加
//     集群级迁移锁让这一包的墙钟几乎全耗在排队上（CI 里这一包单独 70 秒）。
//     版本对不上（模板落后于文件、或库是空的）才真正跑 goose，缺的迁移
//     会补上。goose 能在已迁过的库上再跑一遍，由 TestMigrateIsIdempotent
//     单独守，那里不走这条捷径。
//
// 这条对本包尤其要紧：本包的测试全部是「拿系统目录核对迁移写了什么」，
// 而它们读的是**库**不是**文件**。库不跟着文件走的时候，这些断言守的是
// 上一次跑过的那份迁移，不是工作区里这份。版本号比对守的就是这件事：
// 改过已应用的迁移文件而版本号没变的情况，靠模板每次运行现建，不靠这里。
func migrate(t *testing.T) ([]byte, error) {
	t.Helper()
	if err := testdb.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if schemaAtHead(t) {
		return nil, nil
	}
	return testdb.Migrate(context.Background())
}

// schemaAtHead 报告当前库的 goose 版本是否等于 db/migrations 里最大的文件号。
// 读不到版本表或读不到目录都算「不在最新」，调用方会退回真正的 make migrate。
func schemaAtHead(t *testing.T) bool {
	t.Helper()
	head, ok := migrationHead()
	if !ok {
		return false
	}
	conn, err := pgx.Connect(context.Background(), db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var v int64
	err = conn.QueryRow(context.Background(),
		`SELECT coalesce(max(version_id), 0) FROM goose_db_version`).Scan(&v)
	if err != nil {
		return false
	}
	return v == head
}

var (
	headOnce sync.Once
	headVer  int64
	headOK   bool
)

func migrationHead() (int64, bool) {
	headOnce.Do(func() {
		ents, err := os.ReadDir("../../db/migrations")
		if err != nil {
			return
		}
		var max int64
		var saw bool
		for _, e := range ents {
			name := e.Name()
			i := 0
			for i < len(name) && name[i] >= '0' && name[i] <= '9' {
				i++
			}
			if i == 0 || i == len(name) || name[i] != '_' {
				continue
			}
			n, err := strconv.ParseInt(name[:i], 10, 64)
			if err != nil {
				continue
			}
			saw = true
			if n > max {
				max = n
			}
		}
		headVer, headOK = max, saw
	})
	return headVer, headOK
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
