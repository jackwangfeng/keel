-- 售后期（2026-09-28 破坏性测试遗留）：之前没有售后时效，订单完成 400 天后还能申请退货退款。
-- 已完成（40）的订单在完成时间 + after_sale_days 天之后不能再申请售后（409 after-sale-window-closed）；
-- 还没完成的（已支付、已发货）不受它限制 —— 那时候本来就在售后期里，自动确认收货会把它推到 40。
-- 默认 15 天（国内电商常见的「确认收货后 15 天内可申请售后」），与另外两个天数同一个 1 到 365 的边界。
-- +goose Up
ALTER TABLE shop_preferences ADD COLUMN after_sale_days SMALLINT NOT NULL DEFAULT 15;
ALTER TABLE shop_preferences ADD CONSTRAINT chk_shop_pref_after_sale_days CHECK (after_sale_days BETWEEN 1 AND 365);

-- +goose Down
ALTER TABLE shop_preferences DROP COLUMN after_sale_days;
