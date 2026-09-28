-- 多收款退回（00150，service/payment_return.go）。一个 merchant_id 都没有：租户由 RLS 过滤。

-- name: InsertPaymentReturn :execrows
-- 给一笔订单不认的到账开退回单。同一笔支付至多一张（uk_payment_returns_payment）：已有就不再开（0 行）。
INSERT INTO payment_returns (return_no, payment_id, order_id, channel, amount_cents, reason)
SELECT sqlc.arg(return_no), p.id, p.order_id, p.channel, p.amount_cents, sqlc.arg(reason)
  FROM payments p
 WHERE p.id = sqlc.arg(payment_id)
ON CONFLICT ON CONSTRAINT uk_payment_returns_payment DO NOTHING;

-- name: ListUnacceptedPayments :many
-- 兜底扫描：渠道成功（status = 1）、但订单不认、也还没开退回单的到账。
-- 「订单认的那一笔」与售后原路退回同一个判据（refunds.sql 的 FindSettledPayment）：金额与到账时间对得上
-- orders.paid_cents / paid_at。订单还在待支付（paid_at 为空）时，任何到账都是不认的（只可能是金额不符）。
-- 返回订单状态，调用方据此定原因。
SELECT p.id AS payment_id, o.status AS order_status, (o.paid_at IS NOT NULL)::boolean AS order_paid,
       p.amount_cents, o.payable_cents
  FROM payments p
  JOIN orders o ON o.id = p.order_id
 WHERE p.status = 1
   AND NOT (o.paid_at IS NOT NULL AND p.amount_cents = o.paid_cents AND p.paid_at = o.paid_at)
   AND NOT EXISTS (SELECT 1 FROM payment_returns r WHERE r.payment_id = p.id)
 ORDER BY p.id
 LIMIT sqlc.arg(row_limit);

-- name: ListPaymentReturnsToSubmit :many
-- 待提交（10）的退回单，最早的在前。
SELECT id, return_no FROM payment_returns WHERE status = 10 ORDER BY id LIMIT sqlc.arg(row_limit);

-- name: LockPaymentReturnByNo :one
SELECT r.id, r.return_no, r.payment_id, r.order_id, r.channel, r.amount_cents, r.reason, r.status,
       r.channel_refund_id, r.attempts, r.last_error, r.created_at, r.updated_at, r.returned_at,
       o.order_no, p.channel_txn_id AS payment_txn_id
  FROM payment_returns r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
 WHERE r.return_no = $1
   FOR UPDATE OF r;

-- name: MarkPaymentReturnSubmitted :exec
-- 交给了渠道、等回调（真实渠道）。
UPDATE payment_returns SET status = 30, attempts = attempts + 1, last_error = NULL WHERE id = $1 AND status = 10;

-- name: MarkPaymentReturnAttemptFailed :exec
-- 提交失败（渠道报错 / 没配密钥），留在 10 等下一轮重试，记下原因给后台看。
UPDATE payment_returns SET attempts = attempts + 1, last_error = sqlc.arg(last_error) WHERE id = sqlc.arg(id) AND status = 10;

-- name: SettlePaymentReturn :execrows
-- 渠道回调：退回成功。10（沙箱当场）或 30 → 40。
UPDATE payment_returns
   SET status = 40, returned_at = now(), channel_refund_id = sqlc.arg(channel_refund_id),
       notify_payload = sqlc.arg(notify_payload)
 WHERE id = sqlc.arg(id) AND status IN (10, 30);

-- name: ListPaymentReturns :many
-- 后台「多收款退回」列表：新的在前，可按状态筛。
SELECT r.id, r.return_no, r.payment_id, r.order_id, r.channel, r.amount_cents, r.reason, r.status,
       r.channel_refund_id, r.attempts, r.last_error, r.created_at, r.updated_at, r.returned_at,
       o.order_no, p.channel_txn_id AS payment_txn_id
  FROM payment_returns r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
 WHERE (sqlc.narg(status)::smallint IS NULL OR r.status = sqlc.narg(status)::smallint)
 ORDER BY r.created_at DESC, r.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountPaymentReturns :one
SELECT count(*) FROM payment_returns r
 WHERE (sqlc.narg(status)::smallint IS NULL OR r.status = sqlc.narg(status)::smallint);

-- name: ListPaymentReturnsForOrder :many
-- 买家订单详情上的「多付的钱已退回」。
SELECT r.id, r.return_no, r.payment_id, r.order_id, r.channel, r.amount_cents, r.reason, r.status,
       r.channel_refund_id, r.attempts, r.last_error, r.created_at, r.updated_at, r.returned_at,
       o.order_no, p.channel_txn_id AS payment_txn_id
  FROM payment_returns r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
 WHERE r.order_id = $1
 ORDER BY r.id;
