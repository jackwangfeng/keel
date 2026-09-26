-- 退货寄回物流（契约 POST /refunds/{refund_no}/return-shipment）。
-- DDL 照抄数据模型设计 §11，那里是唯一真相源。
--
-- 退货退款审核通过后退款单停在 20 待买家退货，买家把货寄回、填承运商与运单号，
-- 商家据此查件、收货后确认（20 → 30）。此前库里没有地方放这两样，
-- 商家只能在聊天记录里找单号。
--
-- ===========================================================================
-- 为什么是 refunds 上的三列，而不是一张 return_shipments 表
-- ===========================================================================
--
-- §5 把发货包裹单独成表，理由是「将来多包裹只是多插几行」。退货这边没有那个前提：
-- 一张退款单就是一次寄回，买家填错了是**改**，不是再寄一个包裹；将来真有「分批寄回」，
-- 那是把一张退款单拆成几张，而不是一张单挂几个包裹。为一个恒为一对一的关系建表，
-- 换来的是每一条读退款单的查询都多一次 JOIN。
--
-- 列名带 return_ 前缀：refunds 上将来若有「退款的渠道流水」一类字段，
-- 裸的 carrier_code 读起来分不清是谁的物流。
--
-- return_submitted_at 是买家**最近一次填写**的时间，不是揽收时间（那要查物流，
-- 一期不做，与发货同一个取舍）。
--
-- ===========================================================================
-- chk_refund_return_shipment
-- ===========================================================================
--
-- 三列同生同灭（要么都有、要么都没有）；有的话两个文本非空（NOT NULL 拦不住空串，
-- 与 chk_shipment_text 同一个理由），而且只能出现在退货退款（refund_type = 2）上 ——
-- 仅退款的单挂着一个寄回单号，是一条说不通的数据。
--
-- **不把它与 status 钉在一起**：填写只在 20 发生，但填过之后单子会走到 30 / 40，
-- 也可能被撤回到 60，那时寄回物流仍然是真的（货确实寄出过），不能要求它消失。
-- 「只有 20 能填」由写它的那条 UPDATE（SubmitReturnShipment）的谓词负责。

-- +goose Up

ALTER TABLE refunds ADD COLUMN return_carrier_code TEXT;
ALTER TABLE refunds ADD COLUMN return_tracking_no  TEXT;
ALTER TABLE refunds ADD COLUMN return_submitted_at TIMESTAMPTZ;

ALTER TABLE refunds ADD CONSTRAINT chk_refund_return_shipment CHECK (
    (return_carrier_code IS NULL) = (return_tracking_no IS NULL)
    AND (return_carrier_code IS NULL) = (return_submitted_at IS NULL)
    AND (return_carrier_code IS NULL
         OR (refund_type = 2 AND return_carrier_code <> '' AND return_tracking_no <> ''))
);

-- +goose Down
ALTER TABLE refunds DROP CONSTRAINT chk_refund_return_shipment;
ALTER TABLE refunds DROP COLUMN return_submitted_at;
ALTER TABLE refunds DROP COLUMN return_tracking_no;
ALTER TABLE refunds DROP COLUMN return_carrier_code;
