-- 单字搜索（深度审查 2026-09-27：「杯」「咖」无结果）：让索引任务把全库 search_text 重写一遍。
--
-- 切分算法改了（search.IndexTerms：二元组后追加单字），search_text 那一格的指纹版本跟着升到
-- bigram-v2（search.SearchTextVersion）。但**版本号只管判定，不管触发**：索引任务的触发点
-- ListStaleProductsForIndex 看的是 products.updated_at 走没走到 product_understanding.updated_at
-- 前面，已有商品一件也不会被重新捞回来 —— 那正是 derive.go 里 SearchTextFingerprint 注释
-- 点名的「没被触发点扫到的商品永远不会被重新判定」。
--
-- 所以把 product_understanding.updated_at 拨回纪元：每件商品都会被重新判定一次。判定按两格
-- 指纹各自比对，text_embedding 那一格的指纹没变（TemplateVersion 没动），**不重算向量**；
-- 只有 search_text 被重写。
--
-- 为什么不是别的写法：
--   · 把 search_text 置 NULL 也能触发，但没配 KEEL_EMBED_ENDPOINT 的栈里索引任务不启动，
--     它会一直是 NULL，关键词召回整条断掉 —— 比「单字搜不到」坏得多。拨时间戳在那种栈里
--     什么都不改变（旧 search_text 原样可用）。
--   · 碰 products.updated_at 会让「商品最近修改时间」对商家说谎。
--
-- 触发器 touch_product_understanding_updated_at 会把任何 UPDATE 的 updated_at 覆写成 now()，
-- 只在本事务里临时关掉它。
-- +goose Up
ALTER TABLE product_understanding DISABLE TRIGGER touch_product_understanding_updated_at;
UPDATE product_understanding SET updated_at = 'epoch'::timestamptz;
ALTER TABLE product_understanding ENABLE TRIGGER touch_product_understanding_updated_at;

-- +goose Down
-- 无需回退：时间戳拨回只会多触发一次判定，下一轮索引任务就会把它写回当前时刻。
SELECT 1;
