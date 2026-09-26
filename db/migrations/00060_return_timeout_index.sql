-- 退货超时未寄回自动关闭（service/return_timeout.go）的扫描索引。
-- DDL 照抄数据模型设计 §11，那里是唯一真相源。
--
-- 定时任务十分钟一轮、每家店扫一次：
--
--   status = 20 AND refund_type = 2 AND return_submitted_at IS NULL AND audited_at < 截止
--
-- 此前 refunds 上以状态打头的索引只有 idx_refunds_admin_status（merchant_id, status,
-- created_at DESC, id DESC）—— 能把行收窄到「这家店的 20」，但按 created_at 排、不认
-- 退款类型与寄回物流，每一轮都要把这家店全部 20 的单读一遍再过滤。
--
-- 部分索引只收「等买家寄回、还没寄」的那一截：填了物流、走到 30、被撤回的单全都
-- 不在里面，所以它在任何时候都只有「正在等寄回」那么大（通常一家店几条到几十条）。
-- 按 audited_at 排，与扫描的 ORDER BY 对上，LIMIT 取最久的那几条不用排序。
--
-- 不用 CREATE INDEX CONCURRENTLY：理由与 00035 / 00057 相同（goose 把迁移包在事务里）。
-- 这条索引很小（部分索引），建它挡写的时间可以忽略。

-- +goose Up
CREATE INDEX idx_refunds_return_due ON refunds(merchant_id, audited_at)
    WHERE status = 20 AND refund_type = 2 AND return_submitted_at IS NULL;

-- +goose Down
DROP INDEX idx_refunds_return_due;
