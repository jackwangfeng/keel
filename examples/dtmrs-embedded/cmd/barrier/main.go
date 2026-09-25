// barrier 验证子事务屏障的三种异常：重复请求、空回滚、悬挂。
//
// 需要 PostgreSQL（与其余 demo 不同，它们用 sqlite）——屏障的意义就在于
// 与业务 SQL 同事务提交，拿真数据库跑才算数。
//
//	docker run -d --name keel-dtmrs -e POSTGRES_PASSWORD=x -e POSTGRES_DB=dtm \
//	    -p 55433:5432 postgres:16
//	go run ./cmd/barrier
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	br "github.com/keel/examples/dtmrs-embedded/barrier"
)

const DSN = "postgres://postgres:x@127.0.0.1:55433/dtm"

var fail int

func check(name string, got, want any) {
	ok := fmt.Sprint(got) == fmt.Sprint(want)
	mark := "✅"
	if !ok {
		mark = "❌"
		fail++
	}
	fmt.Printf("%s %-52s %v\n", mark, name, got)
	if !ok {
		fmt.Printf("   期望: %v\n", want)
	}
}

// callBranch 模拟一次分支调用：同一事务内跑屏障判定 + 业务 SQL。
func callBranch(ctx context.Context, pool *pgxpool.Pool, gid, branchID, op string,
	business func(pgx.Tx) error) (br.Decision, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	b := &br.Barrier{TransType: "saga", Gid: gid, BranchID: branchID, Op: op}
	d, err := b.Decide(ctx, tx)
	if err != nil {
		return 0, err
	}
	// 只有 br.Execute 才动业务数据。另外两种是正常路径，什么都不做并返回成功。
	if d == br.Execute && business != nil {
		if err := business(tx); err != nil {
			return 0, err
		}
	}
	return d, tx.Commit(ctx)
}

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, DSN)
	must(err)
	defer pool.Close()

	// 屏障表：与 dtmrs migrate() 产物一致
	_, err = pool.Exec(ctx, `
	DROP TABLE IF EXISTS barrier; DROP TABLE IF EXISTS stock;
	CREATE TABLE barrier (
	    trans_type TEXT NOT NULL, gid TEXT NOT NULL, branch_id TEXT NOT NULL,
	    op TEXT NOT NULL, barrier_id TEXT NOT NULL, reason TEXT NOT NULL,
	    create_time BIGINT NOT NULL,
	    PRIMARY KEY (gid, branch_id, op, barrier_id));
	CREATE TABLE stock (sku TEXT PRIMARY KEY, qty INT NOT NULL);
	INSERT INTO stock VALUES ('A', 100);`)
	must(err)

	deduct := func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE stock SET qty = qty - 1 WHERE sku='A'`)
		return e
	}
	restore := func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE stock SET qty = qty + 1 WHERE sku='A'`)
		return e
	}
	qty := func() int {
		var n int
		must(pool.QueryRow(ctx, `SELECT qty FROM stock WHERE sku='A'`).Scan(&n))
		return n
	}

	fmt.Println("--- 异常一：重复请求（同一正向分支被投递两次）---")
	d1, _ := callBranch(ctx, pool, "g1", "01", "action", deduct)
	d2, _ := callBranch(ctx, pool, "g1", "01", "action", deduct)
	check("第一次判定", d1, br.Execute)
	check("第二次判定", d2, br.Duplicated)
	check("库存只被扣了一次", qty(), 99)

	fmt.Println("\n--- 异常二：空回滚（补偿先到，正向从未执行）---")
	d3, _ := callBranch(ctx, pool, "g2", "01", "compensate", restore)
	check("补偿判定", d3, br.NullCompensation)
	check("库存未被错误回补", qty(), 99)

	fmt.Println("\n--- 异常三：悬挂（补偿已到，迟到的正向不得执行）---")
	d4, _ := callBranch(ctx, pool, "g3", "01", "compensate", restore)
	check("补偿先到，判定", d4, br.NullCompensation)
	d5, _ := callBranch(ctx, pool, "g3", "01", "action", deduct)
	check("迟到的正向，判定", d5, br.Duplicated)
	check("库存未被悬挂的正向扣减", qty(), 99)

	fmt.Println("\n--- 正常链路：正向执行后再补偿 ---")
	d6, _ := callBranch(ctx, pool, "g4", "01", "action", deduct)
	check("正向判定", d6, br.Execute)
	check("扣减生效", qty(), 98)
	d7, _ := callBranch(ctx, pool, "g4", "01", "compensate", restore)
	check("补偿判定", d7, br.Execute)
	check("回补生效", qty(), 99)
	d8, _ := callBranch(ctx, pool, "g4", "01", "compensate", restore)
	check("重复补偿判定", d8, br.Duplicated)
	check("未重复回补", qty(), 99)

	fmt.Println("\n--- 并发：同一分支被 50 个协程同时投递 ---")
	var exec, dup, errs int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := callBranch(ctx, pool, "g5", "01", "action", deduct)
			switch {
			case err != nil:
				atomic.AddInt64(&errs, 1)
			case d == br.Execute:
				atomic.AddInt64(&exec, 1)
			case d == br.Duplicated:
				atomic.AddInt64(&dup, 1)
			}
		}()
	}
	wg.Wait()
	fmt.Printf("   Execute=%d Duplicated=%d 错误=%d\n", exec, dup, errs)
	check("恰好一次 Execute", exec, 1)
	check("库存恰好被扣一次", qty(), 98)

	fmt.Printf("\n===== 失败项: %d =====\n", fail)
	if fail > 0 {
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
