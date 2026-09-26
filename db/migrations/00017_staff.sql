-- 后台身份：staff / staff_tokens（数据模型 §14）。
--
-- =============================================================================
-- 这份迁移要解决一件早就被预见、但一直没人解的事
-- =============================================================================
--
-- §14 把 staff.merchant_id 设计成**可空**：NULL 表示平台级操作员，他不属于
-- 任何一家店，能建商家、做跨租户运维。而 db/tenancy.json 的默认 tenant 类
-- 要求策略谓词是 `merchant_id = current_merchant()` —— 那个谓词对 NULL 恒为假
-- （NULL = 任何东西都是 NULL，不是真）。两件事撞在一起的后果很具体：
--
--   **平台级操作员对所有人不可见，包括他自己。** 他登录不进来，因为按
--   token_hash 点查 staff_tokens 时那一行被 RLS 过滤掉了；就算登进来，
--   /admin/me 也查不到他自己那一行。整条平台级路径是死的。
--
-- 做闸门那一轮的实现者写下过这件事（「我在变异模拟里用了标准策略让闸门跑通，
-- 但这是真实的设计问题，清单里还没有对应的类别」）。本轮把它解掉。
--
-- ### 三条路，选了第三条
--
-- ① `merchant_id = current_merchant() OR merchant_id IS NULL`
--
--    **不行，而且它错得比看上去严重。** 表面的代价是「平台级 staff 对每个
--    租户都可见」——商家 A 的管理员能在自己的员工列表里读到平台操作员的邮箱。
--    但真正致命的是写侧：PostgreSQL 的 USING 在没有单独 WITH CHECK 时同时
--    充当写谓词（数据模型 §2 的实测表格里就有这一条），而闸门
--    TestTenantPoliciesArePresentAndExact 也要求两者相同。于是
--    `merchant_id IS NULL` 这一支在写侧的意思是：
--
--        任何一个商家级管理员都能插入一行 merchant_id 为 NULL 的 staff，
--        也就是**给自己造一个平台级管理员**。
--
--    一条为了「让平台级看得见自己」而加的 OR，把整个两级身份模型接成了
--    一个提权入口。读侧的泄露还看得见，这一条不会有任何报错。
--
-- ② 平台级走独立的 BYPASSRLS 角色与连接池（§2 结尾提过这条）
--
--    **代价太大，而且方向反了。** BYPASSRLS 给的是**全部**表的全部行：
--    平台路径从此对 orders、users、payments 一视同仁地敞开，而它真正需要的
--    只是 staff 与 staff_tokens 里那几行 merchant_id 为 NULL 的记录。
--    另外它要一个新的数据库角色（CREATE ROLE ... BYPASSRLS 要超级用户）、
--    一个新的 DSN、一个绕开 internal/db.Guard 的建池路径 —— 而 Guard 存在的
--    全部理由就是「应用绝不能握着能绕过 RLS 的凭据」（db/dsn.go）。
--    为了两张表的几行数据，把那道闸门开一个口子，不划算。
--
-- ③ **把「当前作用域的租户」本身变成一个可以是 NULL 的值**（本迁移采用）
--
--        merchant_id IS NOT DISTINCT FROM staff_scope_merchant()
--
--    staff_scope_merchant() 在普通租户作用域里返回 current_merchant()，
--    在平台作用域里返回 NULL。于是同一条谓词在两种作用域下分别退化成：
--
--        租户作用域：merchant_id IS NOT DISTINCT FROM 42  ≡  merchant_id = 42
--        平台作用域：merchant_id IS NOT DISTINCT FROM NULL ≡ merchant_id IS NULL
--
--    两件事因此同时成立，而它们正是 ① 拿不到的：
--
--      · 租户作用域下的行为与标准策略**逐行等价**。平台级那些 NULL 行对商家
--        不可见（NULL IS NOT DISTINCT FROM 42 是假），商家也插不出 NULL 行
--        （写谓词同一条）。提权入口不存在。
--      · 平台作用域下**只**解锁 merchant_id IS NULL 的行。平台操作员看不见
--        任何一家店的 staff，也插不进任何一家店的 staff —— 比 BYPASSRLS 窄得多，
--        这是故意的：它要的就是「不属于任何租户」这一个抽屉。
--
--    平台作用域由 app.platform_scope 这个 GUC 打开，和 app.merchant_id 一样
--    是 SET LOCAL 的（repository.WithPlatform）。两者的信任级别相同：都由
--    repository 那一层唯一地设置，业务代码拿不到设置它们的入口。
--
-- ### 一个刻意保留的失败方向
--
-- 平台作用域**不设** app.merchant_id。于是平台事务里一旦碰到别的租户表
-- （orders、products…），它们的策略会调 current_merchant()，而那个函数在
-- 没设上下文时是 RAISE EXCEPTION（00002）。也就是说：平台路径误读业务表时
-- 当场报一条说人话的错，而不是安静地返回零行。fail-closed，且看得见。
--
-- =============================================================================
-- 与设计文档的两处偏离，都已同步回 §14
-- =============================================================================
--
--  1. 多一条 uk_staff_email_platform。§14 只有 uk_staff_email(merchant_id, email)，
--     而**那条索引对平台级操作员不去重**：部分唯一索引里 NULL 彼此相异，
--     (NULL, 'ops@x') 可以存在两行。邮箱是这套认证唯一的信任根（§14 原话），
--     两个平台管理员共用一个邮箱意味着一封找回信能开出两把钥匙。
--  2. merchant_id 加 DEFAULT staff_scope_merchant()。理由与 00010 给
--     user_tokens.merchant_id 加 DEFAULT current_merchant() 一字不差：
--     §14 认证流程 ④ 写着「新员工的 merchant_id 继承自调用者，**不接受前端
--     传入**」，而 DEFAULT 让这句话从一条纪律变成一件**调用方没有参数可以传错**
--     的事 —— 生成的 Go 函数签名里根本没有 merchant_id 这个参数。
--     顺带它让 db/queries/staff.sql 一个 merchant_id 都不必写，
--     于是 scripts/check_query_tenancy.py 不需要为这张表开豁免。

-- +goose Up

-- ---------------------------------------------------------------------------
-- 作用域函数。两个都必须是 STABLE，理由见数据模型 §2「坑三」：
-- IMMUTABLE 且无参数的函数会在**计划期**被折成常量，于是第一次执行时的作用域
-- 被烤进预编译计划，后续所有请求都读到第一个作用域的数据 —— 静默的跨租户泄露。
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
-- platform_scope() 回答「本事务是不是平台级作用域」。
--
-- 没设、设成空串、设成别的任何值，一律是 false。默认必须是 false：
-- 这个开关打开之后能看见的东西是「不属于任何租户」的那一抽屉，
-- 而一个默认打开的开关等于没有开关。
CREATE FUNCTION platform_scope() RETURNS BOOLEAN
LANGUAGE plpgsql STABLE AS $$
BEGIN
    RETURN coalesce(nullif(current_setting('app.platform_scope', true), ''), 'off') = 'on';
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- staff_scope_merchant() 是「本作用域属于哪个租户」，**可以是 NULL**。
--
-- 它与 current_merchant() 的关系：平台作用域之外，两者逐值相同；
-- 平台作用域里 current_merchant() 会报错（没设上下文），而这个函数返回 NULL。
-- 所以它不是 current_merchant() 的替代品，只用在 staff 这条线上 ——
-- 别的表用它会把「忘了设租户」从一条报错变成一次静默的空结果集。
CREATE FUNCTION staff_scope_merchant() RETURNS BIGINT
LANGUAGE plpgsql STABLE AS $$
BEGIN
    IF platform_scope() THEN
        RETURN NULL;
    END IF;
    RETURN current_merchant();
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- staff（数据模型 §14）
--
-- 没有 password_hash，这不是遗漏：整个后台不存在密码 —— 认证走 token，
-- 找回走邮箱。少一个密码就少一整类问题（不用定密码策略、不用防撞库、
-- 不用处理「密码写进日志」）。
-- ---------------------------------------------------------------------------
CREATE TABLE staff (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- NULL = 平台级（能建商家、跨租户运维）；非空 = 属于某个商家。
    -- DEFAULT 见文件头偏离 2：调用方没有这个参数可以传错。
    merchant_id   BIGINT      DEFAULT staff_scope_merchant() REFERENCES merchants(id),
    email         TEXT        NOT NULL,           -- 唯一标识，也是找回通道
    name          TEXT        NOT NULL DEFAULT '',
    role          SMALLINT    NOT NULL DEFAULT 2, -- 1管理员 2操作员
    status        SMALLINT    NOT NULL DEFAULT 1, -- 1正常 2停用
    -- 谁加的；引导账号为 NULL。
    --
    -- **单列自引用外键，这是规矩二在全文唯一一处失效的地方**，理由已经登记在
    -- db/tenancy.json 的 fk_single_column_ok 里：复合外键要求父子两边 merchant_id
    -- 相等，而 MATCH SIMPLE 下任一列为 NULL 即跳过检查 —— 于是「平台管理员创建
    -- 某家店的管理员」（§14 认证流程 ②）写不进去，而那是主路径。
    -- 代价写明白：这一处的跨租户挂接数据库拦不住，只能靠「merchant_id 从会话
    -- 上下文取、绝不从请求体取」那条纪律，而上面那个 DEFAULT 正是它的落点。
    created_by    BIGINT      REFERENCES staff(id),
    last_login_at TIMESTAMPTZ,
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 商家级的邮箱唯一性：收在租户内（规矩三），带软删条件（同 users 的理由）。
CREATE UNIQUE INDEX uk_staff_email
    ON staff(merchant_id, email) WHERE deleted_at IS NULL;

-- 平台级的邮箱唯一性。**上面那条索引对它不生效**，这是本迁移补的一处设计洞：
-- 部分唯一索引里 NULL 彼此相异，所以 (NULL, 'ops@x.com') 能存在两行、十行。
-- 首列不是 merchant_id，已登记进 db/tenancy.json 的 unique_global_ok。
CREATE UNIQUE INDEX uk_staff_email_platform
    ON staff(email) WHERE merchant_id IS NULL AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- staff_tokens（数据模型 §14）
--
-- 只存 hash，明文只在签发的那一瞬间存在。后台 token 能读全部订单与客户手机号、
-- 能改价、能发起退款 —— 它和支付密钥是同一个量级的东西。
--
-- 本表按规矩一的豁免条款不带 merchant_id（db/tenancy.json 里写着理由：
-- 跟随 staff，租户归属由 staff.merchant_id 决定；而 staff.merchant_id 本来
-- 就可空，复制一份下来也表达不了「平台级 token 属于谁」）。
-- ---------------------------------------------------------------------------
CREATE TABLE staff_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    staff_id     BIGINT      NOT NULL REFERENCES staff(id) ON DELETE CASCADE,
    token_hash   TEXT        NOT NULL UNIQUE,   -- sha256(明文) 的十六进制
    kind         SMALLINT    NOT NULL,          -- 1引导 2邮件登录链接 3会话
    expire_at    TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,                   -- 一次性 token 用掉即失效
    revoked_at   TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ,
    created_ip   INET,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 校验会话：按 hash 命中且未过期未撤销。
CREATE INDEX idx_staff_tokens_live ON staff_tokens(staff_id, expire_at)
    WHERE used_at IS NULL AND revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- 行级安全。ENABLE 之外必须再加 FORCE（00002 与数据模型 §2「坑一」）。
-- 策略一律叫 tenant（db/tenancy.json 的 policy_name）。
--
-- USING 与 WITH CHECK 都显式写出来（同 00006 的 inventories）：两者在这里确实
-- 等价，但显式写出来之后，将来任何一次「只放宽写侧」的改动都是一次可见的删改，
-- 而不是一次「补上了原来省略的东西」。而在这张表上，写侧被放宽一格的后果是
-- 文件头 ① 说的那个提权入口。
-- ---------------------------------------------------------------------------
ALTER TABLE staff ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON staff
  USING      (merchant_id IS NOT DISTINCT FROM staff_scope_merchant())
  WITH CHECK (merchant_id IS NOT DISTINCT FROM staff_scope_merchant());

-- staff_tokens 的谓词是对父表的 EXISTS 子查询（parent 类），只是那个子查询里
-- 比的是 staff_scope_merchant() 而不是 current_merchant() —— 理由同上：
-- 平台级 token 挂在一行 merchant_id 为 NULL 的 staff 上，而 current_merchant()
-- 在平台作用域里会报错。
ALTER TABLE staff_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_tokens FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON staff_tokens
  USING      (EXISTS (SELECT 1 FROM staff s
                       WHERE s.id = staff_tokens.staff_id
                         AND s.merchant_id IS NOT DISTINCT FROM staff_scope_merchant()))
  WITH CHECK (EXISTS (SELECT 1 FROM staff s
                       WHERE s.id = staff_tokens.staff_id
                         AND s.merchant_id IS NOT DISTINCT FROM staff_scope_merchant()));

-- ---------------------------------------------------------------------------
-- updated_at 触发器。00007 那个 DO 循环只在它自己那一次迁移里跑过，
-- 之后新建的表要自己挂 —— TestUpdatedAtIsMaintainedByTrigger 会盯着。
-- staff_tokens 没有 updated_at（§14 的 DDL 就没有），所以它不在范围内。
-- ---------------------------------------------------------------------------
CREATE OR REPLACE TRIGGER touch_staff_updated_at
    BEFORE UPDATE ON staff FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ---------------------------------------------------------------------------
-- GRANT 面。00005 之后新表的默认权限只有 SELECT，写权限由建表的迁移显式申明。
-- 两张表都吃各自类别的默认四权（db/tenancy.json），TestAppRoleGrantSurface 逐表比对。
--
-- staff 的 DELETE 说一句：停用走 status=2、删除走 deleted_at 软删，所以 DELETE
-- 今天没有调用点。它仍然给，理由同 00009 里 users 那段 —— GRANT 面按**类别**
-- 申明而不是按今天的调用面裁剪。
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON staff, staff_tokens TO keel_app;

-- +goose Down
REVOKE ALL ON staff, staff_tokens FROM keel_app;

DROP POLICY tenant ON staff_tokens;
DROP POLICY tenant ON staff;

DROP TABLE staff_tokens;
DROP TABLE staff;

DROP FUNCTION staff_scope_merchant();
DROP FUNCTION platform_scope();
