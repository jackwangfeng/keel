-- 营销域：coupon_templates / coupon_scopes / user_coupons，以及 orders 上指向券的那一列。
-- DDL 照抄数据模型设计 §7（与 §5 orders 的一行增补），那里是唯一真相源。
--
-- ===========================================================================
-- 与 §7 原稿相比，这一份改了什么（文档已同步改）
-- ===========================================================================
--
-- ① 适用范围多两种：5 大区、6 门店。
--    §7 原稿写于多门店之前，只有「全场 / 分类 / 商品 / 品牌」四种。连锁的真实
--    需求是「华北大区做活动」「新店开业券」，而价格已经是三层（基准 / 大区 /
--    门店），券的范围跟着多两层是同一件事。
--    语义：大区 / 门店是**下单那家店**的维度，不是商品的维度 —— 在范围外的
--    门店下单，这张券整张不可用；而分类 / 商品 / 品牌决定的是**哪几行**参与计算。
--
-- ② 包邮券（coupon_type = 4）被 CHECK 拒绝。
--    运费在这个系统里不存在：pricing.go 的 freightNotBilledThisRelease 明写
--    「本期不计运费」，orders.freight_cents 恒为落账值 0。一张包邮券因此永远减 0，
--    而买家会看到「已用包邮券」—— 那是一个点了没有效果的功能。枚举值 4 保留，
--    等运费模板落地的那一轮把这条 CHECK 放开。
--
-- ③ 立减券（coupon_type = 3）门槛必须为 0，且不带折扣字段。
--    原稿只写了 discount_cents > 0。立减的定义就是「无门槛」，有门槛的立减就是
--    满减；两种写法表达同一张券，后台与报表就要猜哪种是真的。CHECK 让它只有一种。
--    满减同理不许带 discount_rate / max_discount_cents，折扣不许带 discount_cents。
--
-- ④ 券实例的状态机改成「锁定 → 已使用」两步，状态 2 启用。
--    原稿是 SAGA 正向直接 1 → 3（已使用），「2 锁定」保留不用。那样下单没付钱的
--    单子就已经把券「用掉」了，超时关单要把一张已使用的券退回去 —— 而「已使用」
--    在对账里的意思是「这笔优惠真的给出去了」。现在：
--      SAGA 正向（下单）      1 未使用 → 2 锁定，回填 order_id
--      SAGA 补偿 / 超时关单   2 锁定   → 1 未使用，清空 order_id
--      支付回调（同一事务）   2 锁定   → 3 已使用
--    chk_user_coupon_state 把「状态」与「order_id / locked_at / used_at 有没有值」
--    钉在一起，两者对不上的行写不进去。
--
-- ⑤ 新增三列：coupon_templates.claimable（领券中心可领）、user_coupons.source
--    （1 领取 / 2 定向发放）、orders.user_coupon_id（这一单用哪张券）。
--    最后一列是 SAGA 的硬约束逼出来的：分支只拿到 (gid, branch_id, op) 三个字符串，
--    「这一单用的是哪张券」推不出来，只能落在订单行上 —— 与「扣哪些 SKU」落在
--    order_items 上是同一个理由（order.go 文件头）。
--    它与 user_coupons.order_id 不是冗余：前者是「这一单**想**用哪张券」，下单时写，
--    永不改；后者是「这张券**现在**被哪一单占着」，补偿时清空。补偿之后只剩前者
--    还记得这一单曾经用过哪张券。
--
-- ⑥ 所有 merchant_id 带 DEFAULT current_merchant()，所有索引以 merchant_id 打头
--    （§2 规矩与 00013 同一个理由：db/queries 里不许出现 merchant_id 这个词）。
--
-- ===========================================================================
-- 领券不超发：靠的是模板那一行的行锁，不是应用层的先查后写
-- ===========================================================================
--
-- 领取那条路径的第一条语句是
--   UPDATE coupon_templates SET issued_count = issued_count + 1
--    WHERE id = ? AND (total_count = 0 OR issued_count < total_count) ...
-- READ COMMITTED 下，并发的第二个 UPDATE 会等第一个提交，然后在**最新版本**上
-- 重新评估 WHERE（EvalPlanQual）—— 所以最后一张被抢走之后，后到的那几个
-- 受影响 0 行，而不是各自看着旧快照都 +1。chk_coupon_issuance 是它背后的兜底：
-- 即便有人绕过那条 UPDATE，issued_count 也写不过 total_count。
--
-- 每人限领搭同一把锁：同一模板的所有领取在模板行上排队，拿到锁之后的下一条
-- 语句（数这个人已经有几张）在新快照里看得见前一个领取者已提交的那一行。
-- 代价是同一模板的领取串行化 —— 那正是「总量有限」本身的形状，不是额外的代价。

-- +goose Up

-- ---------------------------------------------------------------------------
-- coupon_templates（券模板 / 批次）
-- ---------------------------------------------------------------------------
CREATE TABLE coupon_templates (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id        BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    name               TEXT        NOT NULL,
    coupon_type        SMALLINT    NOT NULL,             -- 1满减 2折扣 3立减 4包邮（保留，见文件头 ②）
    threshold_cents    BIGINT      NOT NULL DEFAULT 0,   -- 满多少可用，0 表示无门槛
    discount_cents     BIGINT      NOT NULL DEFAULT 0,   -- 满减 / 立减的减免额
    discount_rate      SMALLINT    NOT NULL DEFAULT 0,   -- 折扣，千分比：850 = 8.5 折
    max_discount_cents BIGINT      NOT NULL DEFAULT 0,   -- 折扣封顶，0 = 不封顶
    valid_mode         SMALLINT    NOT NULL DEFAULT 1,   -- 1绝对时间 2领取后N天
    valid_start_at     TIMESTAMPTZ,
    valid_end_at       TIMESTAMPTZ,
    valid_days         INT         NOT NULL DEFAULT 0,
    total_count        INT         NOT NULL DEFAULT 0,   -- 总量，0 = 不限
    issued_count       INT         NOT NULL DEFAULT 0,   -- 已发出（领取 + 定向发放）
    per_user_limit     INT         NOT NULL DEFAULT 1,   -- 每人限领（只约束领券中心）
    claimable          BOOLEAN     NOT NULL DEFAULT FALSE, -- 领券中心可领
    stackable          BOOLEAN     NOT NULL DEFAULT FALSE, -- 第一期不启用
    priority           SMALLINT    NOT NULL DEFAULT 0,     -- 第一期不启用
    extra_rules        JSONB       NOT NULL DEFAULT '{}',
    status             SMALLINT    NOT NULL DEFAULT 1,     -- 1启用 0停用
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_coupon_rule CHECK (
        threshold_cents >= 0 AND max_discount_cents >= 0 AND (
        (coupon_type = 1 AND discount_cents > 0 AND threshold_cents >= discount_cents
                         AND discount_rate = 0 AND max_discount_cents = 0) OR
        (coupon_type = 2 AND discount_rate BETWEEN 1 AND 999 AND discount_cents = 0) OR
        (coupon_type = 3 AND discount_cents > 0 AND threshold_cents = 0
                         AND discount_rate = 0 AND max_discount_cents = 0)
        )
    ),
    CONSTRAINT chk_coupon_validity CHECK (
        (valid_mode = 1 AND valid_start_at IS NOT NULL AND valid_end_at > valid_start_at
                        AND valid_days = 0) OR
        (valid_mode = 2 AND valid_days > 0 AND valid_start_at IS NULL AND valid_end_at IS NULL)
    ),
    CONSTRAINT chk_coupon_issuance CHECK (
        total_count >= 0 AND issued_count >= 0 AND per_user_limit >= 1
        AND (total_count = 0 OR issued_count <= total_count)
    ),
    CONSTRAINT chk_coupon_status CHECK (status IN (0, 1)),
    -- 供 coupon_scopes / user_coupons 做复合外键
    UNIQUE (id, merchant_id)
);
CREATE INDEX idx_coupon_templates_list ON coupon_templates(merchant_id, id DESC);
CREATE INDEX idx_coupon_templates_center ON coupon_templates(merchant_id, id DESC)
    WHERE status = 1 AND claimable;

-- ---------------------------------------------------------------------------
-- coupon_scopes（适用范围）
-- ---------------------------------------------------------------------------
CREATE TABLE coupon_scopes (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT   NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    template_id BIGINT   NOT NULL,
    scope_type  SMALLINT NOT NULL,  -- 1全场 2分类 3商品 4品牌 5大区 6门店
    target_id   BIGINT,             -- 按 scope_type 指向 category / product / brand / region / store
    include     BOOLEAN  NOT NULL DEFAULT TRUE,
    CONSTRAINT chk_coupon_scope CHECK (
        (scope_type = 1 AND target_id IS NULL AND include) OR
        (scope_type BETWEEN 2 AND 6 AND target_id IS NOT NULL)
    ),
    FOREIGN KEY (template_id, merchant_id) REFERENCES coupon_templates(id, merchant_id)
);
CREATE INDEX idx_coupon_scopes_tpl ON coupon_scopes(merchant_id, template_id);
CREATE UNIQUE INDEX uk_coupon_scopes ON coupon_scopes(merchant_id, template_id, scope_type, target_id)
    NULLS NOT DISTINCT;

-- ---------------------------------------------------------------------------
-- user_coupons（券实例）
-- ---------------------------------------------------------------------------
CREATE TABLE user_coupons (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    coupon_code    TEXT        NOT NULL UNIQUE,
    template_id    BIGINT      NOT NULL,
    user_id        BIGINT      NOT NULL,
    source         SMALLINT    NOT NULL,             -- 1领券中心 2商家定向发放
    status         SMALLINT    NOT NULL DEFAULT 1,   -- 1未使用 2锁定 3已使用 4已过期
    order_id       BIGINT,                           -- 锁定时回填，补偿时清空
    valid_start_at TIMESTAMPTZ NOT NULL,
    valid_end_at   TIMESTAMPTZ NOT NULL,
    locked_at      TIMESTAMPTZ,
    used_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_user_coupon_source CHECK (source IN (1, 2)),
    CONSTRAINT chk_user_coupon_window CHECK (valid_end_at > valid_start_at),
    CONSTRAINT chk_user_coupon_state CHECK (
        (status = 1 AND order_id IS NULL     AND locked_at IS NULL     AND used_at IS NULL) OR
        (status = 2 AND order_id IS NOT NULL AND locked_at IS NOT NULL AND used_at IS NULL) OR
        (status = 3 AND order_id IS NOT NULL AND used_at IS NOT NULL) OR
        (status = 4 AND order_id IS NULL)
    ),
    -- 供 orders.user_coupon_id 做复合外键
    UNIQUE (id, merchant_id),
    FOREIGN KEY (template_id, merchant_id) REFERENCES coupon_templates(id, merchant_id),
    FOREIGN KEY (user_id, merchant_id)     REFERENCES users(id, merchant_id),
    FOREIGN KEY (order_id, merchant_id)    REFERENCES orders(id, merchant_id)
);
CREATE INDEX idx_user_coupons_avail
    ON user_coupons(merchant_id, user_id, status, valid_end_at) WHERE status = 1;
CREATE INDEX idx_user_coupons_user ON user_coupons(merchant_id, user_id, id DESC);
CREATE INDEX idx_user_coupons_tpl ON user_coupons(merchant_id, template_id, user_id);
CREATE UNIQUE INDEX uk_user_coupons_order
    ON user_coupons(order_id) WHERE order_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- orders.user_coupon_id（见文件头 ⑤）
-- ---------------------------------------------------------------------------
ALTER TABLE orders ADD COLUMN user_coupon_id BIGINT;
ALTER TABLE orders ADD CONSTRAINT orders_user_coupon_fkey
    FOREIGN KEY (user_coupon_id, merchant_id) REFERENCES user_coupons(id, merchant_id);
-- 优惠只有券这一个来源：没挂券的订单，优惠必须是 0。
-- 这是资金链路上最便宜的一道保险 —— 一笔凭空打了折的订单，数据库直接拒绝。
-- 哪天有了第二个优惠来源（会员价、满减活动），这条 CHECK 要跟着改，而那正是
-- 该有人停下来想一想的时刻。
ALTER TABLE orders ADD CONSTRAINT chk_discount_needs_coupon
    CHECK (user_coupon_id IS NOT NULL OR discount_cents = 0);

-- ---------------------------------------------------------------------------
-- 行级安全：ENABLE 之外必须再加 FORCE（§2「坑一」）
-- ---------------------------------------------------------------------------
ALTER TABLE coupon_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE coupon_templates FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON coupon_templates
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE coupon_scopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE coupon_scopes FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON coupon_scopes
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE user_coupons ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_coupons FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON user_coupons
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_coupon_templates_updated_at
    BEFORE UPDATE ON coupon_templates FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- GRANT 面：tenant 类的四权（db/tenancy.json）。00005 之后新表默认只有 SELECT。
GRANT SELECT, INSERT, UPDATE, DELETE
   ON coupon_templates, coupon_scopes, user_coupons TO keel_app;

-- +goose Down

ALTER TABLE orders DROP CONSTRAINT chk_discount_needs_coupon;
ALTER TABLE orders DROP CONSTRAINT orders_user_coupon_fkey;
ALTER TABLE orders DROP COLUMN user_coupon_id;
REVOKE ALL ON coupon_templates, coupon_scopes, user_coupons FROM keel_app;
DROP TABLE user_coupons;
DROP TABLE coupon_scopes;
DROP TABLE coupon_templates;
