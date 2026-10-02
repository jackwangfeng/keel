-- 任务队列（数据模型 §12）的**入队**那一面，也是这张表在 sqlc 里唯一的一面。
--
-- ===========================================================================
-- 为什么这个文件里只有入队
-- ===========================================================================
--
-- 出队、占位、退避、转死信、清理，全部是**跨租户**的语句（数据模型 §12，
-- 以及 db/migrations/00022_jobs.sql 的文件头第一节）。它们：
--
--   · 跑在 pool 上、不在任何租户事务里 —— sqlc 的产物挂在 tenantTx 上，
--     而那个类型的每一个方法都跑在一个设好 app.merchant_id 的事务里，
--     这几条查询按定义不属于那一面。惯例与 repository.ActiveMerchants 完全一致
--     （理由写在 internal/repository/sweep.go 的文件头）。
--   · 语句里**必须**出现 merchant_id（公平调度就是按它分组的），而
--     scripts/check_query_tenancy.py 不许 db/queries 里出现这个词。
--     那条检查挡的是「应用层再过滤一遍租户，于是 RLS 有没有生效变得测不出来」，
--     而这张表根本没有 RLS —— 把这几条塞进 db/queries 再往 ALLOW 里加一条豁免，
--     等于在那份清单里放一条与它要防的东西无关的条目，把清单变成噪音。
--
-- 所以它们是 internal/repository/jobs.go 里的手写 SQL，逐条对着 §12 抄。
--
-- 入队则相反：它发生在一个**有租户上下文**的事务里（触发点扫描是一家一家
-- 商户走的），merchant_id 由列默认值 current_merchant() 填，语句里一个字都
-- 不用提它。所以入队留在 sqlc 这一侧，而且它是这张表上唯一一条能被
-- 「生成的函数签名里没有 merchant_id 这个参数」这道形状约束保护的语句。

-- name: EnqueueJob :execrows
-- 入队。返回 1 表示真的入了一条，0 表示同一个任务已经在队列里（未完成）。
--
-- ### ON CONFLICT 为什么不写冲突目标
--
-- 冲突目标写出来就是「(merchant_id, queue, job_key) WHERE status IN (0,1)」，
-- 那一行里有 merchant_id —— check_query_tenancy 的判据是「非注释行里出现这个词」，
-- 它不区分「应用层过滤」与「唯一索引的列名」。
--
-- 不带目标的 ON CONFLICT DO NOTHING 语义更宽（撞上**任何**唯一约束都静默跳过），
-- 而这张表上只有两个：主键 id 是 GENERATED ALWAYS AS IDENTITY，撞不了；
-- 剩下的就是 uk_jobs_pending。所以在这张表上两种写法等价。
--
-- barrier 那条 INSERT 走的是同一个写法，理由不同但结论一样（db/tenancy.json
-- 的 barrier 条目：带冲突目标要额外的 SELECT 权，而它的 GRANT 面只有 INSERT）。
--
-- ### 返回 0 是正常路径，不是错误
--
-- 触发点扫描每 30 秒跑一轮，而一件商品的加工可能要跨好几轮。第二轮再扫到它时
-- 队列里那条还在（status 0 或 1），这里返回 0 —— **这正是 uk_jobs_pending
-- 存在的全部意义**：不靠调用方记得「我上轮已经入过了」，靠索引。
-- 调用方把它记成 Deduped 而不是失败（internal/service/index.go 的 IndexReport）。
INSERT INTO jobs (queue, job_key, payload, priority)
VALUES (@queue, @job_key, @payload, @priority)
ON CONFLICT DO NOTHING;

-- name: EnqueueJobWithMaxAttempts :execrows
-- 与 EnqueueJob 同一条入队（同一个 ON CONFLICT、同一个默认租户），只多一个 max_attempts。
--
-- 为库存的 outbox 任务而加（微服务拆分阶段 1b，service/inventory_outbox.go）：关单释放与退款回补
-- 是「库存服务不在就一直等它回来」的任务，默认的 5 次（约一分钟）远不够一次库存服务的故障；
-- 进了死信就是永久少卖。调用方给的次数配合 RetryJobCapped 的封顶退避，覆盖的是小时级的故障。
INSERT INTO jobs (queue, job_key, payload, priority, max_attempts)
VALUES (@queue, @job_key, @payload, @priority, @max_attempts)
ON CONFLICT DO NOTHING;

-- name: HasUnfinishedJobWithPrefix :one
-- 这家商家这个队列里有没有 job_key 以 prefix 开头、还没做完（待跑或在跑）的任务。
-- 为渠道「手动重新同步商品」而加：整店拉取一页一个任务、键各不相同（pull:<binding>:<游标>），
-- uk_jobs_pending 只挡得住同一个键，挡不住「第 3 页还在跑时又从第 1 页排一条链」。
-- jobs 没有 RLS（文件头），租户靠 current_merchant() —— 与入队那两条的列默认值同一个来源。
SELECT EXISTS (
    SELECT 1 FROM jobs
     WHERE merchant_id = current_merchant() AND queue = @queue::text
       AND status IN (0, 1) AND starts_with(job_key, @prefix::text)
)::boolean AS busy;
