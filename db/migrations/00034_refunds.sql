-- 订单后半程（二）：退款与售后。refunds / refund_items / refund_status_transitions。
-- DDL 照抄数据模型设计 §11，那里是唯一真相源。
--
-- ===========================================================================
-- 与 §11 原稿相比，这一份补了什么（文档已同步改）
-- ===========================================================================
--
-- ① merchant_id 带 DEFAULT current_merchant()（两张表都是）。
--    db/queries 里不许出现 merchant_id 这个词（scripts/check_query_tenancy.py），
--    与 orders / payments / shipments 同一条路子：生成的 Go 函数签名里没有这个
--    参数，调用方没有那个参数可以传错。
--
-- ② 退款单的状态与它的时间戳 / 流水号 / 驳回理由钉在一起（chk_refund_state）：
--      40 已退款   ⇔ channel_refund_id 与 refunded_at 都有值
--      50 已拒绝   ⇒ reject_reason 有值（契约：驳回必须带理由）
--      20/30/40/50 ⇒ audited_at 有值（这四个状态都只能经审核到达）
--    与 user_coupons 的 chk_user_coupon_state、orders 的 chk_fulfillment_timestamps
--    同一个思路。第一条尤其要紧：channel_refund_id 是退款回调幂等的最后一道锁，
--    一笔「已退款」却没有渠道流水号的退款单，意味着那道锁对它不存在。
--
-- ③ refund_type / reason_code 的取值由 CHECK 钉住（契约的两个枚举）。
--
-- ④ 退款状态机与订单状态机同样由数据库执行：BEFORE UPDATE OF status 的触发器
--    逐行核对 refund_status_transitions，不在表里的边以 23514 拒绝，约束名
--    refund_status_transition。理由与 00033 文件头第一节一字不差。
--
-- ===========================================================================
-- 「在途超退」为什么仍然不在数据库里
-- ===========================================================================
--
-- §11 写得很清楚：order_items 的 chk_item_refund 只拦「已退 + 本次 > 总数」，
-- 拦不住「已退 + 在途 + 本次 > 总数」。在途量不持久化（reserved_qty 被删掉的
-- 同一个理由），这道校验只能在业务层做 —— 申请退款时在**同一个事务**里、
-- 在订单行锁之下（SELECT ... FOR UPDATE）复算。这份迁移不改变这一点，
-- 只是把它写在这里，免得有人以为这几张表已经拦住了。

-- +goose Up

-- ---------------------------------------------------------------------------
-- 退款单状态机（§11）。shared-reference 类：全租户共用，keel_app 只读。
-- ---------------------------------------------------------------------------
CREATE TABLE refund_status_transitions (
    from_status SMALLINT NOT NULL,
    to_status   SMALLINT NOT NULL,
    PRIMARY KEY (from_status, to_status)
);
INSERT INTO refund_status_transitions VALUES
 (10,20),(10,30),(10,50),(10,60),(20,30),(20,60),(30,40);
GRANT SELECT ON refund_status_transitions TO keel_app;

-- ---------------------------------------------------------------------------
-- refunds（§11）
-- ---------------------------------------------------------------------------
CREATE TABLE refunds (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id        BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    refund_no          TEXT        NOT NULL UNIQUE,   -- 对外编号，不可枚举
    order_id           BIGINT      NOT NULL,
    payment_id         BIGINT      NOT NULL,          -- 原路退回：退哪一笔
    user_id            BIGINT      NOT NULL,
    refund_type        SMALLINT    NOT NULL,   -- 1仅退款 2退货退款
    reason_code        SMALLINT    NOT NULL,   -- 1不想要了 2少发漏发 3商品损坏 4描述不符 5其他
    reason_text        TEXT,
    evidence_urls      TEXT[]      NOT NULL DEFAULT '{}',
    goods_amount_cents BIGINT      NOT NULL DEFAULT 0,  -- 退的货款，已按 §7 分摊倒算
    freight_cents      BIGINT      NOT NULL DEFAULT 0,  -- 退的运费
    amount_cents       BIGINT      NOT NULL,            -- 实退总额
    status             SMALLINT    NOT NULL DEFAULT 10,
    channel            SMALLINT    NOT NULL,            -- 冗余自 payments，对账用
    channel_refund_id  TEXT,                            -- 渠道退款流水号
    notify_payload     JSONB,                           -- 原始回调报文，永久保留
    reject_reason      TEXT,
    audited_at         TIMESTAMPTZ,
    refunded_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_refund_amount CHECK (
        amount_cents = goods_amount_cents + freight_cents
        AND goods_amount_cents >= 0 AND freight_cents >= 0 AND amount_cents > 0
    ),
    CONSTRAINT chk_refund_enums CHECK (
        refund_type IN (1, 2) AND reason_code BETWEEN 1 AND 5
        AND status IN (10, 20, 30, 40, 50, 60)
    ),
    CONSTRAINT chk_refund_state CHECK (
        ((status = 40) = (channel_refund_id IS NOT NULL AND refunded_at IS NOT NULL))
        AND (status <> 50 OR reject_reason IS NOT NULL)
        AND (status NOT IN (20, 30, 40, 50) OR audited_at IS NOT NULL)
    ),
    -- 供 refund_items 做复合外键
    UNIQUE (id, merchant_id),
    FOREIGN KEY (order_id, merchant_id)   REFERENCES orders(id, merchant_id),
    FOREIGN KEY (payment_id, merchant_id) REFERENCES payments(id, merchant_id),
    FOREIGN KEY (user_id, merchant_id)    REFERENCES users(id, merchant_id)
);
-- 退款回调幂等的最后一道锁，与 uk_payments_channel_txn 完全同构。
-- 索引名是接口的一部分：repository 按它把重复回调挑成 ErrDuplicateChannelRefund。
CREATE UNIQUE INDEX uk_refunds_channel_txn
    ON refunds(merchant_id, channel, channel_refund_id)
    WHERE channel_refund_id IS NOT NULL;
CREATE INDEX idx_refunds_order ON refunds(merchant_id, order_id);
CREATE INDEX idx_refunds_user ON refunds(merchant_id, user_id, created_at DESC);
CREATE INDEX idx_refunds_pending ON refunds(merchant_id, status, updated_at)
    WHERE status IN (10, 30);   -- 待审核 / 退款中，给工单与重试任务用

-- ---------------------------------------------------------------------------
-- refund_items（§11）
-- ---------------------------------------------------------------------------
CREATE TABLE refund_items (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id   BIGINT NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    refund_id     BIGINT NOT NULL,
    order_item_id BIGINT NOT NULL,
    quantity      INT    NOT NULL CHECK (quantity > 0),
    amount_cents  BIGINT NOT NULL CHECK (amount_cents >= 0), -- 该行实退，已扣分摊优惠
    FOREIGN KEY (refund_id, merchant_id)
        REFERENCES refunds(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (order_item_id, merchant_id)
        REFERENCES order_items(id, merchant_id)
);
CREATE UNIQUE INDEX uk_refund_items ON refund_items(refund_id, order_item_id);
CREATE INDEX idx_refund_items_order_item ON refund_items(merchant_id, order_item_id);

-- ---------------------------------------------------------------------------
-- 行级安全、updated_at、GRANT 面
-- ---------------------------------------------------------------------------
ALTER TABLE refunds ENABLE ROW LEVEL SECURITY;
ALTER TABLE refunds FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON refunds USING (merchant_id = current_merchant());

ALTER TABLE refund_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE refund_items FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON refund_items USING (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_refunds_updated_at
    BEFORE UPDATE ON refunds FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON refunds, refund_items TO keel_app;

-- ---------------------------------------------------------------------------
-- 退款状态机的执行者（见文件头 ④）
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION guard_refund_status_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM refund_status_transitions
                    WHERE from_status = OLD.status AND to_status = NEW.status) THEN
        RAISE EXCEPTION '退款单 % 的状态不允许从 % 变为 %（refund_status_transitions 里没有这条边）',
                OLD.refund_no, OLD.status, NEW.status
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'refund_status_transition',
                  TABLE = 'refunds';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER guard_refunds_status_transition
    BEFORE UPDATE OF status ON refunds
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION guard_refund_status_transition();

-- +goose Down
DROP TRIGGER guard_refunds_status_transition ON refunds;
DROP FUNCTION guard_refund_status_transition();
REVOKE ALL ON refunds, refund_items, refund_status_transitions FROM keel_app;
DROP POLICY tenant ON refund_items;
DROP POLICY tenant ON refunds;
DROP TABLE refund_items;
DROP TABLE refunds;
DROP TABLE refund_status_transitions;
