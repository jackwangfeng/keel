-- 商品批量导入的记录（数据模型 §3「product_import_batches」，契约 POST /admin/product-imports）。
-- DDL 照抄数据模型设计，那里是唯一真相源。
--
-- ===========================================================================
-- 这张表只记「确认导入」，不记「预检」
-- ===========================================================================
--
-- 契约把预检定成**不落库**：同一份文件可以反复预检，而确认时客户端把同一份文件
-- 再传一次，服务端重新解析、重新校验。所以这里没有「预检中 / 待确认」这种状态，
-- 也就没有「预检过但从没确认的记录要谁来清」这个问题 —— 一行存在，就说明那一次
-- 确认导入的事务提交了。
--
-- ===========================================================================
-- uk_product_import_batches_sha：同一份文件在一家店只导入一次
-- ===========================================================================
--
-- 幂等键（Idempotency-Key）只挡「同一次提交的重发」；「隔天又把同一份文件导了一遍」
-- 「换了个浏览器标签页再点一次」用的是新钥匙，挡不住。那一层靠文件字节的 sha256：
-- 确认导入的事务**第一句**就是往这里插一行（ON CONFLICT DO NOTHING），撞上了就说明
-- 这份文件导过，直接返回那一次的结果，一件商品都不建。
--
-- 两个并发的确认撞在同一份文件上时，后到的那条 INSERT 会在唯一索引上等前一个
-- 事务结束：前一个提交了，它走 DO NOTHING 分支读到那一行；前一个回滚了，它自己插进去。
-- 没有「两边都建了一遍」的窗口。
--
-- 唯一键以 merchant_id 打头（规矩三），两家店导同一份文件各导各的。
--
-- ===========================================================================
-- staff_id 是单列外键
-- ===========================================================================
--
-- 与 uploads.staff_id 同一个理由：staff 的 merchant_id 可空（平台级操作员），
-- 复合外键要求两边 merchant_id 相等，平台管理员经 X-Keel-Merchant 切进来替店里导入
-- 时写不进去。db/tenancy.json 的 fk_single_column_ok 里有这一条。
--
-- ===========================================================================
-- result 为什么是一块 JSONB
-- ===========================================================================
--
-- 它是那一次导入的**回执**（契约 ProductImportResult 的领域形状）：哪几件建成了、
-- 商品 id 是多少、哪几件没导入、为什么、文件里填了哪些图片地址（只记录没下载）。
-- 「同一份文件再确认一次」要原样把它还给调用方。它只被整块写一次、整块读，
-- 从不按其中某个字段查 —— 拆成子表只会多出一组要登记租户策略的表。
-- 计数那几列单独放，是为了后台列表不必解 JSON 就能显示。

-- +goose Up

CREATE TABLE product_import_batches (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id      BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    file_sha256      TEXT        NOT NULL,
    file_name        TEXT        NOT NULL DEFAULT '',
    file_format      TEXT        NOT NULL,
    staff_id         BIGINT      NOT NULL REFERENCES staff(id),
    total_rows       INT         NOT NULL,
    created_products INT         NOT NULL DEFAULT 0,
    created_skus     INT         NOT NULL DEFAULT 0,
    failed_rows      INT         NOT NULL DEFAULT 0,
    result           JSONB       NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_import_format CHECK (file_format IN ('xlsx', 'csv')),
    CONSTRAINT chk_import_sha256 CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT chk_import_counts CHECK (total_rows >= 0 AND created_products >= 0
                                        AND created_skus >= 0 AND failed_rows >= 0)
);

CREATE UNIQUE INDEX uk_product_import_batches_sha
    ON product_import_batches(merchant_id, file_sha256);

ALTER TABLE product_import_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_import_batches FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON product_import_batches
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON product_import_batches TO keel_app;

-- +goose Down

REVOKE ALL ON product_import_batches FROM keel_app;
DROP POLICY tenant ON product_import_batches;
DROP TABLE product_import_batches;
