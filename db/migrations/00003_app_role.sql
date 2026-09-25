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
-- 口令经环境变量注入，开发有默认值。
--
-- 它只在「角色本来不存在」那一支里用，**绝不能**写成顶层的无条件 ALTER ROLE：
-- 角色是集群级对象，而 GRANT 是单库的，所以同一个集群里每多一个库就要再跑一遍
-- 这份迁移。写成无条件的话，给第二个库跑 goose up 而忘了带 KEEL_APP_PASSWORD，
-- 会把生产上已经设好的强口令静默重置回开发默认值——没有报错，没有痕迹，
-- 只是某一天应用连不上，或者更糟：连得上，而口令是公开在版本库里的那个。
--
-- 口令经 SET LOCAL 传进下面的 DO 块，而不是直接写在块里：goose 的 ENVSUB 按行替换，
-- 且会把 $$ 当成转义的 $，所以 ${...} 和 $$ 不能出现在同一段里。
-- SET LOCAL 只在本事务内有效（goose 默认把每份迁移包在事务里），出了事务即失效。
-- +goose ENVSUB ON
SET LOCAL keel.app_password = '${KEEL_APP_PASSWORD:-keel_app}';
-- +goose ENVSUB OFF

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'keel_app') THEN
        -- 用 format %L 而不是字符串拼接：口令里有单引号也不会破坏语句。
        EXECUTE format(
            'CREATE ROLE keel_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE '
            'PASSWORD %L', current_setting('keel.app_password'));
    ELSE
        -- 角色已存在（同集群别的库建的，或这是重跑）。
        -- 危险属性无论如何压掉——不跳过，否则别人建的同名弱角色能混进来。
        -- 但口令一个字不碰，理由见上。需要轮换口令请走单独的运维操作。
        ALTER ROLE keel_app NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
END $$;
-- +goose StatementEnd

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
