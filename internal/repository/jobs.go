package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 任务队列（数据模型 §12）在 repository 边界上的那一面。
//
// ===========================================================================
// 一、这个文件里为什么全是手写 SQL
// ===========================================================================
//
// 除了入队（在 db/queries/jobs.sql 里），这张表上的每一条语句都是**跨租户**的：
// 出队要在一条查询里看见全部租户的待办并按 priority 给出全局顺序，占位 / 退避 /
// 回收 / 清理则是按 job id 或 queue 动，与「当前是哪家店」无关。
//
// 它们因此跑在 pool 上、不在任何租户事务里 —— 这与 ActiveMerchants 是同一个
// 惯例，理由写在 sweep.go 的文件头：sqlc 的产物挂在 tenantTx 上，而那个类型的
// 每一个方法都跑在一个设好 app.merchant_id 的事务里，这几条按定义不属于那一面。
//
// 第二条理由更硬：这几条语句里**必须**出现 merchant_id（公平调度就是按它分组的），
// 而 scripts/check_query_tenancy.py 不许 db/queries 里出现这个词。
// 那条检查挡的是「应用层再过滤一遍租户，于是 RLS 有没有生效变得测不出来」——
// 而 jobs 根本没有 RLS（00022 文件头第一节），把这几条塞进 db/queries 再往
// ALLOW 里加一条豁免，等于往那份清单里放一条与它要防的东西无关的条目。
//
// ===========================================================================
// 二、sweep.go 文件头那段「与 §12 的冲突」，这里补上后半段
// ===========================================================================
//
// sweep.go 写着：§12 说「worker 出队以平台身份执行，不走 RLS 注入」，而
// 「本仓库没有那个身份，也不打算有」—— 于是超时补偿的扫描按租户切成 N 段，
// 公平调度落在应用层。
//
// **那句话到今天仍然成立，而 jobs 走的是另一条路**：不给任何人绕过 RLS 的能力，
// 而是让这一张表不处在 RLS 的管辖之下。差别是可枚举的 —— 一个 BYPASSRLS 角色
// 对全部 36 张表都失效，这里失效的范围恰好是 jobs 一张。keel_app 仍然是
// NOSUPERUSER NOBYPASSRLS，db.NewPool 的自检一个字都不用改。
//
// 后果是 §12 那两条出队语句可以**逐字**落地，公平调度真的发生在 SQL 里，
// 而不是在应用层用 N 段扫描模拟出来。超时补偿那一套没有一起改过来，
// 因为 orders 是 tenant 类、有 RLS，它没有这条路可走。
//
// ===========================================================================
// 三、attempts 与 CHECK 约束之间有一个会炸的组合，写在这里
// ===========================================================================
//
// jobs 上有 `CHECK (attempts >= 0 AND attempts <= max_attempts)`，而占位那一步是
// `attempts = attempts + 1`。于是「已经用满次数的任务又被占一次」会直接违反约束，
// 而它发生在**出队那个事务里** —— 一条坏任务会让整批出队失败，队列从此不动。
//
// 正常路径上撞不上：失败那一步在 attempts >= max_attempts 时把任务转成 3 死信，
// 死信不在 status = 0 的出队范围里。**会撞上的是回收那一步**：一个 worker 在
// attempts 已经等于 max_attempts 的那一次执行中途被 kill，任务停在 status = 1，
// 回收如果无脑放回 0，下一次占位就是 max_attempts + 1。
//
// 所以 ReapStuckJobs 用的是与失败那一步**完全相同**的三目表达式。两处写法一致
// 不是巧合，是同一条规矩：任何一条把任务放回 0 的语句都要先问一次次数用完没有。
const (
	// QueueProductUnderstanding 是商品理解的队列名（§12 的例子逐字照抄）。
	QueueProductUnderstanding = "product.understanding"

	// jobStatusDead 是 jobs.status 的 3（死信）。另外三个取值
	// （0 待执行 / 1 执行中 / 2 已成功）只出现在下面几条 SQL 的字面量里，
	// 不给它们起名字：起了名字而 SQL 里仍然写数字，等于两个真相源。
	// 这一个例外是因为它要被 Go 读回来做判断。
	jobStatusDead int16 = 3
)

// NewJob 是入队的入参。**没有 MerchantID** —— 那一列的默认值是
// current_merchant()（00022 文件头第二节），而这张表没有 RLS 的 WITH CHECK
// 兜底，所以「入队写不出别人的租户」这件事全靠这个结构体里没有那个字段。
type NewJob struct {
	Queue   string
	JobKey  string
	Payload []byte
	// Priority 大的先出队，**仅在租户内有意义**（§12）。回填任务取 -10。
	Priority int16

	// MaxAttempts 非 0 时覆盖表上的默认值（5）。库存的 outbox 任务用它（EnqueueJobWithMaxAttempts 的注释）。
	MaxAttempts int32
}

// Job 是出队拿到的一行。
//
// 它只带执行任务所需的那几列：locked_by / run_after / last_error 是队列自己的
// 账，调用方读它们没有用处，而多读几列意味着多一处「行的形状变了但转换没跟上」。
type Job struct {
	ID         int64
	MerchantID int64
	Queue      string
	JobKey     string
	Payload    []byte
	// Attempts 是**已经占位过几次**（含这一次）。第一次执行时它是 1。
	Attempts int32
}

// JobTx 是入队那一面：它发生在一个有租户上下文的事务里。
type JobTx interface {
	// EnqueueJob 入队。返回 false 表示同一个任务已经在队列里（未完成），
	// 这条是正常路径而不是错误 —— uk_jobs_pending 存在的全部意义就是让调用方
	// 不必记得「我上一轮已经入过了」。
	EnqueueJob(ctx context.Context, j NewJob) (bool, error)
}

func (t tenantTx) EnqueueJob(ctx context.Context, j NewJob) (bool, error) {
	payload := j.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	var n int64
	var err error
	if j.MaxAttempts > 0 {
		n, err = t.q.EnqueueJobWithMaxAttempts(ctx, db.EnqueueJobWithMaxAttemptsParams{
			Queue: j.Queue, JobKey: j.JobKey, Payload: payload, Priority: j.Priority, MaxAttempts: j.MaxAttempts,
		})
	} else {
		n, err = t.q.EnqueueJob(ctx, db.EnqueueJobParams{
			Queue:    j.Queue,
			JobKey:   j.JobKey,
			Payload:  payload,
			Priority: j.Priority,
		})
	}
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// DequeueRequest 是一次出队的参数。
type DequeueRequest struct {
	Queue string
	// Limit 这一次最多取几条。
	Limit int
	// PerTenantInflight 是**每租户在途上限**，也是这套队列在多租户下成立的前提。
	//
	// 商品理解服务设计 §8 把理由写得很重：priority 只在租户内排序，
	// 「一个新商家导入一次商品目录，就能让所有其他店铺的搜索索引停止更新几小时」。
	// 真正起作用的不是低优先级，是这条上限 —— 回填再多也只能占住有限几个 worker。
	//
	// <= 0 表示不设上限，此时第一趟与兜底那一趟等价。**这不是一个该在生产里
	// 出现的取值**，它存在只是为了让测试能把上限摘掉、看见「没有它会怎样」。
	PerTenantInflight int
	// WorkerID 落进 locked_by，排查卡死用（§12）。
	WorkerID string
}

// DequeueJobs 取一批任务并在同一个事务里把它们置为执行中。
//
// ===========================================================================
// 两条语句，逐字照抄 §12
// ===========================================================================
//
// 第一条是公平路径：跳过在途任务已达上限的租户。
// 第二条是兜底：第一条没取到时，去掉租户限制再取一次。
//
// **为什么必须有第二条**（§12 原话）：只有第一条的话，系统里只剩一个租户有任务时，
// 他会被自己的上限卡住，而 worker 全都空转。公平的目的是防饿死，不是让机器闲着。
//
// ### SKIP LOCKED 是整个方案的胜负手
//
// 没有它，多个 worker 的 FOR UPDATE 会排在同一批行上互相等待，队列直接退化成
// 全局串行 —— 起十个 worker 和起一个一样快。这个子句是 PG 9.5 就有的东西，
// 但它是「用数据库当队列」从玩具变成可用方案的那个分界线。
//
// ### 取与占位必须在同一个事务里
//
// FOR UPDATE 的锁随事务结束而释放。分两个事务的话，两个 worker 可以先后拿到
// 同一批行（第一个的锁已经放了、而它还没来得及把 status 改成 1），
// 于是同一件商品被算两遍 —— 钱花两次，而结果一样，没有任何东西报错。
func (r *Repo) DequeueJobs(ctx context.Context, req DequeueRequest) ([]Job, error) {
	if req.Limit <= 0 {
		return nil, nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var jobs []Job
	if req.PerTenantInflight > 0 {
		// 不设上限时**不能**把 0 传进去：HAVING count(*) >= 0 对每一组都成立，
		// busy 会装下全部在途租户，出队变成「谁在跑就永远不许他再跑」。
		// 不设上限的正确形状就是下面兜底那条语句本身。
		jobs, err = scanJobs(ctx, tx, dequeueFairSQL, req.Queue, req.Limit, req.PerTenantInflight)
		if err != nil {
			return nil, fmt.Errorf("出队（公平路径）失败: %w", err)
		}
	}
	if len(jobs) == 0 {
		// 兜底那一趟。条件是「第一趟一条都没取到」——**不是**「取够了没有」：
		// 第一趟取到 3 条而预算是 10 时不该再去掉租户限制多抓 7 条，
		// 那 7 条正是公平调度刚刚决定要让给别人的。
		jobs, err = scanJobs(ctx, tx, dequeueFallbackSQL, req.Queue, req.Limit, 0)
		if err != nil {
			return nil, fmt.Errorf("出队（兜底路径）失败: %w", err)
		}
	}
	if len(jobs) == 0 {
		return nil, tx.Commit(ctx)
	}

	ids := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	// updated_at 不在 SET 里：00022 给这张表挂了 touch_jobs_updated_at，
	// 任何一次 UPDATE 都会把它推到 now()。§12 那段 SQL 里写了它，
	// 在这个库上写与不写等价，而少写一列就少一处将来与触发器打架的地方。
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		   SET status = 1, attempts = attempts + 1,
		       locked_by = $1, locked_at = now()
		 WHERE id = ANY($2)`, req.WorkerID, ids); err != nil {
		return nil, fmt.Errorf("占位失败: %w", err)
	}
	for i := range jobs {
		jobs[i].Attempts++ // 上面那条 UPDATE 刚加过，回传的是执行时的真实值
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return jobs, nil
}

// dequeueFairSQL 是 §12 的第一条出队语句。
//
// busy 那个集合很小 —— 行数上限就是 worker 数 ——  所以 NOT IN 的代价可以忽略，
// 而 merchant_id 在 idx_jobs_dequeue 的 INCLUDE 里，过滤不回表。
//
// $3 永远是一个正数：不设上限那件事由调用方改走兜底那条语句表达，
// 理由见 DequeueJobs 里那段注释（HAVING count(*) >= 0 会把全部在途租户装进 busy）。
const dequeueFairSQL = `
WITH busy AS (
    SELECT merchant_id
      FROM jobs
     WHERE queue = $1 AND status = 1
     GROUP BY merchant_id
    HAVING count(*) >= $3
)
SELECT id, merchant_id, queue, job_key, payload, attempts
  FROM jobs
 WHERE queue = $1 AND status = 0 AND run_after <= now()
   AND merchant_id NOT IN (SELECT merchant_id FROM busy)
 ORDER BY priority DESC, id
 LIMIT $2
 FOR UPDATE SKIP LOCKED`

// dequeueFallbackSQL 是 §12 的第二条：去掉租户限制。
const dequeueFallbackSQL = `
SELECT id, merchant_id, queue, job_key, payload, attempts
  FROM jobs
 WHERE queue = $1 AND status = 0 AND run_after <= now()
 ORDER BY priority DESC, id
 LIMIT $2
 FOR UPDATE SKIP LOCKED`

func scanJobs(ctx context.Context, tx pgx.Tx, sql, queue string, limit, perTenant int) ([]Job, error) {
	args := []any{queue, limit}
	if perTenant > 0 {
		args = append(args, perTenant)
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.MerchantID, &j.Queue, &j.JobKey, &j.Payload, &j.Attempts); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FinishJobs 把一批任务标成成功。
//
// 不直接 DELETE：§12 说成功的任务保留 7 天便于排查，由清理任务删除
// （PurgeFinishedJobs）。保留期里它们不在 uk_jobs_pending 的部分索引范围内，
// 所以同一个商品可以被反复重新加工。
func (r *Repo) FinishJobs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE jobs SET status = 2, locked_by = NULL, locked_at = NULL, last_error = NULL
		 WHERE id = ANY($1)`, ids)
	return err
}

// ErrJobDeadLettered：这一次失败把任务推过了 max_attempts，它进死信了。
//
// 做成 sentinel 而不是一个 bool：死信是要有人看的（§7「超限进死信队列并告警」），
// 而一个被忽略的返回值与「没有这回事」长得一模一样。
var ErrJobDeadLettered = errors.New("任务重试次数用尽，已转死信")

// RetryJob 按指数退避把任务放回队列；次数用尽则转死信并返回 ErrJobDeadLettered。
//
// 退避公式逐字照抄 §12：now() + 1 秒 × 2^attempts。attempts 在占位时就加过了，
// 所以第一次失败退避 2 秒、第二次 4 秒，第五次之后转死信。
func (r *Repo) RetryJob(ctx context.Context, id int64, reason string) error {
	var status int16
	err := r.pool.QueryRow(ctx, `
		UPDATE jobs
		   SET status     = CASE WHEN attempts >= max_attempts THEN 3 ELSE 0 END,
		       run_after  = now() + (interval '1 second' * pow(2, attempts)),
		       last_error = $2,
		       locked_by  = NULL, locked_at = NULL
		 WHERE id = $1
		RETURNING status`, id, reason).Scan(&status)
	if err != nil {
		return err
	}
	if status == jobStatusDead {
		return fmt.Errorf("job %d: %w", id, ErrJobDeadLettered)
	}
	return nil
}

// RetryJobCapped 与 RetryJob 相同，只是退避封顶在 maxBackoff：1 秒 × 2^attempts，最多 maxBackoff。
//
// 为库存的 outbox 任务而加（微服务拆分阶段 1b）：那些任务配着很大的 max_attempts
// （EnqueueJobWithMaxAttempts），不封顶的话第十几次重试之后一次退避就是几个小时，
// 库存服务早就回来了、而放回库存还要再等半天 —— 那半天全是少卖。
func (r *Repo) RetryJobCapped(ctx context.Context, id int64, reason string, maxBackoff time.Duration) error {
	var status int16
	err := r.pool.QueryRow(ctx, `
		UPDATE jobs
		   SET status     = CASE WHEN attempts >= max_attempts THEN 3 ELSE 0 END,
		       run_after  = now() + (interval '1 second' * LEAST(pow(2, attempts), $3::float8)),
		       last_error = $2,
		       locked_by  = NULL, locked_at = NULL
		 WHERE id = $1
		RETURNING status`, id, reason, maxBackoff.Seconds()).Scan(&status)
	if err != nil {
		return err
	}
	if status == jobStatusDead {
		return fmt.Errorf("job %d: %w", id, ErrJobDeadLettered)
	}
	return nil
}

// ClaimJobByKey 按 (租户, 队列, job_key) 占下一条**还没被 worker 取走**（status = 0）的任务：
// 与 DequeueJobs 同一个占位（status = 1、attempts + 1、记下 locked_by），只是点名要哪一条。
// 没有这条（从没入过队、已经被 worker 取走、已经做完）返回 ok = false。
//
// 为 outbox 的「提交之后就地跑一次」而加（service/inventory_outbox.go）：业务事务提交之后，
// 调用方先自己把那件事做了（关单之后立刻放回库存），占下再做、做完照常 FinishJobs / RetryJobCapped
// —— 与 worker 走完全相同的状态机，只是不等下一次轮询。占不到就说明 worker 已经在做了，
// 调用方什么都不用管；那件事本身按单号幂等，即使两边都做了也只生效一次。
//
// 带 merchant_id：jobs 没有 RLS（这个文件的文件头），这里是本仓库的 Go 代码而不是 db/queries，
// 不受「查询里不许出现租户列」那条规矩管；而 job_key 只在租户内唯一（uk_jobs_pending）。
// run_after 不看：退避中的任务同样可以被点名拿来重试（调用方就是那个「现在就再试一次」的理由）。
func (r *Repo) ClaimJobByKey(ctx context.Context, merchantID int64, queue, jobKey, workerID string) (Job, bool, error) {
	var j Job
	err := r.pool.QueryRow(ctx, `
		UPDATE jobs
		   SET status = 1, attempts = attempts + 1, locked_by = $4, locked_at = now()
		 WHERE merchant_id = $1 AND queue = $2 AND job_key = $3 AND status = 0
		   AND attempts < max_attempts
		RETURNING id, merchant_id, queue, job_key, payload, attempts`,
		merchantID, queue, jobKey, workerID).
		Scan(&j.ID, &j.MerchantID, &j.Queue, &j.JobKey, &j.Payload, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// ReapStuckJobs 把卡在「执行中」超过 olderThan 的任务放回队列。
//
// ===========================================================================
// 没有它，每租户在途上限会变成一条永久的封锁
// ===========================================================================
//
// 上限是按 `status = 1` 数出来的。一个 worker 进程被 kill -9（或者机器掉电）
// 之后，它手上那几条任务永远停在 1 —— 于是那家商户的在途数永远等于上限，
// 公平路径永远跳过他。他的商品从此一件也不再被加工，而且不报任何错：
// 队列里有任务、worker 在跑、日志干净，只有那家店的搜索索引停在某个时刻。
//
// 这不是一个理论风险：滚动发布本身就是「把进程杀掉再起一个」。
//
// ### status 的三目表达式与 RetryJob 一字不差，而那是必须的
//
// 见文件头第三节：jobs 上有 CHECK (attempts <= max_attempts)，而占位是
// attempts + 1。把一条 attempts 已经等于 max_attempts 的任务无脑放回 0，
// 下一次占位就违反约束 —— 而占位跑在出队那个事务里，一条坏任务会让**整批**
// 出队失败，队列从此不动。
//
// attempts 不在这里减回去：它记的是「占位过几次」，被 kill 掉的那一次是真的
// 占位过。不减的后果是反复崩溃的任务会在 max_attempts 次之后进死信 ——
// 那正是想要的，一个每次都能把 worker 拖死的任务不该被无限重试。
func (r *Repo) ReapStuckJobs(ctx context.Context, queue string, olderThan time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs
		   SET status     = CASE WHEN attempts >= max_attempts THEN 3 ELSE 0 END,
		       locked_by  = NULL, locked_at = NULL,
		       last_error = 'worker 没有在预期时间内交回这条任务（进程退出或卡死），已回收'
		 WHERE queue = $1 AND status = 1
		   AND locked_at < now() - $2::interval`,
		queue, olderThan.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PurgeFinishedJobs 删掉 retain 之前完成的成功任务，一次最多 limit 条。
//
// §12：「成功的任务保留 7 天便于排查，之后由清理任务删除；死信永久保留。」
// 死信不在这条语句的范围里，那是刻意的 —— 它们是要有人看的东西。
//
// ### 它今天走的是过滤扫描，而这一点要写下来
//
// 00022 只建了 §12 给的三个索引，没有一个覆盖 `status = 2`。所以这条 DELETE
// 的子查询要扫过出队索引够不到的那部分行。在一张靠 autovacuum（scale_factor
// 0.01）压着、常驻规模在几千行量级的队列表上，这是可以接受的；
// 真到了需要第四个索引的那天，那是一次要同时改数据模型 §12 的动作，
// 不夹带在这一轮。LIMIT 在这里的作用就是给那一天留出余地：
// 清理永远是一次有界的操作，不会某一次突然锁住半张表。
func (r *Repo) PurgeFinishedJobs(ctx context.Context, queue string, retain time.Duration, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM jobs
		 WHERE id IN (SELECT id FROM jobs
		               WHERE queue = $1 AND status = 2
		                 AND updated_at < now() - $2::interval
		               LIMIT $3)`,
		queue, retain.String(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
