-- 渠道单建 keel 订单时的依据（第三期审查修复）：
--
--   {"order_no": "<keel 单号>", "items": {"<平台行 ID>": <order_items.id>}, "absorbed_refunds": ["<平台退款 ID>", …]}
--
-- items：平台行 → keel 订单行。同一个变体可以在平台单上占两行（价不同 / 分开加购），按 SKU 对不出是哪一行，
-- 平台退款按平台行 ID 落到 keel 订单行上。
-- absorbed_refunds：建 keel 订单时平台上已经有的、带行的退款。keel 订单按当时的剩余件数（currentQuantity）建，
-- 这些退款退掉的件数已经不在 keel 订单里，之后不再记成 keel 退款单（否则重复退）。
-- order_no 标明这份依据属于哪一张 keel 订单：「重试」重新建单时整份换掉，旧草稿关掉之后它的依据就作废。
-- 每次 UpdateChannelOrderSnapshot 都会重写 amounts / lines，所以依据单独放一列，只在建草稿时写。
-- channel_orders 是第三期的新表、还很小，ADD COLUMN 带常量默认值只改目录、不重写表。
-- +goose Up
ALTER TABLE channel_orders ADD COLUMN keel_basis JSONB NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
ALTER TABLE channel_orders DROP COLUMN keel_basis;
