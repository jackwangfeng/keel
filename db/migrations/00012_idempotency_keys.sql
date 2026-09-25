-- 接口幂等键（数据模型 §12）。
--
-- ### 为什么它是 Task 5 的隐藏前置
--
-- 契约的 `POST /orders` 写着「**必须携带 Idempotency-Key**」，三种处置
-- （重放 / 409 处理中 / 422 键被复用）都要服务端状态，而这张表一直不存在。
-- 没有它，`Idempotency-Key` 只能被读出来然后丢掉 —— 用户连点两下就是两笔订单，
-- 而契约声明的那三种响应一条也给不出来。
--
-- ### 与文档 DDL 的一处偏离：merchant_id 有了 DEFAULT current_merchant()
--
-- §12 的 DDL 是 `merchant_id BIGINT NOT NULL REFERENCES merchants(id)`，没有默认值。
-- 照抄的话，抢占插入那条语句里就必须出现 `merchant_id` 这个词，而
-- scripts/check_query_tenancy.py 不许 db/queries 里出现它（理由见那个脚本的
-- 文件头：应用层再过滤一遍租户，「RLS 到底有没有生效」就变得测不出来了）。
--
-- 值得注意的是 §12 自己那段示意 SQL 也没写 merchant_id：
--
--     INSERT INTO idempotency_keys (scope, user_id, idem_key, request_hash, expire_at)
--     VALUES ($1, $2, $3, $4, now() + interval '24 hours')
--     ON CONFLICT (scope, user_id, idem_key) DO NOTHING;
--
-- 那条语句配上 `NOT NULL` 且无默认值的列是写不进去的。所以这不是「实现偷懒」，
-- 是文档里两段互相矛盾的东西，本轮按 00010 给 user_tokens 用过的同一条路子
-- 收口：把默认值补上，让租户只能来自事务里那句 set_config('app.merchant_id')。
-- **数据模型 §12 的 DDL 已随本轮一起改。**
--
-- 默认值同时是一道形状约束：调用方**没有那个参数可以传错** —— 生成的 Go 函数
-- 签名里根本没有 merchant_id。RLS 的 WITH CHECK 仍是第二道，真有人写了别家的
-- merchant_id，策略会当场拒绝。
--
-- ### 抢占插入的 ON CONFLICT 这里**要**写冲突目标，和 barrier 相反
--
-- 00008 的 barrier 上 keel_app 只有 INSERT，所以那条语句不许带冲突目标
-- （带了要额外的 SELECT 权，会当场 42501）。这张表是 tenant 类，四权齐全，
-- 而且必须带冲突目标：不带的话，`expire_at` 那类无关约束的冲突也会被吞掉，
-- 而抢占插入唯一想沉默跳过的是主键撞车。
--
-- ### db/tenancy.json 不需要为它加条目
--
-- 它自带 merchant_id、策略是列比较，就是默认的 tenant 类。要登记的两条
-- 早就在册（那份清单刻意是**前瞻**的）：
--   · fk_missing_ok 里的 idempotency_keys.user_id（纯基础设施表，随 expire_at
--     过期即删，挂外键会让清理任务和用户注销互相牵制）；
--   · unique_global_ok 里的 idempotency_keys(scope,user_id,idem_key)
--     （user_id 已蕴含租户；scope 打头是为了让抢占插入是一次点查）。

-- +goose Up

CREATE TABLE idempotency_keys (
    scope         TEXT        NOT NULL,            -- 接口标识，如 'orders.create'
    merchant_id   BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    user_id       BIGINT      NOT NULL,
    idem_key      TEXT        NOT NULL,            -- 客户端传来的 Idempotency-Key
    request_hash  TEXT        NOT NULL,            -- sha256(规范化后的请求体)
    status        SMALLINT    NOT NULL DEFAULT 0,  -- 0处理中 1成功 2失败
    response_code INT,
    response_body JSONB,
    expire_at     TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, user_id, idem_key)
);

-- 清理任务是按租户入队的 jobs，走 RLS，注入的 merchant_id = ... 需要它打头（§12）。
CREATE INDEX idx_idem_expire ON idempotency_keys(merchant_id, expire_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON idempotency_keys USING (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_idempotency_keys_updated_at
    BEFORE UPDATE ON idempotency_keys FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- tenant 类的默认 GRANT 面。DELETE 今天没有调用点（过期行的清理属于 §12 说的
-- 定时任务），仍然给：GRANT 面按类别申明，不按今天的调用面裁剪。
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO keel_app;

-- +goose Down
REVOKE ALL ON idempotency_keys FROM keel_app;
DROP POLICY tenant ON idempotency_keys;
DROP TABLE idempotency_keys;
