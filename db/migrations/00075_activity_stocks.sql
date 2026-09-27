-- 活动配额搬家：activity_stocks（微服务拆分阶段 1b，docs/电商系统-微服务拆分方案.md
-- 「服务边界」与「要拆掉的耦合」第 1 节）。
--
-- 秒杀 / 限时折扣的「配额」与「已售」是**库存**，不是营销规则：它们在下单扣减那一刻与门店库存
-- 同生共死（同一个库存分支、同一个本地事务），在关单时一起放回。00058 把它们放在
-- promotion_skus.stock_qty / sold_qty，于是库存服务拆出去之后，同一个概念会被切成两半 ——
-- 门店库存在库存库，活动配额在 core 库，扣减再也装不进一个本地事务。所以搬过来：
--
--   activity_stocks(merchant_id, promotion_id, sku_id, quota, sold)   库存服务独有
--   promotion_skus（价格配置 + per_user_limit）                         core 独有
--
-- 单体库里这条迁移建表并从 promotion_skus 回填；拆分部署的库存库里，同一张表由
-- db/migrations-inventory 的第一条迁移建（CREATE TABLE IF NOT EXISTS，与这里逐列一致）。
--
-- ### 不带任何外键
--
-- promotion_id / sku_id 指向 core 的 promotions / skus，拆分之后那两张表在另一个库里，
-- 外键根本建不出来；merchant_id 也不指向 merchants（库存库里没有 merchants）。
-- 一致性由应用层保证：core 改活动商品时调库存服务整组设配额（「卖出过的不能移除、配额不能
-- 低于已售」由库存服务在它自己的事务里判），对账任务报孤儿（阶段 2）。db/tenancy.json 的
-- fk_missing_ok 里逐条登记了理由。
--
-- ### promotion_skus.stock_qty / sold_qty：本版停用，不删列
--
-- 与 00062 同一个 expand / contract：代码从这一版起不再读写这两列（配额与已售只经库存服务），
-- 列留一个版本，旧实例在滚动发布的窗口里仍能读到它们；下一版再 DROP COLUMN。
-- 回填之后两边会分叉（新代码只动 activity_stocks），Down 因此把 activity_stocks 的数写回去 ——
-- 回滚到旧版时旧代码读到的是最新的已售件数，而不是这条迁移那一刻的快照。
-- 滚动发布的窗口里旧实例仍按 promotion_skus 扣配额、新实例按 activity_stocks 扣，两边各算各的：
-- 窗口内的秒杀可能多卖出「旧实例那一份」。有秒杀在跑时请先停旧实例再迁移（部署指南）。
--
-- ### quota = 0 表示不限（限时折扣），与 stock_qty 的约定一致
--
-- 限时折扣也有一行：sold 照样累计（「卖出过的 SKU 不能移出活动」这条规则对两种活动都成立，
-- 判据就是 sold > 0），只是不设上限。行不存在 = 还没同步配额，库存服务按「配额不足」拒绝扣减、
-- core 计价时跳过那个报价（按门店价报）—— 宁可少卖，不超卖。

-- +goose Up
CREATE TABLE activity_stocks (
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant(),
    promotion_id BIGINT      NOT NULL,
    sku_id       BIGINT      NOT NULL,
    quota        INT         NOT NULL DEFAULT 0,   -- 活动配额，0 = 不限（限时折扣）
    sold         INT         NOT NULL DEFAULT 0,   -- 已按活动价售出（含待支付）
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, promotion_id, sku_id),
    CONSTRAINT chk_activity_stock_qty CHECK (
        quota >= 0 AND sold >= 0 AND (quota = 0 OR sold <= quota)
    )
);
-- 计价与商品标签按一批 SKU 取它们身上的活动余量。
CREATE INDEX idx_activity_stocks_sku ON activity_stocks(merchant_id, sku_id);

INSERT INTO activity_stocks (merchant_id, promotion_id, sku_id, quota, sold)
SELECT merchant_id, promotion_id, sku_id, stock_qty, sold_qty
  FROM promotion_skus;

COMMENT ON TABLE activity_stocks IS
    '活动配额与已售（库存服务独有，微服务拆分阶段 1b，00075）。不带外键，理由见 00075 文件头。';
COMMENT ON COLUMN promotion_skus.stock_qty IS
    '已停用（00075）：配额搬到 activity_stocks.quota，代码不再读写；下一版删列。';
COMMENT ON COLUMN promotion_skus.sold_qty IS
    '已停用（00075）：已售搬到 activity_stocks.sold，代码不再读写；下一版删列。';

-- 行级安全：ENABLE 之外必须再加 FORCE（数据模型 §2「坑一」）。
ALTER TABLE activity_stocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE activity_stocks FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON activity_stocks
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_activity_stocks_updated_at
    BEFORE UPDATE ON activity_stocks FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- GRANT 面：tenant 类的四权（db/tenancy.json）。00005 之后新表默认只有 SELECT。
GRANT SELECT, INSERT, UPDATE, DELETE ON activity_stocks TO keel_app;

-- +goose Down
UPDATE promotion_skus ps
   SET stock_qty = a.quota, sold_qty = a.sold
  FROM activity_stocks a
 WHERE a.merchant_id = ps.merchant_id AND a.promotion_id = ps.promotion_id AND a.sku_id = ps.sku_id;
COMMENT ON COLUMN promotion_skus.stock_qty IS NULL;
COMMENT ON COLUMN promotion_skus.sold_qty IS NULL;
DROP TABLE activity_stocks;
