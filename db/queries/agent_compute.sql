-- AI 经营 M10 计算工具（docs/AI经营-M10M11设计.md §2）：promotion_review 用到的、
-- 别处没有的两条聚合查询。slow_movers 复用 restock.sql 的 RestockCandidates / StoreSKUSales，
-- 这里不重复。
--
-- 与这个目录里别的文件一样，一个 merchant_id 都没有：租户由 RLS 过滤。
-- 注释里不许有反引号，理由见 db/queries/inventories.sql 的第三条说明。

-- name: PromotionSkuOrderStats :one
-- 一个窗口内、含给定 SKU 的已支付订单：单量、这些订单的销售额（整单实收，不是分摊到这几行的那一份 ——
-- 买家为了凑活动商品很可能一并买了别的）、这些 SKU 本身卖出的件数。
-- 给 promotion_review 复盘限时折扣 / 秒杀（promo_type 3/4，有 promotion_skus 报价行）用。
--
-- 已支付的判据与 reports.sql 逐字一致（paid_at 落在窗口、status 属于已支付的五种）。
-- sku_ids 为空时 sqlc.arg 的 ANY 恒假，三个数字都会是 0，调用方按「没有参与 SKU」处理，不必先判空再决定要不要跑这条查询。
WITH win AS (
    SELECT o.id, o.paid_cents
      FROM orders o
     WHERE o.paid_at IS NOT NULL
       AND o.paid_at >= sqlc.arg(window_start)::timestamptz
       AND o.paid_at <  sqlc.arg(window_end)::timestamptz
       AND o.status IN (20, 30, 40, 50, 60)
), lines AS (
    SELECT oi.order_id, oi.quantity
      FROM win
      JOIN order_items oi ON oi.order_id = win.id
     WHERE oi.sku_id = ANY(sqlc.arg(sku_ids)::bigint[])
)
SELECT count(DISTINCT lines.order_id)::bigint AS order_count,
       COALESCE((SELECT sum(w.paid_cents) FROM win w
                  WHERE w.id IN (SELECT DISTINCT order_id FROM lines)), 0)::bigint AS paid_cents,
       COALESCE(sum(lines.quantity), 0)::bigint AS units_sold
  FROM lines;

-- name: CouponTemplateOrderStats :one
-- 一张券模板带来的已支付订单：单量、销售额（整单实收），以及这些订单里由这张券让出的优惠。
--
-- 一单的优惠只有活动与券两个来源（数据模型 §7·二 chk_discount_sources：discount_cents =
-- promotion_discount_cents，当且仅当没挂券），所以「这张券让出的优惠」= discount_cents 减去
-- promotion_discount_cents 的那一份，包邮券抵的运费也算在其中（discount_cents 本就含它）。
-- 给 promotion_review 复盘一张券模板用。
SELECT count(*)::bigint AS order_count,
       COALESCE(sum(o.paid_cents), 0)::bigint AS paid_cents,
       COALESCE(sum(o.discount_cents - o.promotion_discount_cents), 0)::bigint AS coupon_discount_cents
  FROM orders o
  JOIN user_coupons uc ON uc.id = o.user_coupon_id
 WHERE uc.template_id = sqlc.arg(template_id)
   AND o.paid_at IS NOT NULL
   AND o.status IN (20, 30, 40, 50, 60);
