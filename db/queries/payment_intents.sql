-- 支付意图（00150，service/payment_intent.go）。一个 merchant_id 都没有：租户由 RLS 过滤。

-- name: FindActivePaymentIntent :one
-- 这一单此刻有效的那个支付意图（至多一行：uk_payment_intents_active）。调用方已锁住订单行。
SELECT id, order_id, channel, channel_txn_id, amount_cents, status, created_at
  FROM payment_intents
 WHERE order_id = $1 AND status = 1;

-- name: InsertPaymentIntent :exec
INSERT INTO payment_intents (order_id, channel, channel_txn_id, amount_cents)
VALUES ($1, $2, $3, $4);

-- name: SupersedePaymentIntent :exec
-- 换渠道：旧的作废（真实渠道要先调它的关单接口，沙箱不需要）。
UPDATE payment_intents SET status = 2, closed_at = now() WHERE id = $1 AND status = 1;

-- name: MarkPaymentIntentSettled :exec
-- 回调入账时认领的那一个：已入账。流水号不是我们发的（渠道侧直接推来的）时一行都不改，那也对。
UPDATE payment_intents SET status = 3, closed_at = now()
 WHERE channel = $1 AND channel_txn_id = $2 AND status IN (1, 2);
