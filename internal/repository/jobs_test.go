package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 任务队列（数据模型 §12）的行为闸门。
//
// 这一组守的是 jobs 那张表自己的性质，而不是商品理解的性质 ——
// 后者在 internal/service/index_test.go，那边按「一轮」断言，
// 刻意不去看队列里有几条（理由写在那个文件的 indexRound 上）。
//
// 这里要能区分的是三对长得很像的事：
//
//	· 「取到了任务」 vs 「取到了**别人**的任务」（每租户在途上限）
//	· 「上限在挡」   vs 「上限把机器闲住了」（兜底那一趟）
//	· 「失败了」     vs 「失败并且下次不会立刻再来」（指数退避）
//
// 每一对里的后一半才是这套方案在多租户下成立的理由，而它们都不会自己报错。

const testQueue = repository.QueueProductUnderstanding

// jobsFixture 播若干家商户，并提供一条管理员连接直接看 jobs 表。
type jobsFixture struct {
	admin     *pgx.Conn
	repo      *repository.Repo
	merchants []int64
}

func newJobsFixture(t *testing.T, tag string, n int) *jobsFixture {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	f := &jobsFixture{admin: admin}
	suffix := fmt.Sprintf("%s-%d", tag, time.Now().UnixNano())
	for i := 0; i < n; i++ {
		var mid int64
		// status = 2（停用）：夹具不是营业中的店，理由同 seedOneProduct。
		if err := admin.QueryRow(ctx,
			`INSERT INTO merchants (code, name, status) VALUES ($1,'Q',2) RETURNING id`,
			fmt.Sprintf("%s-%d", suffix, i)).Scan(&mid); err != nil {
			t.Fatal(err)
		}
		f.merchants = append(f.merchants, mid)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, stmt := range []string{
			`DELETE FROM jobs      WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, f.merchants); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	pool, err := db.NewPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.repo = repository.New(pool)
	return f
}

// enqueue 以某家商户的身份入队一条。
func (f *jobsFixture) enqueue(t *testing.T, mid int64, key string) bool {
	t.Helper()
	var ok bool
	err := f.repo.WithTenant(tenant.NewContext(context.Background(), mid),
		func(tx repository.Tx) error {
			var e error
			ok, e = tx.EnqueueJob(context.Background(), repository.NewJob{
				Queue: testQueue, JobKey: key, Payload: []byte(`{"product_id":1}`),
			})
			return e
		})
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// 这个队列夹具自己的队列名：出队是跨租户的，共用 product.understanding
// 会让同一个包里别的测试留下的任务被捞进来。给每个夹具一个独立的队列名
// 是唯一能把它们隔开的办法 —— 而那本身就说明了这张表的性质。
func (f *jobsFixture) queueOf(tag string) string {
	return testQueue + "." + tag
}

func (f *jobsFixture) enqueueTo(t *testing.T, mid int64, queue, key string) {
	t.Helper()
	err := f.repo.WithTenant(tenant.NewContext(context.Background(), mid),
		func(tx repository.Tx) error {
			_, e := tx.EnqueueJob(context.Background(), repository.NewJob{
				Queue: queue, JobKey: key, Payload: []byte(`{"product_id":1}`),
			})
			return e
		})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *jobsFixture) row(t *testing.T, id int64) (status int16, attempts int32,
	lastErr *string, runAfter time.Time, lockedBy *string) {
	t.Helper()
	if err := f.admin.QueryRow(context.Background(),
		`SELECT status, attempts, last_error, run_after, locked_by FROM jobs WHERE id = $1`,
		id).Scan(&status, &attempts, &lastErr, &runAfter, &lockedBy); err != nil {
		t.Fatal(err)
	}
	return
}

// ---------------------------------------------------------------------------
// 一、入队：租户由默认值填，同一个任务不重复入队
// ---------------------------------------------------------------------------

// 入队那条语句里一个 merchant_id 都没有，那一列由 DEFAULT current_merchant() 填。
//
// **这张表没有 RLS**（00022 文件头第一节），所以「入队写不出别人的租户」
// 这件事没有 WITH CHECK 兜底，全靠那个默认值。把它删掉不会有任何别的断言变红。
func TestEnqueueTakesTheTenantFromTheColumnDefault(t *testing.T) {
	f := newJobsFixture(t, "enq", 2)
	f.enqueue(t, f.merchants[0], "product:1")
	f.enqueue(t, f.merchants[1], "product:1") // 同一个 job_key，不同租户

	var owners []int64
	rows, err := f.admin.Query(context.Background(),
		`SELECT merchant_id FROM jobs WHERE merchant_id = ANY($1) ORDER BY merchant_id`,
		f.merchants)
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
	if len(owners) != 2 || owners[0] != f.merchants[0] || owners[1] != f.merchants[1] {
		t.Fatalf("两条任务的 merchant_id 是 %v，期望 %v —— "+
			"那一列不是由 DEFAULT current_merchant() 填的，"+
			"而这张表没有 RLS，也就没有第二道拦得住它", owners, f.merchants)
	}
}

// uk_jobs_pending：同一个任务在**未完成**时不会重复入队。
//
// 条件里必须包含「1 执行中」，否则任务刚被取走就能再入一条 —— 于是同一件商品
// 被两个 worker 同时加工，钱花两次而结果一样，不报错。
func TestEnqueueIsIdempotentWhilePending(t *testing.T) {
	f := newJobsFixture(t, "dedup", 1)
	mid := f.merchants[0]

	if !f.enqueue(t, mid, "product:1") {
		t.Fatal("第一次入队没成功")
	}
	if f.enqueue(t, mid, "product:1") {
		t.Fatal("同一个任务入了第二条 —— uk_jobs_pending 没挡住，" +
			"同一件商品会被加工两遍")
	}

	// 取走之后（status = 1 执行中）仍然不许再入。
	jobs, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: testQueue, Limit: 10, PerTenantInflight: 10, WorkerID: "w1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("出队拿到 %d 条，期望 1 条", len(jobs))
	}
	if f.enqueue(t, mid, "product:1") {
		t.Fatal("任务正在执行中却又入了一条 —— uk_jobs_pending 的 WHERE 里" +
			"少了 status = 1，任务刚被取走就能再入一条")
	}

	// 做完之后可以再入：同一个商品当然要能被反复重新加工。
	if err := f.repo.FinishJobs(context.Background(), []int64{jobs[0].ID}); err != nil {
		t.Fatal(err)
	}
	if !f.enqueue(t, mid, "product:1") {
		t.Fatal("任务已经成功了却还是入不了队 —— uk_jobs_pending 的 WHERE 里" +
			"多包含了 status = 2，这件商品从此再也不会被重新加工")
	}
}

// ---------------------------------------------------------------------------
// 二、每租户在途上限 —— 这套队列在多租户下成立的前提
// ---------------------------------------------------------------------------

// 商品理解服务设计 §8：「一个新商家导入一次商品目录，就能让所有其他店铺的
// 搜索索引停止更新几小时。」priority 挡不住（它只在租户内排序），
// 真正起作用的是这条上限。
//
// 造的场景：A 家一大堆待办 + 已经有 2 条在途（上限就是 2），B 家一条。
// 出队必须给出 B 家那条。
//
// **阳性对照就在同一条测试里**：把上限摘掉（PerTenantInflight = 0，
// 也就是兜底那条语句）再取一次，拿到的必须是 A 家的 —— 否则上面那半句
// 可能只是因为「A 家的任务根本没进候选」，而不是因为上限在挡。
func TestPerTenantInflightCapLetsOtherTenantsThrough(t *testing.T) {
	f := newJobsFixture(t, "fair", 2)
	q := f.queueOf("fair")
	a, b := f.merchants[0], f.merchants[1]

	// A 家先入 5 条，其中 2 条被取走（在途）。
	for i := 0; i < 5; i++ {
		f.enqueueTo(t, a, q, fmt.Sprintf("product:%d", i))
	}
	inflight, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 2, PerTenantInflight: 2, WorkerID: "w1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inflight) != 2 {
		t.Fatalf("先取了 %d 条，期望 2 条", len(inflight))
	}

	// B 家入一条。**它排在 A 家那 3 条剩余任务的后面**（id 更大），
	// 所以「按 id 取」会先给 A 家的 —— 只有上限在挡时才轮得到 B。
	f.enqueueTo(t, b, q, "product:b")

	got, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 1, PerTenantInflight: 2, WorkerID: "w2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("出队拿到 %d 条，期望 1 条", len(got))
	}
	if got[0].MerchantID != b {
		t.Fatalf("出队拿到的是商户 %d 的任务，期望 %d（B 家）—— "+
			"每租户在途上限没生效。A 家一次目录导入就能占满整个 worker 池，"+
			"而所有其他店铺的搜索索引会停止更新几小时（设计 §8）。"+
			"注意这不会报任何错：队列在跑、worker 在跑、日志干净",
			got[0].MerchantID, b)
	}

	// 阳性对照：摘掉上限之后，同一时刻取到的是 A 家的。
	// 没有这一句，上面那条在「A 家的任务压根不在候选集里」时同样绿。
	f.enqueueTo(t, b, "unused-queue", "noop") // 保持 B 家在库里有别的行，无关紧要
	nocap, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 1, PerTenantInflight: 0, WorkerID: "w3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(nocap) != 1 || nocap[0].MerchantID != a {
		t.Fatalf("摘掉上限之后取到的是 %+v，期望商户 %d（A 家）的任务 —— "+
			"A 家的任务本来就不在候选集里，上面那条断言证明不了上限在工作",
			nocap, a)
	}
}

// 兜底那一趟：只剩一个租户有任务、而他已经到上限时，**不能让 worker 空转**。
//
// §12 原话：公平的目的是防饿死，不是让机器闲着。
//
// 这条与上一条是一对，方向相反：上一条证明上限在挡，这一条证明它挡得不过分。
// 只有上一条的话，一个「永远返回空」的实现也是绿的。
func TestSingleTenantIsNotStarvedByItsOwnCap(t *testing.T) {
	f := newJobsFixture(t, "solo", 1)
	q := f.queueOf("solo")
	a := f.merchants[0]

	for i := 0; i < 4; i++ {
		f.enqueueTo(t, a, q, fmt.Sprintf("product:%d", i))
	}
	// 先占满上限。
	if _, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 2, PerTenantInflight: 2, WorkerID: "w1",
	}); err != nil {
		t.Fatal(err)
	}
	// 系统里只剩他有任务。公平路径会一条都取不到，兜底那一趟必须补上。
	got, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 2, PerTenantInflight: 2, WorkerID: "w2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("系统里只剩一个租户有任务，而他被自己的上限卡住了，" +
			"worker 全都空转 —— §12 的第二条出队语句（去掉租户限制再取一次）" +
			"没有落地。症状是队列里堆着活而机器闲着，不报错")
	}
	for _, j := range got {
		if j.MerchantID != a {
			t.Fatalf("兜底那一趟取到了商户 %d 的任务，而库里只有 %d 家", j.MerchantID, a)
		}
	}
}

// SKIP LOCKED：两个并发消费者不会拿到同一条任务。
//
// §12 把它叫做「整个方案的胜负手」：没有它，多个 worker 的 FOR UPDATE 会排在
// 同一批行上互相等待，队列退化成全局串行 —— 起十个 worker 和起一个一样快。
//
// 这里造的是更要紧的那一半：不只是「不会等」，而是**不会重复**。
// 同一条任务被两个 worker 拿到的后果是同一件商品被算两遍，钱花两次，
// 结果一样，不报错。
func TestConcurrentDequeueNeverHandsOutTheSameJobTwice(t *testing.T) {
	f := newJobsFixture(t, "skip", 1)
	q := f.queueOf("skip")
	a := f.merchants[0]
	const n = 20
	for i := 0; i < n; i++ {
		f.enqueueTo(t, a, q, fmt.Sprintf("product:%d", i))
	}

	type result struct {
		jobs []repository.Job
		err  error
	}
	const workers = 4
	ch := make(chan result, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			jobs, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
				Queue: q, Limit: 5, PerTenantInflight: 0, // 不设上限：这里验的是 SKIP LOCKED
				WorkerID: fmt.Sprintf("w%d", w),
			})
			ch <- result{jobs, err}
		}(w)
	}

	seen := map[int64]int{}
	total := 0
	for i := 0; i < workers; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("并发出队报错: %v", r.err)
		}
		for _, j := range r.jobs {
			seen[j.ID]++
			total++
		}
	}
	for id, c := range seen {
		if c > 1 {
			t.Errorf("任务 %d 被交出去了 %d 次 —— FOR UPDATE SKIP LOCKED 与占位"+
				"不在同一个事务里，同一件商品会被算两遍，钱花两次而结果一样，"+
				"不报任何错", id, c)
		}
	}
	if total == 0 {
		t.Fatal("四个并发消费者一条任务都没取到 —— 上面那条去重断言在空集上空转")
	}
	if len(seen) != total {
		t.Fatalf("交出去 %d 条但只有 %d 个不同的 id", total, len(seen))
	}
}

// ---------------------------------------------------------------------------
// 三、失败：指数退避、死信、回收
// ---------------------------------------------------------------------------

// 失败 → 退避放回队列，run_after 在未来，last_error 落库，占位痕迹清干净。
//
// **退避这一半最容易被漏掉**：一个只把 status 改回 0 的实现在这条测试里
// 只有 run_after 那一句会红，而它在生产里的样子是一个连续失败的任务
// 以毫秒级频率被反复重试，把引擎和数据库一起打满。
func TestRetryBacksOffExponentiallyAndRecordsTheError(t *testing.T) {
	f := newJobsFixture(t, "retry", 1)
	q := f.queueOf("retry")
	f.enqueueTo(t, f.merchants[0], q, "product:1")

	jobs, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 1, PerTenantInflight: 1, WorkerID: "w1",
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("出队失败: %v / %d 条", err, len(jobs))
	}
	if jobs[0].Attempts != 1 {
		t.Fatalf("出队之后 attempts 是 %d，期望 1 —— 占位那一步没有加计数，"+
			"于是 max_attempts 永远到不了，一条坏任务会被无限重试", jobs[0].Attempts)
	}

	before := time.Now()
	if err := f.repo.RetryJob(context.Background(), jobs[0].ID, "注入的故障"); err != nil {
		t.Fatal(err)
	}
	status, attempts, lastErr, runAfter, lockedBy := f.row(t, jobs[0].ID)
	if status != 0 {
		t.Errorf("失败之后 status 是 %d，期望 0（待执行）", status)
	}
	if attempts != 1 {
		t.Errorf("attempts 是 %d，期望还是 1（失败不加计数，占位才加）", attempts)
	}
	if lastErr == nil || *lastErr != "注入的故障" {
		t.Errorf("last_error 是 %v，期望「注入的故障」—— 失败原因没落库，"+
			"而 product_understanding.last_error 按挂账是不能写的，"+
			"两处都不写就等于这次失败在库里没有痕迹", lastErr)
	}
	if lockedBy != nil {
		t.Errorf("locked_by 还是 %q —— 占位痕迹没清，排查卡死时会指向一个"+
			"早就放手了的 worker", *lockedBy)
	}
	// 1 秒 × 2^1 = 2 秒（§12 的公式，attempts 在占位时已经加到 1）。
	if d := runAfter.Sub(before); d < time.Second || d > 5*time.Second {
		t.Errorf("run_after 在 %v 之后，期望约 2 秒 —— 指数退避没生效。"+
			"一个连续失败的任务会被以毫秒级频率反复重试，把引擎和数据库一起打满", d)
	}

	// 退避期内**取不出来**。少了这一条，上面那个时间戳可以是对的而查询不看它。
	got, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 1, PerTenantInflight: 1, WorkerID: "w2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("退避期内任务又被取出来了 —— 出队那条 SQL 的 " +
			"`run_after <= now()` 没了，退避于是只是一个没人读的时间戳")
	}
}

// 次数用尽 → 死信，而且**要有一个能被 errors.Is 认出来的信号**。
//
// §7：「超限进死信队列并告警。」一个被忽略的 bool 返回值与「没有这回事」
// 长得一模一样，所以它是 sentinel。
func TestRetryDeadLettersWhenAttemptsAreExhausted(t *testing.T) {
	f := newJobsFixture(t, "dead", 1)
	q := f.queueOf("dead")
	f.enqueueTo(t, f.merchants[0], q, "product:1")

	var id int64
	for i := 0; i < 5; i++ { // max_attempts 默认 5
		jobs, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
			Queue: q, Limit: 1, PerTenantInflight: 1, WorkerID: "w1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 {
			t.Fatalf("第 %d 次出队拿到 %d 条，期望 1 条 —— 退避那一步把它弄丢了", i, len(jobs))
		}
		id = jobs[0].ID
		err = f.repo.RetryJob(context.Background(), id, fmt.Sprintf("第 %d 次失败", i+1))
		if i < 4 {
			if err != nil {
				t.Fatalf("第 %d 次失败就进死信了（max_attempts = 5）: %v", i+1, err)
			}
			// 把退避拨过去，好接着跑下一轮。
			if _, e := f.admin.Exec(context.Background(),
				`UPDATE jobs SET run_after = now() - interval '1 second' WHERE id = $1`,
				id); e != nil {
				t.Fatal(e)
			}
			continue
		}
		if !errors.Is(err, repository.ErrJobDeadLettered) {
			t.Fatalf("第 5 次失败之后返回的是 %v，期望 ErrJobDeadLettered —— "+
				"没有这个信号的话，一条永远失败的任务会安静地被无限重试，"+
				"而 §7 要求超限进死信并告警", err)
		}
	}
	status, attempts, _, _, _ := f.row(t, id)
	if status != 3 {
		t.Errorf("用尽次数之后 status 是 %d，期望 3（死信）", status)
	}
	if attempts != 5 {
		t.Errorf("attempts 是 %d，期望 5", attempts)
	}

	// 死信不再出队。
	got, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 10, PerTenantInflight: 10, WorkerID: "w9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("死信又被取出来了 —— 出队那条 SQL 的 status = 0 条件不在了")
	}
}

// 回收卡死的任务，而且**次数用尽的那些直接进死信，不放回 0**。
//
// 这一条盯的是一个会把整个队列卡死的组合，写在 repository/jobs.go 的文件头
// 第三节：jobs 上有 CHECK (attempts <= max_attempts)，而占位是 attempts + 1。
// 把一条 attempts 已经等于 max_attempts 的任务无脑放回 0，下一次占位就违反约束 ——
// 而占位跑在出队那个事务里，**一条坏任务会让整批出队失败，队列从此不动**。
func TestReapPutsExhaustedJobsStraightIntoTheDeadLetter(t *testing.T) {
	f := newJobsFixture(t, "reap", 1)
	q := f.queueOf("reap")
	f.enqueueTo(t, f.merchants[0], q, "product:normal")
	f.enqueueTo(t, f.merchants[0], q, "product:exhausted")

	jobs, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 10, PerTenantInflight: 10, WorkerID: "crashed",
	})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("出队失败: %v / %d 条", err, len(jobs))
	}
	var normal, exhausted int64
	for _, j := range jobs {
		if j.JobKey == "product:exhausted" {
			exhausted = j.ID
		} else {
			normal = j.ID
		}
	}
	// 把其中一条推到「这一次占位正好是最后一次」的状态，并让两条都看起来卡了很久。
	if _, err := f.admin.Exec(context.Background(),
		`UPDATE jobs SET attempts = max_attempts WHERE id = $1`, exhausted); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(context.Background(),
		`UPDATE jobs SET locked_at = now() - interval '1 hour' WHERE id = ANY($1)`,
		[]int64{normal, exhausted}); err != nil {
		t.Fatal(err)
	}

	n, err := f.repo.ReapStuckJobs(context.Background(), q, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("回收了 %d 条，期望 2 条 —— 卡在执行中的任务占着那家店的"+
			"在途配额，不回收的话他的商品从此一件也不再被加工，而且不报错", n)
	}
	if st, _, _, _, _ := f.row(t, normal); st != 0 {
		t.Errorf("还有重试余量的任务被回收成了 status = %d，期望 0", st)
	}
	if st, _, _, _, _ := f.row(t, exhausted); st != 3 {
		t.Errorf("次数已经用尽的任务被回收成了 status = %d，期望 3（死信）", st)
	}

	// 真正的后果验一遍：回收之后还能出队。次数用尽那条如果被放回 0，
	// 下面这次出队会在 chk_jobs_attempts 上整批失败。
	got, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 10, PerTenantInflight: 10, WorkerID: "w2",
	})
	if err != nil {
		t.Fatalf("回收之后出队报错了: %v —— 次数用尽的任务被放回了 0，"+
			"占位那一步的 attempts + 1 违反 chk_jobs_attempts，"+
			"而占位是整批做的：一条坏任务让整个队列不动", err)
	}
	if len(got) != 1 || got[0].ID != normal {
		t.Fatalf("回收之后出队拿到 %+v，期望只有那条还有余量的（%d）", got, normal)
	}
}

// 成功的任务保留一段时间再清理（§12：保留 7 天），死信永久保留。
func TestPurgeRemovesOldDoneJobsButKeepsDeadLetters(t *testing.T) {
	f := newJobsFixture(t, "purge", 1)
	q := f.queueOf("purge")
	f.enqueueTo(t, f.merchants[0], q, "product:done")
	f.enqueueTo(t, f.merchants[0], q, "product:dead")

	jobs, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 10, PerTenantInflight: 10, WorkerID: "w1",
	})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("出队失败: %v / %d 条", err, len(jobs))
	}
	for _, j := range jobs {
		if j.JobKey == "product:dead" {
			if _, err := f.admin.Exec(context.Background(),
				`UPDATE jobs SET status = 3 WHERE id = $1`, j.ID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := f.repo.FinishJobs(context.Background(), []int64{j.ID}); err != nil {
			t.Fatal(err)
		}
	}
	// 把两条都做旧。
	//
	// **必须先把 touch_jobs_updated_at 关掉**，否则这一句什么也做不到：
	// 00022 给这张表挂了 BEFORE UPDATE 触发器，**任何一次 UPDATE 都会把
	// updated_at 推回 now()** —— 包括这一句自己。带着触发器跑的话
	// `SET updated_at = now() - interval '30 days'` 的结果是 updated_at = now()，
	// 两条任务都还在保留期里，PurgeFinishedJobs 清理 0 条，
	// 而失败信息会指向清理逻辑，真因却在这三行里。
	//
	// 关触发器要表属主权限，所以走 f.admin 而不是 f.repo 那条 keel_app 连接。
	// 这是测试夹具的特权，不是被测代码的 —— 生产路径上那个触发器一直开着，
	// 而它开着正是对的：status = 2 那一行的 updated_at 就该是「做完的时刻」，
	// 保留期从那时起算。
	if _, err := f.admin.Exec(context.Background(),
		`ALTER TABLE jobs DISABLE TRIGGER touch_jobs_updated_at`); err != nil {
		t.Fatal(err)
	}
	_, ageErr := f.admin.Exec(context.Background(),
		`UPDATE jobs SET updated_at = now() - interval '30 days' WHERE queue = $1`, q)
	if _, err := f.admin.Exec(context.Background(),
		`ALTER TABLE jobs ENABLE TRIGGER touch_jobs_updated_at`); err != nil {
		t.Fatal(err)
	}
	if ageErr != nil {
		t.Fatal(ageErr)
	}

	n, err := f.repo.PurgeFinishedJobs(context.Background(), q, 7*24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("清理了 %d 条，期望 1 条（只有成功的那条）", n)
	}
	var left string
	if err := f.admin.QueryRow(context.Background(),
		`SELECT job_key FROM jobs WHERE queue = $1`, q).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != "product:dead" {
		t.Fatalf("留下的是 %q，期望 product:dead —— 死信被清掉了，"+
			"而它们是唯一记着「这件事反复失败」的地方（§12：死信永久保留）", left)
	}

	// 保留期内的成功任务不许被清掉。
	f.enqueueTo(t, f.merchants[0], q, "product:fresh")
	fresh, err := f.repo.DequeueJobs(context.Background(), repository.DequeueRequest{
		Queue: q, Limit: 1, PerTenantInflight: 1, WorkerID: "w1",
	})
	if err != nil || len(fresh) != 1 {
		t.Fatalf("出队失败: %v / %d 条", err, len(fresh))
	}
	if err := f.repo.FinishJobs(context.Background(), []int64{fresh[0].ID}); err != nil {
		t.Fatal(err)
	}
	n, err = f.repo.PurgeFinishedJobs(context.Background(), q, 7*24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("清理了 %d 条刚做完的任务，期望 0 —— 保留期没生效，"+
			"排查「这件商品刚才为什么没索引」时没有任何痕迹可看", n)
	}
}
