-- 活动配额的**定义**回到 core：promotion_skus.quota_qty（2026-09-30，总体架构「派生数据同步约定」）。
--
-- 00075 把配额与已售整块搬进了库存服务的 activity_stocks，core 只在写活动时把请求里的配额「转交」过去，
-- 自己一份都不留。于是 core 与库存服务之间的同步只能是「先调库存服务、再写活动表」两段，中间失败两边
-- 就不一致，只有每小时的库存对账能发现。改成二阶段消息之后（service/promotion_quota_msg.go）：活动的写入
-- 与「要同步配额」的消息在 core 的同一个本地事务里提交，库存服务收到消息后**回源**读 core 的定义再设 ——
-- 这就要求 core 手里有「运营配的是多少」这个真相。所以：
--
--   promotion_skus.quota_qty   运营配置的配额（定义，core 的真相；0 = 不限，与 activity_stocks.quota 同一约定）
--   activity_stocks.quota      库存服务里生效的那一份（由消息同步；扣减只认它）
--   activity_stocks.sold       已售（库存服务独有，core 从不写）
--
-- 为什么不复用停用了的 stock_qty：它 NOT NULL DEFAULT 0，而 00075 之后写的行全是 0 —— 读出来分不清
-- 「不限」与「没记录」，拿它去同步会把那些活动的配额改成不限。
--
-- ### NULL = 这一行写于本迁移之前，core 不知道它的配额
--
-- 不回填。单体库里 activity_stocks 就在旁边，回填是一条 UPDATE；但拆分部署的 core 库里那张表是 cutover
-- 之前的旧快照（split-migrate.sh 只收回了权限，没删数据），拿它回填就是把旧配额写成定义。同步时 NULL 的行
-- 「沿用库存服务的现值」（inventory 包 activity_msg.go），运营下一次整组提交活动商品时它们就都有值了。

-- +goose Up
ALTER TABLE promotion_skus ADD COLUMN quota_qty INT;
ALTER TABLE promotion_skus ADD CONSTRAINT chk_promotion_skus_quota_qty CHECK (quota_qty IS NULL OR quota_qty >= 0);
COMMENT ON COLUMN promotion_skus.quota_qty IS
    '运营配置的活动配额（定义，00180）；库存服务的 activity_stocks.quota 由二阶段消息按它同步。NULL = 写于 00180 之前，同步时沿用库存服务现值。';

-- +goose Down
ALTER TABLE promotion_skus DROP CONSTRAINT chk_promotion_skus_quota_qty;
ALTER TABLE promotion_skus DROP COLUMN quota_qty;
