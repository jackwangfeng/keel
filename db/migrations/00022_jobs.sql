-- 任务队列就是一张表（数据模型 §12「jobs」）。M4 商品理解服务骨架，阶段 1。
--
-- README 明确宣称「不需要 Redis、不需要 RabbitMQ，任务队列是一张表」。这份迁移
-- 兑现它。DDL、三个索引与两条出队语句逐条照抄 §12，**没有自己发明的东西**；
-- 与文档的偏离只有一处（merchant_id 的 DEFAULT），写在下面第二节，
-- 数据模型 §12 已随本轮一起改。
--
-- ===========================================================================
-- 一、这张表**没有 RLS**，而这是一个需要解释的决定
-- ===========================================================================
--
-- 库里此前只有三类表不挂租户策略：merchants / shop_settings（tenant-root，
-- 解析期就要读，此时 current_merchant() 还会 RAISE）、状态机那两张
-- （shared-reference，没有租户维度）、以及 barrier 与 goose 的版本表
-- （cross-tenant-infra，压根没有 merchant_id 可挂）。
--
-- jobs 不属于上面任何一类：**它有 merchant_id，而且那一列有真实语义**
-- （公平调度按它分组，uk_jobs_pending 按它收口）。它不挂策略的理由是第四条，
-- 独立于前三条：
--
--   **出队是跨租户的公平调度，而租户策略会让它不可能。** §12 那两条出队语句
--   要在**一条**查询里看见全部租户的待办：第一条要按 merchant_id 分组数出
--   在途已达上限的租户再排除掉，第二条（兜底）要在去掉租户限制之后按
--   `ORDER BY priority DESC, id` 取全局最靠前的几条。挂上
--   `merchant_id = current_merchant()` 之后，`busy` 那个 CTE 永远只看得见
--   一家店，`NOT IN` 恒为真，兜底那一趟和第一趟变成同一条查询 ——
--   公平调度不是「打了折扣」，是整个机制蒸发了，而且不报任何错。
--
--   更直接的一条：worker 跑在任何 HTTP 请求之外，它那个事务里**没有**
--   app.merchant_id。策略谓词会调 current_merchant()，而那个函数在未设置时
--   是 RAISE（00002）—— 挂上策略的结果不是「查不到」，是每一次出队都 42501。
--
-- ### 它与 repository/sweep.go 拒绝掉的那条路**不是同一件事**
--
-- sweep.go 的文件头写着：§12 说「worker 出队以平台身份执行，不走 RLS 注入」，
-- 而「本仓库没有那个身份，也不打算有」—— 指的是**一个能绕过 RLS 的数据库角色**。
-- 那条路被拒绝的理由很硬：db.NewPool 的自检会当场拒绝一个 rolsuper /
-- rolbypassrls 的角色启动（internal/db/pool_test.go），因为应用进程握着能绕过
-- RLS 的连接比任何一次越权读取都更难发现。
--
-- 这份迁移走的是另一条：**不给任何人绕过 RLS 的能力，而是让这一张表不处在
-- RLS 的管辖之下**。差别是可枚举的 —— 一个 BYPASSRLS 角色对**全部 36 张表**
-- 都失效，而这里失效的范围恰好是 jobs 一张表，其余每一张照旧。
-- keel_app 仍然是 NOSUPERUSER NOBYPASSRLS，pool 的自检一个字都不用改。
--
-- ### 那么谁来挡「A 商家读到 B 商家的任务」
--
-- 诚实地列出来，三道，一道比一道弱：
--
--   ① **入队写不出别人的租户。** merchant_id 的默认值是 current_merchant()，
--      而入队那条 SQL（db/queries/jobs.sql 的 EnqueueJob）里根本没有
--      merchant_id 这个词 —— 生成的 Go 函数签名里也没有那个参数，
--      「往 B 店名下入一条队」连编译都编不出来。没有租户上下文时入队不是
--      写进一行猜出来的 merchant_id，是当场 42501（current_merchant() 的 RAISE）。
--      这一条由 internal/db/jobs_test.go 的
--      TestEnqueueOutsideATenantTransactionFails 钉住。
--   ② **读的入口只有一个。** 跨租户读写只发生在 internal/repository/jobs.go
--      那几条手写 SQL 里，它们跑在 pool 上、不在任何租户事务里，与
--      ActiveMerchants 同一个惯例（repository/sweep.go 文件头）。
--      db/queries 下**没有**任何一条 SELECT jobs 的查询，也就不存在
--      「某条业务查询顺手读了别人的任务」这种形状。
--   ③ **payload 里不放业务数据。** 它只放一个 product_id（见下面 payload 那一列
--      的注释）。真要发生一次越界读，读到的是一个整数，不是商品文案或金额。
--
-- ①③ 是结构性的，②靠的是纪律 —— 写清楚，不假装它和 RLS 等价。
-- 代价接受得起的原因是这张表的内容：它是调度元数据，不是业务数据。
--
-- ===========================================================================
-- 二、与文档 DDL 的一处偏离：merchant_id 有了 DEFAULT current_merchant()
-- ===========================================================================
--
-- §12 原本写的是 `merchant_id BIGINT NOT NULL REFERENCES merchants(id)`。
-- 照抄的话入队语句里就必须出现 merchant_id 这个词，而
-- scripts/check_query_tenancy.py 不许 db/queries 里出现它。
--
-- 这是 00010 / 00012 / 00013 / 00014 / 00016 用过的同一条路子，第六次，
-- **数据模型 §12 的 DDL 已随本轮一起改**，两边保持一份真相。
--
-- 在这张表上它还多一层意思：这张表没有 RLS 的 WITH CHECK 兜底（第一节），
-- 所以「入队时的 merchant_id 只能是当前租户」这件事**全靠这个默认值**。
-- 把它删掉不会有任何断言变红，除非那条断言是冲着这件事来的 ——
-- 所以 internal/db/jobs_test.go 里有一条专门盯它。
--
-- ===========================================================================
-- 三、三个索引刻意不以 merchant_id 打头（§12 的唯一一组例外）
-- ===========================================================================
--
-- 数据模型 §2 把「业务索引一律以 merchant_id 开头」列为规矩一与 RLS 的推论，
-- 并点名三类例外，jobs 的出队索引是其中一类。理由在 §12 里，抄要点：
-- 出队是跨租户的，`ORDER BY priority DESC, id` 必须由索引直接给出顺序，
-- 把 merchant_id 提到首列会把这个顺序碎成每租户一段。所以 merchant_id
-- 放在 INCLUDE 里（过滤不回表）或作为第二列。
--
-- 那条推论的**前提**在这条路径上也不成立：它说的是「RLS 会给每条业务查询注入
-- merchant_id = ?」，而这张表没有 RLS。
--
-- **唯一键 uk_jobs_pending 不在例外之列**，它照规矩三收进了租户内
-- （首列就是 merchant_id），所以 db/tenancy.json 的 unique_global_ok 里
-- 没有它的条目 —— 不需要豁免的东西不该出现在豁免清单里。
--
-- ===========================================================================
-- 四、autovacuum：§12 说「必须」，所以它在 DDL 里，不在某个人的记忆里
-- ===========================================================================
--
-- §12 末尾：「先撞上的往往不是吞吐，而是表膨胀 —— 队列表是高频 UPDATE +
-- DELETE 的表，死元组堆积会让出队索引扫描越来越慢，必须单独给它调 autovacuum
-- 阈值」。默认的 scale_factor 是 0.2，也就是要等死元组堆到表的 20% 才清理；
-- 一张常驻几百行、每天翻几十万次的队列表，那个阈值等于永远不触发相对及时的清理，
-- 而症状是出队变慢，没有任何东西报错。
--
-- 写进迁移而不是写进运维手册：手册里的一句话在一次新部署里等于不存在。

-- +goose Up

CREATE TABLE jobs (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    queue        TEXT        NOT NULL,            -- 'product.understanding' / 'vector.text' ...
    job_key      TEXT        NOT NULL,            -- 业务幂等键，如 'product:12345'
    -- 只放定位业务对象所需的最小标识（本轮是 {"product_id": 123}）。
    -- 不放商品文案、不放金额：这张表没有 RLS（第一节），payload 越薄，
    -- 万一发生一次越界读，读到的东西越没有价值。而且任务执行时本来就要回读
    -- 那一行商品 —— 入队时的快照到执行时已经可能过期，存它反而是第二个真相源。
    payload      JSONB       NOT NULL DEFAULT '{}',
    priority     SMALLINT    NOT NULL DEFAULT 0,  -- 大的先出队，**仅在租户内有意义**
    status       SMALLINT    NOT NULL DEFAULT 0,  -- 0待执行 1执行中 2已成功 3死信
    attempts     INT         NOT NULL DEFAULT 0,
    max_attempts INT         NOT NULL DEFAULT 5,
    run_after    TIMESTAMPTZ NOT NULL DEFAULT now(),  -- 退避后的可执行时间
    locked_by    TEXT,                            -- worker 标识，排查卡死用
    locked_at    TIMESTAMPTZ,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_jobs_attempts CHECK (attempts >= 0 AND attempts <= max_attempts)
);

-- 同一个任务在未完成时不会重复入队。**条件里必须包含 1 执行中**，否则任务
-- 刚被取走就能再入一条（§12）。成功与死信的行不在索引里，所以同一个商品
-- 可以被反复重新加工 —— 重试中的任务回到 0 而不是一个新状态，也是为了
-- 让这个索引持续有效。
CREATE UNIQUE INDEX uk_jobs_pending
    ON jobs(merchant_id, queue, job_key) WHERE status IN (0, 1);

-- 出队扫描。带上 merchant_id 是为了让公平调度的过滤在索引上完成，不回表。
--
-- 为什么 run_after 不在前面：绝大多数待执行任务的 run_after 早已到期，
-- 处在退避中的是少数。按 (queue, priority DESC, id) 组织让索引直接给出出队顺序，
-- run_after 作为过滤条件回表判断，比让索引先按时间排、再在内存里重排 priority
-- 划算。退避任务占比很高的那天，这个结论要重新测（§12）。
CREATE INDEX idx_jobs_dequeue ON jobs(queue, priority DESC, id)
    INCLUDE (merchant_id) WHERE status = 0;

-- 统计各租户在途数，集合很小（上限是 worker 数 × 每租户上限）。
-- 每租户在途上限就是靠它算出来的，而那条上限是多租户下队列成立的前提
-- （商品理解服务设计 §8：一个新商家导入一次商品目录，就能让所有其他店铺的
-- 搜索索引停止更新几小时）。
CREATE INDEX idx_jobs_inflight ON jobs(queue, merchant_id) WHERE status = 1;

CREATE INDEX idx_jobs_dead ON jobs(queue, updated_at DESC) WHERE status = 3;

-- **刻意不建租户策略**，理由见文件头第一节。
-- ENABLE ROW LEVEL SECURITY 也不加：不加策略地 ENABLE 等于拒绝一切，
-- 而加一条策略就把出队做没了。db/tenancy.json 里 jobs 是 cross-tenant-queue 类，
-- internal/db/migrate_test.go 的 TestTenantPoliciesArePresentAndExact 会核对
-- 「这张表上一条策略都没有」—— 哪天有人顺手给它加一条，那条测试当场红。

CREATE OR REPLACE TRIGGER touch_jobs_updated_at
    BEFORE UPDATE ON jobs FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 表膨胀的对策，理由见文件头第四节。0.01 = 死元组到表的 1% 就清理。
ALTER TABLE jobs SET (
    autovacuum_vacuum_scale_factor  = 0.01,
    autovacuum_analyze_scale_factor = 0.01
);

-- worker 要 SELECT（出队扫描）、UPDATE（占位、退避、转死信）、
-- DELETE（清理七天前的成功任务），入队要 INSERT。四权都要，收不窄。
-- 这一点写在 db/tenancy.json 的 jobs 条目里，与这里是同一份账。
GRANT SELECT, INSERT, UPDATE, DELETE ON jobs TO keel_app;

-- +goose Down

REVOKE ALL ON jobs FROM keel_app;
DROP TABLE jobs;
