-- 单价上限：一亿元（10,000,000,000 分，catalogimport.MaxPriceCents）。
--
-- 2026-09-28 破坏性测试：后台录入 SKU 价格没有上界，int64 最大值原样入库；买家把它与别的商品一起结算，
-- 应付合计溢出成负数，试算与下单 500；单买这一件能下出应付 9.2×10¹⁸ 分的订单并进入支付。
-- 服务层已在录入时拒（422）；这里是任何路径的兜底，并保证求和不溢出（一亿元 × 999 件 × 50 行 ≈ 5×10¹⁴ 分）。
--
-- NOT VALID：只约束今后的写入，不扫已有行（与 00063 同一个理由：大表加约束不锁全表太久；已有的越界行
-- 由人工清理后再 VALIDATE）。批量导入早就按一亿元拒（catalogimport），正常数据不会越界。
-- +goose Up
ALTER TABLE skus              ADD CONSTRAINT chk_price_upper        CHECK (price_cents <= 10000000000 AND cost_cents <= 10000000000) NOT VALID;
ALTER TABLE region_sku_prices ADD CONSTRAINT chk_region_price_upper CHECK (price_cents <= 10000000000) NOT VALID;
ALTER TABLE store_sku_prices  ADD CONSTRAINT chk_store_price_upper  CHECK (price_cents <= 10000000000) NOT VALID;
ALTER TABLE promotion_skus    ADD CONSTRAINT chk_promotion_sku_price_upper CHECK (promo_price_cents <= 10000000000) NOT VALID;

-- +goose Down
ALTER TABLE promotion_skus    DROP CONSTRAINT chk_promotion_sku_price_upper;
ALTER TABLE store_sku_prices  DROP CONSTRAINT chk_store_price_upper;
ALTER TABLE region_sku_prices DROP CONSTRAINT chk_region_price_upper;
ALTER TABLE skus              DROP CONSTRAINT chk_price_upper;
