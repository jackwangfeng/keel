-- 校验 00323 以 NOT VALID 加的 chk_refund_payer（与 00323 分开，理由同 00322）。
-- 存量退款单全是自营的（payment_id、user_id 都非空），校验必过。
-- +goose Up
ALTER TABLE refunds VALIDATE CONSTRAINT chk_refund_payer;

-- +goose Down
SELECT 1;
