-- 订单上的运费（数据模型 §5 orders、§7「运费怎么算」「包邮券」）：
-- orders.freight_discount_cents / orders.freight_snapshot，以及包邮券（coupon_type = 4）解禁。
-- DDL 照抄数据模型设计 §5 / §7，那里是唯一真相源。
--
-- ===========================================================================
-- 一、金额恒等式一个字不改
-- ===========================================================================
--
-- chk_amount 仍是 payable = goods + freight - discount。包邮券抵掉的运费算进
-- discount_cents（「优惠」本来就是订单上所有优惠的合计），另记一列
-- freight_discount_cents 说清楚其中多少是抵运费的：
--
--   · freight_cents          运费（抵扣之前），下单那一刻按模板算出来的
--   · freight_discount_cents 其中被包邮券抵掉的部分
--   · 实收运费 = freight_cents - freight_discount_cents —— 售后退运费的上限按它算
--
-- 于是 SUM(order_items.discount_cents) = discount_cents - freight_discount_cents：
-- 包邮券不分摊到行（它抵的不是任何一件商品的钱），行上的实付净额与退款公式
-- （refund_calc.go）不受影响。chk_freight_discount 把两条边界钉住：抵扣不超过运费
-- （运费最多抵到 0），也不超过优惠合计（于是 chk_discount_needs_coupon 顺带保证
-- 抵运费的一定挂着一张券）。
--
-- 不改 chk_amount、不另加一项进恒等式，是刻意的：恒等式是报表、对账、退款三处
-- 共同的锚，给它加一项等于让三处一起改；而「优惠合计」这个口径本来就该包含抵运费。
--
-- ===========================================================================
-- 二、freight_snapshot：下单那一刻用的是哪条规则
-- ===========================================================================
--
-- 与 store_snapshot、coupon_name 同一个理由：模板随时会改，三个月前那一单的运费
-- 要能回答「这 8 块钱是怎么来的」。快照是契约 FreightBreakdown 的形状（按模板分组、
-- 命中的规则、包邮原因），只给展示与对账用，不参与任何计算。
-- 可空：00042 之前的订单没有算过运费（那时 freight_cents 恒为落账值 0），
-- 给它们编一份「没有模板、不计运费」的快照是在伪造历史。
--
-- ===========================================================================
-- 三、包邮券
-- ===========================================================================
--
-- 00026 让 chk_coupon_rule 没有 coupon_type = 4 那一支（「没有运费，包邮券永远减 0」）。
-- 运费落地之后放开：包邮券不带减免额与折扣率，threshold_cents 是门槛（比适用商品小计），
-- max_discount_cents 是最多抵多少运费（0 = 全免）。顶层的 threshold_cents >= 0 AND
-- max_discount_cents >= 0 已经在，这一支只钉「不许带别的券型的字段」。

-- +goose Up

ALTER TABLE orders ADD COLUMN freight_discount_cents BIGINT NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN freight_snapshot JSONB;
ALTER TABLE orders ADD CONSTRAINT chk_freight_discount CHECK (
    freight_cents >= 0 AND freight_discount_cents >= 0
    AND freight_discount_cents <= freight_cents
    AND freight_discount_cents <= discount_cents
);

ALTER TABLE coupon_templates DROP CONSTRAINT chk_coupon_rule;
ALTER TABLE coupon_templates ADD CONSTRAINT chk_coupon_rule CHECK (
    threshold_cents >= 0 AND max_discount_cents >= 0 AND (
    (coupon_type = 1 AND discount_cents > 0 AND threshold_cents >= discount_cents
                     AND discount_rate = 0 AND max_discount_cents = 0) OR
    (coupon_type = 2 AND discount_rate BETWEEN 1 AND 999 AND discount_cents = 0) OR
    (coupon_type = 3 AND discount_cents > 0 AND threshold_cents = 0
                     AND discount_rate = 0 AND max_discount_cents = 0) OR
    (coupon_type = 4 AND discount_cents = 0 AND discount_rate = 0)
    )
);

-- +goose Down
-- 回滚前库里若已有包邮券模板，重建旧约束会失败 —— 那是对的：回滚到「没有运费」的
-- 版本，那些券就成了永远减 0 的券，要先由人决定怎么处置它们。
ALTER TABLE coupon_templates DROP CONSTRAINT chk_coupon_rule;
ALTER TABLE coupon_templates ADD CONSTRAINT chk_coupon_rule CHECK (
    threshold_cents >= 0 AND max_discount_cents >= 0 AND (
    (coupon_type = 1 AND discount_cents > 0 AND threshold_cents >= discount_cents
                     AND discount_rate = 0 AND max_discount_cents = 0) OR
    (coupon_type = 2 AND discount_rate BETWEEN 1 AND 999 AND discount_cents = 0) OR
    (coupon_type = 3 AND discount_cents > 0 AND threshold_cents = 0
                     AND discount_rate = 0 AND max_discount_cents = 0)
    )
);
ALTER TABLE orders DROP CONSTRAINT chk_freight_discount;
ALTER TABLE orders DROP COLUMN freight_snapshot;
ALTER TABLE orders DROP COLUMN freight_discount_cents;
