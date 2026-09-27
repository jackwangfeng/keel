-- 删掉库存表上的跨域外键（微服务拆分阶段 1b，docs/电商系统-微服务拆分方案.md
-- 「要拆掉的耦合」第 1 节）。
--
-- inventories / inventory_logs 归库存服务，skus / stores / merchants 归 core。拆分部署下它们
-- 在两个库里，外键建不出来；单体库里留着它们，就等于单体与拆分跑的是两套约束 —— 单体上
-- 通过的写入到了拆分部署里才第一次不受约束，那正是「同一套代码、两种部署」要消灭的差别。
-- 所以两种部署一起删，改由应用层保证：
--
--   · 建 SKU 时 core 调库存服务建行（阶段 1a 起就是这样，InitSKUs），SKU 在 core 这边先提交；
--   · 删 SKU 是软删，库存行留着无害（没有任何读路径会从库存反查 SKU）；
--   · 下单扣减只扣 core 定价阶段判过「在架、这家店卖」的 SKU（判定挪到 core，
--     库存服务只认 id 与数，数据模型 §4）；
--   · 对账任务报孤儿（阶段 2）。
--
-- 失去的保证写清楚：数据库不再拒绝「把 A 商家的库存挂到 B 商家的 SKU 上」。
-- 守住它的是 RLS（库存表的 merchant_id = current_merchant()，两种部署都在）与
-- 「core 只拿自己租户里查得到的 SKU 去调库存服务」。db/tenancy.json 的 fk_missing_ok
-- 逐条登记了这些列与理由。
--
-- 约束名取自 00020（inventories_sku_fkey / inventories_store_fkey）与自动命名
-- （*_merchant_id_fkey、inventory_logs_sku_id_merchant_id_fkey、inventory_logs_store_fkey）；
-- 写 IF EXISTS 是为了让这条迁移在一个手工删过其中几条的库上也能跑完。

-- +goose Up
ALTER TABLE inventories    DROP CONSTRAINT IF EXISTS inventories_merchant_id_fkey;
ALTER TABLE inventories    DROP CONSTRAINT IF EXISTS inventories_sku_fkey;
ALTER TABLE inventories    DROP CONSTRAINT IF EXISTS inventories_store_fkey;
ALTER TABLE inventory_logs DROP CONSTRAINT IF EXISTS inventory_logs_merchant_id_fkey;
ALTER TABLE inventory_logs DROP CONSTRAINT IF EXISTS inventory_logs_sku_id_merchant_id_fkey;
ALTER TABLE inventory_logs DROP CONSTRAINT IF EXISTS inventory_logs_store_fkey;

-- +goose Down
-- NOT VALID：删外键之后写进来的行可能已经有孤儿（软删 SKU 之外，拆分部署导回来的数据），
-- 回滚不该因为历史数据卡住。新写入照样受约束；要全量校验另跑 VALIDATE CONSTRAINT。
ALTER TABLE inventories ADD CONSTRAINT inventories_merchant_id_fkey
    FOREIGN KEY (merchant_id) REFERENCES merchants(id) NOT VALID;
ALTER TABLE inventories ADD CONSTRAINT inventories_sku_fkey
    FOREIGN KEY (sku_id, merchant_id) REFERENCES skus(id, merchant_id) NOT VALID;
ALTER TABLE inventories ADD CONSTRAINT inventories_store_fkey
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id) NOT VALID;
ALTER TABLE inventory_logs ADD CONSTRAINT inventory_logs_merchant_id_fkey
    FOREIGN KEY (merchant_id) REFERENCES merchants(id) NOT VALID;
ALTER TABLE inventory_logs ADD CONSTRAINT inventory_logs_sku_id_merchant_id_fkey
    FOREIGN KEY (sku_id, merchant_id) REFERENCES skus(id, merchant_id) NOT VALID;
ALTER TABLE inventory_logs ADD CONSTRAINT inventory_logs_store_fkey
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id) NOT VALID;
