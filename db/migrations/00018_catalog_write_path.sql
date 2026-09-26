-- 商家写路径的表结构（数据模型 §3 商品域 + §13 文件存储）。M4 Task 2。
--
-- M4 的第一个任务补了 16 条 /admin/ 写接口的**契约**，并把几处 DDL 写进了数据
-- 模型文档，而 db/migrations 一个字都没改 —— 那几张表/列至今只存在于文档里。
-- 本文件把它们落到库里。**不含 handler**：后台身份（staff 会话）是另一条并发
-- 任务（00017），没有它这些接口连「这次是谁在写」都回答不了。
--
-- 四件事，一次落地：
--
--   ① uploads（§13）        —— 文件元数据表，此前从未建过
--   ② skus.deleted_at（§3） —— 软删，外加 uk_skus_code 改成部分唯一索引
--   ③ product_images（§3）  —— 商品图与商品的关联，双复合外键
--   ④ 三张老表的 merchant_id 补 DEFAULT current_merchant()
--
-- ===========================================================================
-- 一、本文件**依赖 00017 建出 staff**，这不是可选的
-- ===========================================================================
--
-- §13 的 uploads 带 「staff_id BIGINT REFERENCES staff(id)」，而 staff 由并发的
-- 00017（后台身份）建。一开始想把这条外键略掉、等 00017 落地再补，
-- **闸门当场否掉了这条路**，实测报错原文：
--
--     --- FAIL: TestCrossTenantForeignKeysAreComposite
--         migrate_test.go:408: fk_single_column_ok 里的 uploads(staff_id)
--                              已经不是一条单列跨租户外键了，该清理了
--
-- db/tenancy.json 的 fk_single_column_ok 里 uploads(staff_id) 这条豁免是
-- **早就登记好的**（清单刻意是前瞻的）。Go 侧的过期检查只对「库里已有的表」
-- 生效，而 uploads 一旦建出来它就生效了 —— 于是「uploads 已存在、staff 还没有」
-- 是一个闸门不接受的中间态。清单没法两头讨好：把这条豁免删掉，
-- Python 侧（读设计文档，文档里 staff 一直在）就会以规矩二报红。
--
-- 换句话说，**闸门自己说了顺序：staff 必须排在 uploads 前面**。所以 00018
-- 排在 00017 之后是硬的，不是凑巧。合并时 00017 必须先进来。
--
-- ===========================================================================
-- 二、与文档 DDL 的一处偏离：merchant_id 有了 DEFAULT current_merchant()
-- ===========================================================================
--
-- §3 / §13 的 DDL 原本都写着 「merchant_id BIGINT NOT NULL REFERENCES
-- merchants(id)」，没有默认值。照抄的话，本轮的入库语句里就必须出现 merchant_id
-- 这个词，而 scripts/check_query_tenancy.py 不许 db/queries 里出现它
-- （理由见那个脚本的文件头：应用层再过滤一遍租户，「RLS 到底有没有生效」
-- 就变得测不出来了）。
--
-- 这是 00010 / 00012 / 00013 / 00014 / 00016 用过的同一条路子，第六次。
-- **数据模型 §3 与 §13 的 DDL 已随本轮一起改**，两边保持一份真相。
--
-- 老表那三条（categories / products / skus）走 ALTER COLUMN SET DEFAULT，
-- 与 00013 给 orders / order_items / inventory_logs 补默认值是同一个动作：
-- DEFAULT 只在**列被省略时**才生效，所以种子、夹具、迁移里那些显式带
-- merchant_id 的 INSERT 一个都不受影响。
--
-- 默认值同时是一道形状约束：生成出来的 Go 函数签名里根本没有 merchant_id，
-- 「拿 A 店的上下文往 B 店名下建商品」连编译都编不出来。RLS 的 WITH CHECK
-- 仍是第二道。
--
-- ===========================================================================
-- 三、uk_skus_code 为什么必须 DROP 再 CREATE，而不是 ALTER
-- ===========================================================================
--
-- PostgreSQL 没有「给已有唯一索引加一个 WHERE」的 ALTER。而这一步本身是
-- DELETE SKU 这条接口能不能成立的前提：软删不删行，不改索引的话一个被删掉的
-- 规格会**永久占住一个货号**，而货号是商家自己的编码体系，不是我们发的 id
-- （契约 DELETE /admin/skus/{sku_id} 的描述里明写了这一条）。
--
-- Down 段把它还原成原来的全局形状。**那一步在有软删数据的库上会失败** ——
-- 两行同货号（一行活的、一行删掉的）在非部分索引下是重复键。这是 Down 的
-- 真实语义，不是缺陷：回滚这次迁移就是要回到「货号不可复用」的世界，
-- 而那个世界里这两行本来就不该同时存在。冷库上 migrate-down → migrate
-- 一轮是干净的（本轮实测）。

-- +goose Up

-- ---------------------------------------------------------------------------
-- ① uploads（数据模型 §13）
-- ---------------------------------------------------------------------------
--
-- 没有 updated_at，所以不挂 touch_updated_at 触发器 —— 文件元数据是一次性写入
-- 的，除了 referenced 从 FALSE 翻到 TRUE 之外没有别的可变字段。
--
-- user_id 走复合外键，staff_id 走单列外键，两者待遇不同不是笔误：staff 是全文
-- 唯一一张 merchant_id 可空的表（平台级操作员为 NULL），复合外键在它身上会让
-- 「平台管理员替某家店传文件」写不进去。db/tenancy.json 的 fk_single_column_ok
-- 里三条指向 staff 的豁免说的就是这件事。
CREATE TABLE uploads (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id   BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    -- 上传者二选一，见 §14「uploads 的归属」：C 端用户或后台操作员
    user_id       BIGINT,
    staff_id      BIGINT      REFERENCES staff(id),
    purpose       SMALLINT    NOT NULL,   -- 1商品图 2头像 3退款凭证
    driver        SMALLINT    NOT NULL DEFAULT 1,  -- 1本地磁盘 2S3兼容
    storage_key   TEXT        NOT NULL,   -- driver 内部路径，不对外暴露
    content_type  TEXT        NOT NULL,
    size_bytes    BIGINT      NOT NULL,
    sha256        TEXT        NOT NULL,
    referenced    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_upload_size CHECK (size_bytes > 0),
    CONSTRAINT chk_upload_owner CHECK (
        (user_id IS NOT NULL) <> (staff_id IS NOT NULL)
    ),
    -- 供 product_images 做复合外键：让「把别家租户的文件挂到自己商品上」在物理上不可能
    UNIQUE (id, merchant_id),
    FOREIGN KEY (user_id, merchant_id) REFERENCES users(id, merchant_id)
);

-- 刻意保持**全局**唯一：storage_key 是磁盘或对象存储里的物理路径，两个租户写到
-- 同一个路径就是真的互相覆盖文件。已登记在 db/tenancy.json 的 unique_global_ok。
CREATE UNIQUE INDEX uk_uploads_key ON uploads(driver, storage_key);

-- 孤儿回收任务扫描：创建超过 24 小时仍未被引用的文件。
CREATE INDEX idx_uploads_orphan
    ON uploads(merchant_id, created_at) WHERE NOT referenced;

ALTER TABLE uploads ENABLE ROW LEVEL SECURITY;
ALTER TABLE uploads FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON uploads USING (merchant_id = current_merchant());

-- tenant 类的默认 GRANT 面（db/tenancy.json），TestAppRoleGrantSurface 逐表比对。
-- 00005 之后新表只自动拿到 SELECT，写权限必须显式给 —— 少给的症状是运行期 42501，
-- 而 42501 同时也是 RLS 拒绝的 SQLSTATE，两者在日志里长得一模一样。
GRANT SELECT, INSERT, UPDATE, DELETE ON uploads TO keel_app;

-- ---------------------------------------------------------------------------
-- ② skus.deleted_at + uk_skus_code 改成部分唯一索引（数据模型 §3）
-- ---------------------------------------------------------------------------
--
-- 为什么是软删而不是真的 DELETE 一行：order_items / cart_items / inventories /
-- inventory_logs 四张表都对 skus 有外键，一个卖过一次的 SKU 在数据库层面就删不掉。
ALTER TABLE skus ADD COLUMN deleted_at TIMESTAMPTZ;

DROP INDEX uk_skus_code;
CREATE UNIQUE INDEX uk_skus_code
    ON skus(merchant_id, sku_code) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- ③ product_images（数据模型 §3）
-- ---------------------------------------------------------------------------
--
-- 主图 = sort_order 最小的那一张，**没有 is_primary 布尔**：布尔允许「零张主图」
-- 与「两张主图」这两种业务上不存在的状态被表达出来，而顺序本来就要维护。
--
-- 不带 deleted_at：纯关联表，解除关联就是删行。写入面是整组替换
-- （PUT /admin/products/{product_id}/images），不是增量 —— 顺序是一个整体，
-- 增量接口下「把第 3 张挪到第 1 位」与「删掉第 2 张」两个并发请求会互相踩出
-- 一个有空洞的顺序。
--
-- 两条复合外键是这张表存在的理由之一：有了 (upload_id, merchant_id)，
-- 「把别家的图挂到自己商品上」在物理上不可能。挂 JSONB 列做不到这一点。
CREATE TABLE product_images (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    product_id  BIGINT      NOT NULL,
    upload_id   BIGINT      NOT NULL,
    sort_order  INT         NOT NULL DEFAULT 0,   -- 0 即主图
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, product_id, upload_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id),
    FOREIGN KEY (upload_id, merchant_id)  REFERENCES uploads(id, merchant_id)
);
CREATE INDEX idx_product_images_product
    ON product_images(merchant_id, product_id, sort_order);

ALTER TABLE product_images ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_images FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON product_images USING (merchant_id = current_merchant());
GRANT SELECT, INSERT, UPDATE, DELETE ON product_images TO keel_app;

-- ---------------------------------------------------------------------------
-- ④ 老表的 merchant_id 补 DEFAULT（理由见文件头第二段）
-- ---------------------------------------------------------------------------
ALTER TABLE categories ALTER COLUMN merchant_id SET DEFAULT current_merchant();
ALTER TABLE products   ALTER COLUMN merchant_id SET DEFAULT current_merchant();
ALTER TABLE skus       ALTER COLUMN merchant_id SET DEFAULT current_merchant();

-- +goose Down

ALTER TABLE skus       ALTER COLUMN merchant_id DROP DEFAULT;
ALTER TABLE products   ALTER COLUMN merchant_id DROP DEFAULT;
ALTER TABLE categories ALTER COLUMN merchant_id DROP DEFAULT;

DROP POLICY tenant ON product_images;
DROP TABLE product_images;

-- 还原成 00001 的全局形状。见文件头第三段：库里已有同货号的软删行时这一步会
-- 失败，那是 Down 的真实语义。
DROP INDEX uk_skus_code;
CREATE UNIQUE INDEX uk_skus_code ON skus(merchant_id, sku_code);
ALTER TABLE skus DROP COLUMN deleted_at;

DROP POLICY tenant ON uploads;
DROP TABLE uploads;
