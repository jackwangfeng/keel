-- 应用角色：RLS 的前提条件。
--
-- 超级用户与带 BYPASSRLS 的角色无条件绕过行级安全，FORCE 也拦不住。
-- postgres 官方镜像按 POSTGRES_USER 建出来的角色就是超级用户，所以「应用直接用
-- 建库那个角色连过来」这个最省事的做法，会让前一份迁移里所有 RLS 静默失效：
-- 既读得到别家租户的数据，也不会在忘记设租户上下文时报错。
--
-- 属主边界（改动前务必读）：五张表的属主保持为跑迁移的那个角色（通常是 keel），
-- keel_app 只是被授权者，不是属主。因此 keel_app 走的是普通 RLS 路径，
-- ENABLE 就足以约束它；00002 里的 FORCE 是给「属主自己连上来」这种部署形态兜底的。
-- 如果以后有人 ALTER TABLE ... OWNER TO keel_app，安全边界就只剩 FORCE 一道，
-- 那时 00002 的 FORCE 从「冗余保险」变成「唯一防线」——别把它当多余的删掉。

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'keel_app') THEN
        CREATE ROLE keel_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    ELSE
        -- 角色是集群级对象，可能是别的库建的。无论如何把危险属性压掉。
        ALTER ROLE keel_app NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
END $$;
-- +goose StatementEnd

-- 口令只有开发默认值，生产必须经 KEEL_APP_PASSWORD 注入。
-- 单独一行是因为下面要开 ENVSUB：goose 的变量替换按行做，而 $$ 会被它当成转义的 $，
-- 所以上面那个 DO 块必须留在 ENVSUB 之外。
-- +goose ENVSUB ON
ALTER ROLE keel_app PASSWORD '${KEEL_APP_PASSWORD:-keel_app}';
-- +goose ENVSUB OFF

GRANT USAGE ON SCHEMA public TO keel_app;
GRANT SELECT, INSERT, UPDATE, DELETE
    ON merchants, shop_settings, categories, products, skus TO keel_app;

-- 以后新增的表自动带上同样的授权。没有这条，每加一张表都要有人记得补 GRANT，
-- 而漏补的表在 RLS 之外还多一层「应用根本读不到」的故障，症状却和漏 GRANT 无关。
-- 默认权限挂在「建表的那个角色」上，所以用 current_user 而不是写死 keel。
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                   'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO keel_app',
                   current_user);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                   'GRANT USAGE, SELECT ON SEQUENCES TO keel_app',
                   current_user);
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                   'REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM keel_app',
                   current_user);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                   'REVOKE USAGE, SELECT ON SEQUENCES FROM keel_app',
                   current_user);
END $$;
-- +goose StatementEnd

REVOKE ALL ON merchants, shop_settings, categories, products, skus FROM keel_app;
REVOKE USAGE ON SCHEMA public FROM keel_app;

-- 角色是集群级的，同集群别的库可能还在用它，所以删不掉就留着，不让回滚失败。
-- +goose StatementBegin
DO $$
BEGIN
    DROP ROLE keel_app;
EXCEPTION
    WHEN dependent_objects_still_exist OR insufficient_privilege THEN
        RAISE NOTICE 'keel_app 仍被其它对象引用，保留该角色';
END $$;
-- +goose StatementEnd
