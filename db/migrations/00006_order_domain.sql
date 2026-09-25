-- 订单域：orders / order_items / inventories / inventory_logs /
-- order_status_transitions。DDL 照抄数据模型设计 §4 与 §5，那里是唯一真相源。
--
-- 三处与文档的**已知偏离**，每一处都写明了为什么：
--
-- ① orders.user_id 暂时没有外键。文档 §5 写的是
--    `FOREIGN KEY (user_id, merchant_id) REFERENCES users(id, merchant_id)`，
--    而 users 属于 §9，本轮没有建。没有落点就建不出这条外键。
--    这不是一笔会被忘掉的账：internal/db/migrate_test.go 的
--    TestForeignKeysAreNotSilentlyMissing 按「列名 <x>_id + 库里存在同名父表」
--    解析，users 一旦被建出来，orders.user_id 当场变成一条「缺失的外键」而变红，
--    建 users 的那份迁移不补上它就过不去。所以这里**刻意不**往
--    db/tenancy.json 的 fk_missing_ok 里登记——登记等于把这个提醒关掉。
--
-- ② barrier 不在这里建。它是 dtmrs 的 migrate() 产物（数据模型 §6），
--    归 M2 Task 3。清单里它已经是 documented_only。
--
-- ③ §1 说的 touch_updated_at() 触发器全库都还没有（00001 也没建），
--    本轮不为订单域单独开一个先例——那会让「哪些表的 updated_at 是自动的」
--    变成一张要靠人记的表。补的话应当一次补齐所有表，是独立的一份迁移。

-- +goose Up

-- ---------------------------------------------------------------------------
-- 订单状态机（数据模型 §5）。shared-reference 类：全租户共用，没有租户维度。
--
-- 它**不挂 RLS**，也不能挂：挂上之后谓词对任何租户都不成立，所有人都读不到，
-- 状态流转校验会在第一跳就失败。防护改由 GRANT 面承担——keel_app 只读。
-- 任何租户都能改状态机，比跨租户读取更难发现：没有任何一条查询会因此报错，
-- 只是流转规则变了。db/tenancy.json 的 shared-reference 类写着这条。
-- ---------------------------------------------------------------------------
CREATE TABLE order_status_transitions (
    from_status SMALLINT NOT NULL,
    to_status   SMALLINT NOT NULL,
    PRIMARY KEY (from_status, to_status)
);

-- 10 待支付 / 20 已支付 / 30 已发货 / 40 已完成 / 50 退款中 / 60 已退款 / 90 已关闭。
-- 没有 30 → 50 那条边：文档 §5 论证过，已发货订单的退款走 refund_status 这一维，
-- 履约进度不该被售后流程抹掉。
INSERT INTO order_status_transitions (from_status, to_status) VALUES
 (10,20),(10,90),(20,30),(20,50),(30,40),(50,60),(50,20);

-- ---------------------------------------------------------------------------
-- orders（数据模型 §5）
-- ---------------------------------------------------------------------------
CREATE TABLE orders (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id           BIGINT      NOT NULL REFERENCES merchants(id),
    -- 全局唯一是**刻意**的，见 §2 那条分界线：我们自己生成的不可枚举编号保持
    -- 全局唯一（要跟支付渠道对账），外部系统生成的标识一律收进租户内。
    -- 规矩三的闸门按 db/tenancy.json 的 unique_global_ok 里的 orders(order_no) 放行。
    order_no              TEXT        NOT NULL UNIQUE,
    user_id               BIGINT      NOT NULL,   -- 外键见文件头 ①
    status                SMALLINT    NOT NULL DEFAULT 10,
    refund_status         SMALLINT    NOT NULL DEFAULT 0,
    goods_amount_cents    BIGINT      NOT NULL,
    freight_cents         BIGINT      NOT NULL DEFAULT 0,
    discount_cents        BIGINT      NOT NULL DEFAULT 0,
    payable_cents         BIGINT      NOT NULL,
    paid_cents            BIGINT      NOT NULL DEFAULT 0,
    refunded_cents        BIGINT      NOT NULL DEFAULT 0,
    receiver_snapshot     JSONB       NOT NULL,
    remark                TEXT,
    expire_at             TIMESTAMPTZ NOT NULL,
    paid_at               TIMESTAMPTZ,
    shipped_at            TIMESTAMPTZ,
    finished_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_amount CHECK (
        payable_cents = goods_amount_cents + freight_cents - discount_cents
        AND paid_cents >= 0 AND refunded_cents <= paid_cents
    ),
    CONSTRAINT chk_refund_status CHECK (
        (refund_status = 0 AND refunded_cents = 0) OR
        (refund_status = 1) OR
        (refund_status = 2 AND refunded_cents > 0 AND refunded_cents < paid_cents) OR
        (refund_status = 3 AND refunded_cents = paid_cents AND paid_cents > 0)
    )
);
CREATE INDEX idx_orders_user ON orders(merchant_id, user_id, created_at DESC);
CREATE INDEX idx_orders_status_expire ON orders(merchant_id, status, expire_at)
    WHERE status = 10;
-- 供 order_items / shipments / payments / refunds 做复合外键。
CREATE UNIQUE INDEX uk_orders_id_merchant ON orders(id, merchant_id);
CREATE INDEX idx_orders_refunding ON orders(merchant_id, refund_status, updated_at DESC)
    WHERE refund_status = 1;

-- ---------------------------------------------------------------------------
-- order_items（数据模型 §5）
--
-- sku_id / product_id 都是复合外键。上一轮推翻了「快照行上的溯源字段保持无外键」
-- 那个结论：products 走软删除、skus 根本没有删除路径，所以外键从来不会挡住
-- 任何一次实际发生的删除；而不补的代价是 A 商家的订单行可以引用 B 商家的 SKU，
-- 错挂之后展示、对账、退款看起来全都正常，只有拿 sku_id 回查库存时才露馅——
-- 那正是 M2 下单 SAGA 的主链路。
-- ---------------------------------------------------------------------------
CREATE TABLE order_items (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT      NOT NULL REFERENCES merchants(id),
    order_id       BIGINT      NOT NULL,
    sku_id         BIGINT      NOT NULL,
    product_id     BIGINT      NOT NULL,
    title_snapshot TEXT        NOT NULL,
    spec_snapshot  JSONB       NOT NULL,
    image_snapshot TEXT,
    price_cents    BIGINT      NOT NULL,
    quantity       INT         NOT NULL CHECK (quantity > 0),
    amount_cents   BIGINT      NOT NULL,
    discount_cents BIGINT      NOT NULL DEFAULT 0,
    refunded_qty   INT         NOT NULL DEFAULT 0,
    refunded_cents BIGINT      NOT NULL DEFAULT 0,
    CONSTRAINT chk_item_refund CHECK (
        refunded_qty >= 0 AND refunded_qty <= quantity
        AND refunded_cents >= 0 AND refunded_cents <= amount_cents - discount_cents
    ),
    -- 供 refund_items 做复合外键
    UNIQUE (id, merchant_id),
    FOREIGN KEY (order_id, merchant_id)   REFERENCES orders(id, merchant_id),
    FOREIGN KEY (sku_id, merchant_id)     REFERENCES skus(id, merchant_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id)
);
CREATE INDEX idx_order_items_order ON order_items(merchant_id, order_id);

-- ---------------------------------------------------------------------------
-- inventories（数据模型 §4）。parent-scoped 类：按规矩一豁免 merchant_id，
-- 租户归属由 skus 决定。豁免的是「不冗余那一列」，**不是**「不挂策略」——
-- 两者正交，策略谓词可以是任意布尔表达式，不必是本表的列比较。
-- ---------------------------------------------------------------------------
CREATE TABLE inventories (
    sku_id        BIGINT      PRIMARY KEY REFERENCES skus(id),
    available_qty INT         NOT NULL DEFAULT 0,
    warning_qty   INT         NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_qty_nonneg CHECK (available_qty >= 0)
);

-- ---------------------------------------------------------------------------
-- inventory_logs（数据模型 §4）
-- ---------------------------------------------------------------------------
CREATE TABLE inventory_logs (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id      BIGINT      NOT NULL REFERENCES merchants(id),
    sku_id           BIGINT      NOT NULL,
    change_qty       INT         NOT NULL,   -- 正负
    biz_type         SMALLINT    NOT NULL,   -- 1下单扣减 2SAGA补偿回补 3超时关单释放 4退款回补 5手工调整
    biz_id           TEXT        NOT NULL,   -- 订单号等
    before_available INT         NOT NULL,
    after_available  INT         NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (sku_id, merchant_id) REFERENCES skus(id, merchant_id)
);
CREATE INDEX idx_inv_logs_biz ON inventory_logs(merchant_id, biz_id);
CREATE INDEX idx_inv_logs_sku_time
    ON inventory_logs(merchant_id, sku_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 行级安全。ENABLE 之外必须再加 FORCE，理由见 00002 与数据模型 §2「坑一」。
-- 策略一律叫 tenant（db/tenancy.json 的 policy_name）。
-- ---------------------------------------------------------------------------
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON orders USING (merchant_id = current_merchant());

ALTER TABLE order_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE order_items FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON order_items USING (merchant_id = current_merchant());

ALTER TABLE inventory_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_logs FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON inventory_logs USING (merchant_id = current_merchant());

-- inventories 的谓词是对 skus 的 EXISTS 子查询（数据模型 §4）。
--
-- USING 与 WITH CHECK 两侧都写出来，而不是只写 USING 让 PostgreSQL 回退：
-- 两者在这里确实等价，但显式写出来之后，将来任何一次「只放宽写侧」的改动
-- （ALTER POLICY ... WITH CHECK (true)，读侧看不出任何异常）都是一次可见的
-- 删改，而不是一次「补上了原来省略的东西」。
CREATE POLICY tenant ON inventories
  USING      (EXISTS (SELECT 1 FROM skus s
                       WHERE s.id = inventories.sku_id
                         AND s.merchant_id = current_merchant()))
  WITH CHECK (EXISTS (SELECT 1 FROM skus s
                       WHERE s.id = inventories.sku_id
                         AND s.merchant_id = current_merchant()));
ALTER TABLE inventories ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventories FORCE  ROW LEVEL SECURITY;

-- ---------------------------------------------------------------------------
-- GRANT 面。00005 之后新表的默认权限只有 SELECT，写权限由建表的这份迁移
-- 显式申明。这里逐表写全（而不是只补差额），是为了让 GRANT 面在这一处能一眼读完；
-- internal/db/migrate_test.go 的 TestAppRoleGrantSurface 按 db/tenancy.json
-- 的类别逐表比对，多给少给都会红。
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE
    ON orders, order_items, inventories, inventory_logs TO keel_app;
-- shared-reference：只读。状态机是全租户共用的静态参考数据。
GRANT SELECT ON order_status_transitions TO keel_app;

-- +goose Down
REVOKE ALL ON order_status_transitions FROM keel_app;
REVOKE ALL ON orders, order_items, inventories, inventory_logs FROM keel_app;

DROP POLICY tenant ON inventories;
DROP POLICY tenant ON inventory_logs;
DROP POLICY tenant ON order_items;
DROP POLICY tenant ON orders;

DROP TABLE inventory_logs;
DROP TABLE inventories;
DROP TABLE order_items;
DROP TABLE orders;
DROP TABLE order_status_transitions;
