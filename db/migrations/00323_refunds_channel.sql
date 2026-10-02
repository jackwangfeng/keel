-- 渠道单的退款（第三期 Task 5）：平台上取消 / 退款之后，keel 订单上记一张**已成功**的退款单。
--
-- 渠道单没有 keel 买家（orders.user_id 空，00320），也没有 payments 行（钱在平台上收、平台上退），
-- 所以 refunds.user_id / payment_id 放开可空：两者同空同有（chk_refund_payer）；同空的只能是渠道退款
-- （channel = 10、直接落在 40 已退款，不经审核与支付渠道）。外键是 MATCH SIMPLE，为空时不检查。
-- 幂等靠已有的 uk_refunds_channel_txn（merchant_id, channel, channel_refund_id）：
-- channel_refund_id = channel_refund:<binding>:<平台退款 ID>（整单取消是 …:<外部单号>:cancel）。
-- 买家侧的查询都带 user_id = 我，渠道退款自然查不到。
-- +goose Up
ALTER TABLE refunds ALTER COLUMN payment_id DROP NOT NULL;
ALTER TABLE refunds ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE refunds ADD CONSTRAINT chk_refund_payer CHECK (
    (payment_id IS NULL) = (user_id IS NULL)
    AND (payment_id IS NOT NULL OR (channel = 10 AND status = 40))
) NOT VALID;

-- +goose Down
DELETE FROM refund_items WHERE refund_id IN (SELECT id FROM refunds WHERE payment_id IS NULL);
DELETE FROM refunds WHERE payment_id IS NULL;
ALTER TABLE refunds DROP CONSTRAINT chk_refund_payer;
ALTER TABLE refunds ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE refunds ALTER COLUMN payment_id SET NOT NULL;
