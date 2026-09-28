-- 多收款：少发生 + 兜住（2026-09-28 破坏性测试）。
--
-- 之前发起支付（POST /orders/{no}/payments）只看订单是不是待支付，每次都造一个新的渠道流水号、库里不留痕，
-- 于是同一单可以拿到任意多套都能付的支付参数（先点微信再点支付宝、并发点两次）；第二笔到账时订单已不是
-- 待支付，支付单落一行 status = 1 留痕、订单不认，回调回 200 —— 钱收了，只有一条 Error 日志，
-- 不退、后台也看不到。取消 / 超时关单之后才到的回调、金额不符的到账是同一个口子。
--
-- ① payment_intents：一单同一时刻至多一个有效的支付意图（uk_payment_intents_active）。再次发起支付：
--    同渠道复用同一个流水号（参数一模一样），换渠道先把旧的作废（真实渠道要调关单接口）再发新的。
--    回调入账时认领的那一个记成已入账（3）。
-- ② payment_returns：订单不认的每一笔到账（重复支付 / 订单已取消或关闭 / 金额与应付不符）一张退回单，
--    原路退回（沙箱当场模拟渠道退款；真实渠道等回调，走 /webhooks/refunds/{channel}，按单号前缀 PR 分派）。
--    与售后退款（refunds）分开：它不是买家的售后，不动订单的退款维度、不回补库存、不进售后统计。
-- +goose Up
CREATE TABLE payment_intents (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    order_id       BIGINT      NOT NULL,
    channel        SMALLINT    NOT NULL,            -- 同 payments.channel：1 微信 2 支付宝
    channel_txn_id TEXT        NOT NULL,            -- 发给渠道的流水号；入账后即 payments.channel_txn_id
    amount_cents   BIGINT      NOT NULL,
    status         SMALLINT    NOT NULL DEFAULT 1,  -- 1 有效 / 2 已作废（换了渠道）/ 3 已入账
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at      TIMESTAMPTZ,
    CONSTRAINT chk_payment_intent_status CHECK (status IN (1, 2, 3)),
    CONSTRAINT chk_payment_intent_amount CHECK (amount_cents > 0),
    FOREIGN KEY (order_id, merchant_id) REFERENCES orders(id, merchant_id)
);
CREATE UNIQUE INDEX uk_payment_intents_txn ON payment_intents(merchant_id, channel, channel_txn_id);
CREATE UNIQUE INDEX uk_payment_intents_active ON payment_intents(merchant_id, order_id) WHERE status = 1;

ALTER TABLE payment_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_intents FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON payment_intents USING (merchant_id = current_merchant());
GRANT SELECT, INSERT, UPDATE, DELETE ON payment_intents TO keel_app;

CREATE TABLE payment_returns (
    id                BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id       BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    return_no         TEXT        NOT NULL,            -- PR 开头，与售后退款单号区分（渠道回调按它分派）
    payment_id        BIGINT      NOT NULL,
    order_id          BIGINT      NOT NULL,
    channel           SMALLINT    NOT NULL,
    amount_cents      BIGINT      NOT NULL,            -- 退回的就是那一笔到账的全额
    reason            SMALLINT    NOT NULL,            -- 1 重复支付 / 2 订单已取消或关闭 / 3 金额与应付不符
    status            SMALLINT    NOT NULL DEFAULT 10, -- 10 待提交 / 30 退回中（等渠道回调）/ 40 已退回
    channel_refund_id TEXT,
    notify_payload    JSONB,
    attempts          INT         NOT NULL DEFAULT 0,
    last_error        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    returned_at       TIMESTAMPTZ,
    CONSTRAINT chk_payment_return_amount CHECK (amount_cents > 0),
    CONSTRAINT chk_payment_return_reason CHECK (reason IN (1, 2, 3)),
    CONSTRAINT chk_payment_return_status CHECK (status IN (10, 30, 40)),
    CONSTRAINT chk_payment_return_done CHECK ((status = 40) = (returned_at IS NOT NULL)),
    CONSTRAINT uk_payment_returns_no UNIQUE (merchant_id, return_no),
    CONSTRAINT uk_payment_returns_payment UNIQUE (merchant_id, payment_id),  -- 一笔到账至多退一次
    FOREIGN KEY (payment_id, merchant_id) REFERENCES payments(id, merchant_id),
    FOREIGN KEY (order_id, merchant_id) REFERENCES orders(id, merchant_id)
);
CREATE UNIQUE INDEX uk_payment_returns_channel_refund ON payment_returns(merchant_id, channel, channel_refund_id)
    WHERE channel_refund_id IS NOT NULL;
CREATE INDEX idx_payment_returns_pending ON payment_returns(merchant_id, status) WHERE status <> 40;
CREATE INDEX idx_payment_returns_order ON payment_returns(merchant_id, order_id);

ALTER TABLE payment_returns ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_returns FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON payment_returns USING (merchant_id = current_merchant());
CREATE OR REPLACE TRIGGER touch_payment_returns_updated_at
    BEFORE UPDATE ON payment_returns FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON payment_returns TO keel_app;

-- +goose Down
REVOKE ALL ON payment_returns FROM keel_app;
DROP TABLE payment_returns;
REVOKE ALL ON payment_intents FROM keel_app;
DROP TABLE payment_intents;
