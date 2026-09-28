-- 公开的 AI 经营日志（AI 经营 M11，docs/AI经营-M10M11设计.md §8）：店铺开关，默认关。开了之后
-- GET /api/v1/ai-log（买家侧、免登录）返回最近的简报与提案（标题、状态、效果），演示站用它做 /ai-log/ 页。
-- 简报与提案里只有经营数字与商品名（没有买家信息），但它们是店的经营数据 —— 公不公开是店长的决定，所以默认关。
-- +goose Up
ALTER TABLE shop_preferences ADD COLUMN public_ai_log BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE shop_preferences DROP COLUMN public_ai_log;
