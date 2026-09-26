-- 自动确认收货（数据模型 §5「发货的三条规则」之三）：发货后 N 天，30 已发货 → 40 已完成。
-- DDL 照抄数据模型设计 §2 / §5，那里是唯一真相源。
--
-- ===========================================================================
-- 一、N 从哪来：shop_settings.auto_confirm_days 这一列早就在
-- ===========================================================================
--
-- 00001 建 shop_settings 时就带着 auto_confirm_days SMALLINT NOT NULL DEFAULT 7，
-- §5 那句「本轮未落地：shop_settings 还没有 auto_confirm_days 这一列」是一句过期的话，
-- 本轮在文档里一并改掉。默认 7 天沿用文档与 00001，不另起一个数。
--
-- 开店（00021）不写 shop_settings，所以一家店可能**根本没有这一行**。那时按列默认值
-- 7 天走（repository.AutoConfirmDays），而不是「没有配置就不自动确认」——
-- 后者的症状是一家新店的订单永远停在 30，而表面上一切正常。
--
-- 这里补一条 CHECK：天数必须在 1～365 之间。0 或负数意味着「一发货就自动确认」
-- （扫描条件是 shipped_at < now() - N 天），买家连申请售后的窗口都没有；
-- 上限只挡明显录错的值（多敲一个 0）。它不是一条业务上限，是一条防呆。
--
-- ===========================================================================
-- 二、扫描用的索引
-- ===========================================================================
--
-- 扫描的真实谓词是 merchant_id = current_merchant() AND status = 30
-- AND shipped_at < 截止时间（RLS 补上前一半），按 shipped_at 从早到晚取。
-- 已有的索引都不对：idx_orders_admin_status 是 (merchant_id, status, created_at)，
-- 排序列不对；idx_orders_status_expire 是部分索引，只收 10。
-- 部分索引只收 30：已完成的单会越积越多，而它们永远不再被这条扫描关心。

-- +goose Up

ALTER TABLE shop_settings ADD CONSTRAINT chk_auto_confirm_days
    CHECK (auto_confirm_days BETWEEN 1 AND 365);

CREATE INDEX idx_orders_auto_confirm ON orders(merchant_id, shipped_at)
    WHERE status = 30;

-- +goose Down
DROP INDEX idx_orders_auto_confirm;
ALTER TABLE shop_settings DROP CONSTRAINT chk_auto_confirm_days;
