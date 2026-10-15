-- 券的精准发放（带对照组），docs/superpowers/specs/2026-10-16-ai-ops-onboarding-restock-coupon-design.md 第三节。
-- 人群怎么划在 service/customer_segments.go（纯函数，按这里取回的逐人统计判）；这里只取数与记名单。
--
-- 与这个目录里别的文件一样，一个 merchant_id 都没有：租户由 RLS 过滤。
-- 注释里不许有反引号，理由见 db/queries/inventories.sql 的第三条说明。

-- name: CustomerStats :many
-- 本店每个在用买家的下单统计：已付款单数、近 90 天付款金额、最后一次付款时间、最后一次加购时间。
-- 已付款的口径与成绩单一致：状态 20 / 30 / 40 / 50。
SELECT u.id AS user_id,
       count(o.id)::bigint AS paid_orders,
       COALESCE(sum(o.paid_cents) FILTER (WHERE o.paid_at >= sqlc.arg(now_at)::timestamptz - interval '90 days'), 0)::bigint AS paid_90d_cents,
       max(o.paid_at)::timestamptz AS last_paid_at,
       (SELECT max(ci.updated_at) FROM carts c JOIN cart_items ci ON ci.cart_id = c.id WHERE c.user_id = u.id)::timestamptz AS last_cart_at
  FROM users u
  LEFT JOIN orders o ON o.user_id = u.id AND o.status IN (20, 30, 40, 50) AND o.paid_at IS NOT NULL
 WHERE u.deleted_at IS NULL AND u.status = 1
 GROUP BY u.id
 ORDER BY u.id;

-- name: InsertCouponTargetMember :exec
INSERT INTO coupon_target_members (proposal_id, user_id, treated, user_coupon_id)
VALUES (sqlc.arg(proposal_id), sqlc.arg(user_id), sqlc.arg(treated), sqlc.narg(user_coupon_id));

-- name: CouponTargetGroups :many
-- 复盘：发券组与对照组各自的人数、窗口内下过已付款订单的人数、这些人在窗口内的付款合计。
SELECT m.treated,
       count(*)::bigint AS members,
       count(*) FILTER (WHERE x.paid_cents IS NOT NULL)::bigint AS converted,
       COALESCE(sum(x.paid_cents), 0)::bigint AS paid_cents
  FROM coupon_target_members m
  LEFT JOIN LATERAL (
        SELECT sum(o.paid_cents) AS paid_cents
          FROM orders o
         WHERE o.user_id = m.user_id AND o.status IN (20, 30, 40, 50)
           AND o.paid_at >= sqlc.arg(from_at) AND o.paid_at < sqlc.arg(to_at)
        HAVING count(*) > 0) x ON true
 WHERE m.proposal_id = sqlc.arg(proposal_id)
 GROUP BY m.treated
 ORDER BY m.treated;
