-- 库存库的全部表（微服务拆分阶段 1b，docs/电商系统-微服务拆分方案.md「数据库与迁移」）。
--
-- 这个目录是库存服务自己的迁移，goose 版本表是 goose_db_version_inventory（make migrate-inventory），
-- 与 core 的 goose_db_version 分开 —— 两个目录可以指向同一个库（单体）也可以指向两个库（拆分），
-- 各记各的版本，谁也不会把对方的编号当成自己的。
--
-- ===========================================================================
-- 同一份迁移，两种库：单体库里是空操作，新库里建出全部
-- ===========================================================================
--
-- 单体库里这些东西早就由 core 迁移建好了（inventories 00006/00020、inventory_logs 00006/00020/00063、
-- barrier 00008、activity_stocks 00075、current_merchant() 00002、touch_updated_at() 00007）。
-- 所以这里每一句都是「不存在才建」：CREATE TABLE / INDEX IF NOT EXISTS，函数、策略、触发器、
-- 角色都先查系统目录。于是：
--
--   · 单体库（先跑过 make migrate）：这份迁移什么都不改，只在 goose_db_version_inventory 里记一笔；
--   · 拆分部署的新库：建出与单体库**逐列一致**的四张表 + 两个函数 + RLS 策略 + 授权。
--
-- 表结构必须与 core 迁移建出来的逐字一致（列、默认值、约束、索引）。不一致的后果是：
-- 单体上测过的东西到了拆分部署里行为不同，而那正是「同一套代码」要消灭的差别。
-- internal/handler 的两库测试在一个只跑过本目录的库上跑完整条下单 / 关单 / 退款链路，守住这一点。
--
-- ===========================================================================
-- 库存库里没有 merchants：RLS 怎么成立
-- ===========================================================================
--
-- 租户隔离在库存库里照旧是 RLS：三张业务表各带 merchant_id，策略是
-- merchant_id = current_merchant()，FORCE ROW LEVEL SECURITY。它**不需要 merchants 表**：
--
--   · current_merchant() 只读事务级的 app.merchant_id（没设就 42501 报错），不查任何表；
--     库存服务按请求头 X-Keel-Merchant-ID（内网调用，已验签）或 gid 里的租户（SAGA 分支）
--     开事务时 SET LOCAL 进去 —— 与 core 同一段 repository.withTenantTx；
--   · merchant_id 的 DEFAULT current_merchant() 让插入写不出别家的租户，与 core 同一个机制；
--   · 指向 merchants(id) 的外键删掉了（core 那边由 00076 一起删），库存库里本来也建不出来。
--
-- 于是「租户从哪来」在库存库里只有一个答案：调用方（core）在一个已经判过权的请求里给的租户。
-- 库存服务不认识商家是否存在、是否停用 —— 那是 core 的事，和它不认识 SKU 在不在架一样。
--
-- barrier 是 cross-tenant-infra（主键是全局事务维度，没有 merchant_id），与 core 库里那张
-- 同一个 GRANT 面：keel_app 只有 INSERT（理由见 db/migrations/00008_barrier.sql）。
--
-- ===========================================================================
-- 建在哪个 schema：连接串的 search_path 决定（B 档）
-- ===========================================================================
--
-- 所有「已存在就跳过」的判断都按 current_schema()，不写死 public：B 档用
-- `options=-csearch_path=inventory` 连上来，表、函数、策略、触发器全部落进 inventory schema，
-- 与 public 里 core 那份互不相干（public 里有同名的 current_merchant() 也不会被当成「已存在」）。
-- A 档与 C 档 current_schema() 就是 public，与改之前逐字等价。
-- inventory schema 本身与 keel_inventory 角色由 scripts/split-migrate.sh prepare-b 先建好
-- （goose 要先在这个 schema 里建版本表，迁移自己建不了它）。
--
-- ===========================================================================
-- 不可回滚
-- ===========================================================================
--
-- Down 什么都不做：单体库里这些表归 core 迁移所有，库存目录回滚时去 DROP 它们就是删掉整个
-- 库存。拆分部署的库存库要推倒重来，删库重建（make migrate-inventory）即可。

-- +goose Up

-- 应用角色：与 db/migrations/00003 同一段（角色是集群级对象，库存库可能在另一个集群上）。
-- 口令只在角色不存在时用，已存在就只压掉危险属性、不碰口令（理由写在 00003）。
--
-- 角色名可配（KEEL_INVENTORY_ROLE，默认 keel_app）：A 档与两库测试都用 keel_app；
-- B 档（同一个 Postgres、独立 schema）必须换成 keel_inventory —— 否则 core 的 keel_app
-- 也拿到了库存表的权限，「靠权限隔离」就不成立。下面所有 GRANT 都给这个角色。
-- +goose ENVSUB ON
SET LOCAL keel.inventory_role = '${KEEL_INVENTORY_ROLE:-keel_app}';
SET LOCAL keel.app_password = '${KEEL_INVENTORY_PASSWORD:-keel_app}';
-- +goose ENVSUB OFF

-- +goose StatementBegin
DO $$
DECLARE r TEXT := current_setting('keel.inventory_role');
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
        EXECUTE format(
            'CREATE ROLE %I LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE '
            'PASSWORD %L', r, current_setting('keel.app_password'));
    ELSE
        EXECUTE format('ALTER ROLE %I NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE', r);
    END IF;
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', current_schema(), r);
END $$;
-- +goose StatementEnd

-- current_merchant()：与 db/migrations/00002 逐字一致。已存在（单体库）就不动它。
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
                    WHERE n.nspname = current_schema() AND p.proname = 'current_merchant') THEN
        EXECUTE $f$
CREATE FUNCTION current_merchant() RETURNS BIGINT
LANGUAGE plpgsql STABLE AS $b$
DECLARE v TEXT := nullif(current_setting('app.merchant_id', true), '');
BEGIN
    IF v IS NULL THEN
        RAISE EXCEPTION '租户上下文未设置：事务开始时必须 SET LOCAL app.merchant_id'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN v::BIGINT;
END $b$
$f$;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
                    WHERE n.nspname = current_schema() AND p.proname = 'touch_updated_at') THEN
        EXECUTE $f$
CREATE FUNCTION touch_updated_at() RETURNS trigger AS $b$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$b$ LANGUAGE plpgsql
$f$;
    END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- inventories（数据模型 §4）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inventories (
    sku_id        BIGINT      NOT NULL,
    available_qty INT         NOT NULL DEFAULT 0,
    warning_qty   INT         NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    merchant_id   BIGINT      NOT NULL DEFAULT current_merchant(),
    store_id      BIGINT      NOT NULL,
    CONSTRAINT inventories_pkey PRIMARY KEY (sku_id, store_id),
    CONSTRAINT chk_qty_nonneg CHECK (available_qty >= 0)
);
CREATE INDEX IF NOT EXISTS idx_inventories_store ON inventories(merchant_id, store_id)
    WHERE available_qty > 0;
CREATE INDEX IF NOT EXISTS idx_inventories_warning ON inventories(merchant_id, store_id, sku_id)
    WHERE available_qty <= warning_qty;

-- ---------------------------------------------------------------------------
-- inventory_logs（数据模型 §4）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inventory_logs (
    id               BIGINT GENERATED ALWAYS AS IDENTITY,
    merchant_id      BIGINT      NOT NULL DEFAULT current_merchant(),
    sku_id           BIGINT      NOT NULL,
    change_qty       INT         NOT NULL,
    biz_type         SMALLINT    NOT NULL,
    biz_id           TEXT        NOT NULL,
    before_available INT         NOT NULL,
    after_available  INT         NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    store_id         BIGINT      NOT NULL,
    reason           TEXT,
    CONSTRAINT inventory_logs_pkey PRIMARY KEY (id),
    CONSTRAINT chk_inv_logs_reason_len CHECK (reason IS NULL OR char_length(reason) <= 200)
);
CREATE INDEX IF NOT EXISTS idx_inv_logs_biz ON inventory_logs(merchant_id, biz_id);
CREATE INDEX IF NOT EXISTS idx_inv_logs_sku_time
    ON inventory_logs(merchant_id, store_id, sku_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- activity_stocks（活动配额与已售，db/migrations/00075）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS activity_stocks (
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant(),
    promotion_id BIGINT      NOT NULL,
    sku_id       BIGINT      NOT NULL,
    quota        INT         NOT NULL DEFAULT 0,
    sold         INT         NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT activity_stocks_pkey PRIMARY KEY (merchant_id, promotion_id, sku_id),
    CONSTRAINT chk_activity_stock_qty CHECK (
        quota >= 0 AND sold >= 0 AND (quota = 0 OR sold <= quota)
    )
);
CREATE INDEX IF NOT EXISTS idx_activity_stocks_sku ON activity_stocks(merchant_id, sku_id);

-- ---------------------------------------------------------------------------
-- barrier（子事务屏障，db/migrations/00008；形状照抄 dtmrs-barrier v0.11.0，别加列）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS barrier (
    trans_type  TEXT   NOT NULL,
    gid         TEXT   NOT NULL,
    branch_id   TEXT   NOT NULL,
    op          TEXT   NOT NULL,
    barrier_id  TEXT   NOT NULL,
    reason      TEXT   NOT NULL,
    create_time BIGINT NOT NULL,
    CONSTRAINT barrier_pkey PRIMARY KEY (gid, branch_id, op, barrier_id)
);

-- ---------------------------------------------------------------------------
-- 行级安全、触发器（已存在就跳过：CREATE POLICY / TRIGGER 没有 IF NOT EXISTS）
-- ---------------------------------------------------------------------------
ALTER TABLE inventories     ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventories     FORCE  ROW LEVEL SECURITY;
ALTER TABLE inventory_logs  ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_logs  FORCE  ROW LEVEL SECURITY;
ALTER TABLE activity_stocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE activity_stocks FORCE  ROW LEVEL SECURITY;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = current_schema()
                    AND tablename = 'inventories' AND policyname = 'tenant') THEN
        CREATE POLICY tenant ON inventories
          USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());
    END IF;
    -- inventory_logs 的策略只有 USING（与 core 库里 00020 那一条逐字一致）：
    -- 没写 WITH CHECK 时 PostgreSQL 拿 USING 当插入检查，效果相同。
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = current_schema()
                    AND tablename = 'inventory_logs' AND policyname = 'tenant') THEN
        CREATE POLICY tenant ON inventory_logs USING (merchant_id = current_merchant());
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = current_schema()
                    AND tablename = 'activity_stocks' AND policyname = 'tenant') THEN
        CREATE POLICY tenant ON activity_stocks
          USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'touch_inventories_updated_at'
                    AND tgrelid = 'inventories'::regclass) THEN
        CREATE TRIGGER touch_inventories_updated_at
            BEFORE UPDATE ON inventories FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'touch_activity_stocks_updated_at'
                    AND tgrelid = 'activity_stocks'::regclass) THEN
        CREATE TRIGGER touch_activity_stocks_updated_at
            BEFORE UPDATE ON activity_stocks FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
    END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- GRANT 面（与 core 库一致；GRANT 本来就是幂等的）
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
DO $$
DECLARE r TEXT := current_setting('keel.inventory_role');
BEGIN
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON inventories, inventory_logs, activity_stocks TO %I', r);
    EXECUTE format('GRANT SELECT, USAGE ON SEQUENCE inventory_logs_id_seq TO %I', r);
    EXECUTE format('REVOKE ALL ON barrier FROM %I', r);
    EXECUTE format('GRANT INSERT ON barrier TO %I', r);
END $$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
