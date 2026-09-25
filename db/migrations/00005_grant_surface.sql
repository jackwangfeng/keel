-- 收窄 keel_app 的 GRANT 面。
--
-- 00003 的注释说 merchants / shop_settings「靠 keel_app 的 GRANT 面与解析层自身」
-- 防护 —— 它们被刻意豁免出 RLS，因为租户解析必须在 SET LOCAL 之前读这两张表。
-- 实测那句话当时没有兑现：GRANT 面是 SELECT/INSERT/UPDATE/DELETE 全给。
--
--   租户 1 的上下文里改租户 2 的 domain（域名劫持）→ UPDATE 1
--   租户 1 的上下文里把租户 2 停用（全站 DoS）     → UPDATE 1
--   完全没有租户上下文时改 merchants               → UPDATE 1
--
-- M1 不可利用，因为一个写接口都没有。但「不可利用」是应用层的偶然属性，
-- 不是数据库的性质：M2 之后只要出现「商家改自己的店铺设置」这类接口，
-- 同一条连接就能改别家的域名和状态。RLS 拦不住它（这两张表刻意没挂），
-- 解析层也拦不住它（解析层只读，越权发生在别的语句里）。
--
-- 解析层需要 SELECT，所以只收写权限。建店、迁移、种子走的是管理员角色。
--
-- 默认权限从「四权」收成「只读」。
--
-- 00003 给未来的每一张表都自动授了四权，理由是「每加一张表都要有人记得补 GRANT，
-- 而漏补的表在 RLS 之外还多一层故障」。那个理由在只有租户表的世界里成立，
-- 在真实的表清单里不成立：数据模型里已经躺着 order_status_transitions /
-- refund_status_transitions 这类**全租户共用的静态参考数据**，它们没有租户维度、
-- 挂不了 RLS，四权全给等于任何租户都能改订单状态机。
-- 那比跨租户读取更难发现 —— 没有任何一条查询会因此报错，只是状态流转变了。
--
-- 所以默认值改成最小的那个：新表自动可读，写权限由建表的那份迁移显式申明。
-- 「忘了补 GRANT」这件事不再靠人记得：internal/db/migrate_test.go 的
-- TestAppRoleGrantSurface 按 db/tenancy.json 里的类别逐表比对实际权限，
-- 多给和少给都会红。这正是 00003 当年缺的那一半 —— 它用一个过宽的默认值
-- 换掉了一个本该由闸门承担的职责。
--
-- 默认权限挂在「建表的那个角色」上，所以用 current_user 而不是写死 keel。
-- REVOKE 只从已登记的默认 ACL 里减项，重复执行无副作用。

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                   'REVOKE INSERT, UPDATE, DELETE ON TABLES FROM keel_app',
                   current_user);
END $$;
-- +goose StatementEnd

REVOKE INSERT, UPDATE, DELETE ON merchants, shop_settings FROM keel_app;

-- +goose Down
GRANT INSERT, UPDATE, DELETE ON merchants, shop_settings TO keel_app;

-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                   'GRANT INSERT, UPDATE, DELETE ON TABLES TO keel_app',
                   current_user);
END $$;
-- +goose StatementEnd
