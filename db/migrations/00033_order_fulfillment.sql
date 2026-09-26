-- 订单后半程（一）：发货包裹表 shipments，以及订单状态机的数据库兜底。
-- DDL 照抄数据模型设计 §5，那里是唯一真相源。
--
-- ===========================================================================
-- 一、order_status_transitions 从今天起**真的被执行**
-- ===========================================================================
--
-- 00006 就建了这张表，§5 称它为「状态机的唯一真相源」，而在这份迁移之前
-- 库里没有任何东西读它 —— 每一条改 orders.status 的语句都靠自己的
-- `WHERE status = ?` 守边，那是**代码**。一条写错了谓词（或者干脆没写）的
-- UPDATE 可以把一笔已关闭的订单推成已发货，数据库一声不吭。
--
-- 发货、确认收货、取消、整单退款这几条边一次性要落地，是把「表里写着的规则」
-- 变成「数据库执行的规则」的时机：BEFORE UPDATE OF status 的触发器逐行核对
-- (OLD.status, NEW.status) 在不在表里，不在就以 23514 拒绝，约束名
-- order_status_transition（repository 按这个名字把它翻成一个 sentinel）。
--
-- 服务层的条件更新仍然是第一道：它们带着预期的起点状态，失配时影响 0 行，
-- 调用方据此回契约里那几个 409。触发器是第二道，管的是「有人写了一条不带
-- 起点状态的 UPDATE」—— 两道的失效方式不一样，所以两道都要有。
--
-- 为什么用触发器而不是 CHECK：CHECK 看不到 OLD。为什么不怕 §11 那句
-- 「触发器里做聚合会放大锁竞争」：这里是一次主键点查一张 9 行的静态表，
-- 不是聚合。
--
-- 函数不是 SECURITY DEFINER：keel_app 对 order_status_transitions 本来就有
-- SELECT（shared-reference 类，00006），以调用者身份读得到；给它 DEFINER
-- 反而是让一个触发器函数带着属主权限跑，而它根本不需要。
--
-- ===========================================================================
-- 二、状态与时间戳钉在一起
-- ===========================================================================
--
-- chk_fulfillment_timestamps：已支付及之后的状态必须有 paid_at，已发货 / 已完成
-- 必须有 shipped_at，已完成必须有 finished_at。与 user_coupons 的
-- chk_user_coupon_state 同一个思路：状态与「这件事什么时候发生」是同一件事的
-- 两种表示，让它们互相校验。它挡的是「发货推了状态却忘了写 shipped_at」——
-- 那会让「发货后 N 天自动确认收货」的倒计时没有起点。
--
-- 存量数据：30 / 40 在这份迁移之前不可达（没有发货接口）；20 / 50 / 60 只能
-- 经支付回调的 SettleOrder 进入，它总是写 paid_at。所以 ADD CONSTRAINT 的
-- 全表校验在任何一个真实库上都应当通过；若不通过，说明有人绕过应用改过数据，
-- 那正是该停下来看的时候。
--
-- ===========================================================================
-- 三、shipments 与文档的一处增补
-- ===========================================================================
--
-- merchant_id 带 DEFAULT current_merchant()：db/queries 里不许出现 merchant_id
-- 这个词（scripts/check_query_tenancy.py），与 orders / payments 同一条路子。
-- 文档 §5 已同步。created_by 刻意保持单列外键，理由在 §5 与 db/tenancy.json
-- 的 fk_single_column_ok。

-- +goose Up

CREATE TABLE shipments (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id   BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    order_id      BIGINT      NOT NULL,
    carrier_code  TEXT        NOT NULL,   -- 'sf' / 'jd' / 'yto' 等
    tracking_no   TEXT        NOT NULL,
    status        SMALLINT    NOT NULL DEFAULT 1,  -- 1已发出 2已签收 3异常
    shipped_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at  TIMESTAMPTZ,
    created_by    BIGINT      REFERENCES staff(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_shipment_status CHECK (status IN (1, 2, 3)),
    CONSTRAINT chk_shipment_text CHECK (carrier_code <> '' AND tracking_no <> ''),
    FOREIGN KEY (order_id, merchant_id) REFERENCES orders(id, merchant_id)
);
CREATE INDEX idx_shipments_order ON shipments(merchant_id, order_id);
-- 同一承运商的运单号在**本店内**不会重复；重复提交同一个单号是录入错误，不是新包裹。
-- 索引名是接口的一部分：repository 按它把这一种 23505 挑成 ErrTrackingNoDuplicated。
CREATE UNIQUE INDEX uk_shipments_tracking
    ON shipments(merchant_id, carrier_code, tracking_no);

ALTER TABLE shipments ENABLE ROW LEVEL SECURITY;
ALTER TABLE shipments FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON shipments USING (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_shipments_updated_at
    BEFORE UPDATE ON shipments FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON shipments TO keel_app;

-- 订单状态机的执行者（见文件头第一节）。
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION guard_order_status_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM order_status_transitions
                    WHERE from_status = OLD.status AND to_status = NEW.status) THEN
        RAISE EXCEPTION '订单 % 的状态不允许从 % 变为 %（order_status_transitions 里没有这条边）',
                OLD.order_no, OLD.status, NEW.status
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'order_status_transition',
                  TABLE = 'orders';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER guard_orders_status_transition
    BEFORE UPDATE OF status ON orders
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION guard_order_status_transition();

ALTER TABLE orders ADD CONSTRAINT chk_fulfillment_timestamps CHECK (
    (status NOT IN (20, 30, 40, 50, 60) OR paid_at IS NOT NULL)
    AND (status NOT IN (30, 40) OR shipped_at IS NOT NULL)
    AND (status <> 40 OR finished_at IS NOT NULL)
);

-- +goose Down
ALTER TABLE orders DROP CONSTRAINT chk_fulfillment_timestamps;
DROP TRIGGER guard_orders_status_transition ON orders;
DROP FUNCTION guard_order_status_transition();
REVOKE ALL ON shipments FROM keel_app;
DROP POLICY tenant ON shipments;
DROP TABLE shipments;
