-- 支付单（数据模型 §5 payments）。Task 7 支付回调的前置表。
--
-- 00006 建订单域时就在注释里提过它（「供 order_items / shipments / payments /
-- refunds 做复合外键」），但那一轮没有落点：orders 刚建出来，支付回调还没有。
-- 现在有了。**不改 00006 一个字**，整张表落在这份新迁移里。
--
-- ===========================================================================
-- 一、与文档 DDL 的一处偏离：merchant_id 有了 DEFAULT current_merchant()
-- ===========================================================================
--
-- §5 的 DDL 是 `merchant_id BIGINT NOT NULL REFERENCES merchants(id)`，没有默认值。
-- 照抄的话，支付回调那条 INSERT 里就必须出现 merchant_id 这个词，而
-- scripts/check_query_tenancy.py 不许 db/queries 里出现它（理由见那个脚本的
-- 文件头：应用层再过滤一遍租户，「RLS 到底有没有生效」就变得测不出来了）。
--
-- 这是 00010 / 00012 / 00013 用过的同一条路子，第四次。**数据模型 §5 的 DDL
-- 已随本轮一起改**，两边保持一份真相。
--
-- 默认值同时是一道形状约束：生成的 Go 函数签名里根本没有 merchant_id，
-- 「拿 A 店的上下文往 B 店名下记一笔支付」连编译都编不出来。RLS 的 WITH CHECK
-- 仍是第二道。
--
-- ===========================================================================
-- 二、uk_payments_channel_txn 是支付回调幂等的**唯一**一道锁
-- ===========================================================================
--
-- 三列 (merchant_id, channel, channel_txn_id)，部分唯一（channel_txn_id 非空）。
-- 契约与架构 §5 的散文里写的是「(channel, channel_txn_id) 唯一索引」——那是简写，
-- 数据模型 §5 已经把它收进租户内，理由写在那里：channel_txn_id 是渠道生成的，
-- 渠道只保证在单个商户号维度内不重复；全局唯一等于替渠道做了一个它没做的承诺，
-- 而承诺落空的后果是**一笔真实到账被静默当成重复丢弃**。
--
-- ### 它决定了回调那条 INSERT 的形状：不许写 ON CONFLICT
--
-- 直觉写法是 `INSERT ... ON CONFLICT DO NOTHING`，撞了就当重复回调。那是错的，
-- 而且错得没有声音：不带冲突目标的 DO NOTHING 会把**任何**唯一冲突吞掉，
-- 包括 payment_no 撞车——而 payment_no 是我们自己生成的 72 bit 随机编号，
-- 它撞车意味着熵源坏了或者生成逻辑被改坏了，是一个必须炸出来的 bug。
-- 带冲突目标又写不出来：目标里要写 merchant_id，撞 check_query_tenancy。
--
-- 所以回调那条 INSERT 是一条**光秃秃的 INSERT**，由 Go 侧按
-- pgErr.ConstraintName == 'uk_payments_channel_txn' 把「重复回调」这一种冲突
-- 挑出来（repository.ErrDuplicateChannelTxn），其余 23505 原样上浮。
-- 索引名因此是接口的一部分，改名要同时改 Go 侧那个常量。
--
-- ===========================================================================
-- 三、payment_no 全局唯一，登记早就在册
-- ===========================================================================
--
-- db/tenancy.json 的 unique_global_ok 里 `payments(payment_no)` 已经写着
-- 「① 我们自己生成的不可枚举编号」——那份清单刻意是前瞻的，表没建就先登记好。
-- 本轮不需要往那个文件里加任何一行：payments 自带 merchant_id、策略是列比较，
-- 就是默认的 tenant 类。

-- +goose Up

CREATE TABLE payments (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    payment_no     TEXT        NOT NULL UNIQUE,
    order_id       BIGINT      NOT NULL,
    channel        SMALLINT    NOT NULL,            -- 1微信 2支付宝 3余额
    amount_cents   BIGINT      NOT NULL,
    status         SMALLINT    NOT NULL DEFAULT 0,  -- 0待支付 1成功 2失败 3已关闭
    channel_txn_id TEXT,                            -- 渠道流水号，用于对账
    notify_payload JSONB,                           -- 原始回调报文，永久保留
    paid_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 供 refunds 做复合外键
    UNIQUE (id, merchant_id),
    FOREIGN KEY (order_id, merchant_id) REFERENCES orders(id, merchant_id)
);

CREATE UNIQUE INDEX uk_payments_channel_txn
    ON payments(merchant_id, channel, channel_txn_id) WHERE channel_txn_id IS NOT NULL;

-- 「这一单有过哪些支付尝试」。对账与二次支付排查都从这里进。
CREATE INDEX idx_payments_order ON payments(merchant_id, order_id);

-- 行级安全。ENABLE 之外必须再加 FORCE（00002 与数据模型 §2「坑一」）。
ALTER TABLE payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON payments USING (merchant_id = current_merchant());

-- 00007 那个 DO 循环只在它自己那一次迁移里跑过，之后新建的表要自己挂 ——
-- TestUpdatedAtIsMaintainedByTrigger 会盯着。
CREATE OR REPLACE TRIGGER touch_payments_updated_at
    BEFORE UPDATE ON payments FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- tenant 类的默认 GRANT 面（db/tenancy.json），TestAppRoleGrantSurface 逐表比对。
-- UPDATE/DELETE 今天没有调用点（支付单一旦落库就是账，只增不改），仍然给：
-- GRANT 面按**类别**申明，不按今天的调用面裁剪（理由同 00011 里 user_addresses 那段）。
GRANT SELECT, INSERT, UPDATE, DELETE ON payments TO keel_app;

-- +goose Down
REVOKE ALL ON payments FROM keel_app;
DROP POLICY tenant ON payments;
DROP TABLE payments;
