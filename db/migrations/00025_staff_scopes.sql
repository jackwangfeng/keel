-- 分级权限：大区管理员（role 3）、门店管理员（role 4）与他们的管辖范围（数据模型 §14）。
--
-- =============================================================================
-- 为什么要这一张表
-- =============================================================================
--
-- 00017 的 staff 只有两层角色（1 管理员 / 2 操作员），区别只有「能不能管员工」。
-- 商品、门店、大区、改价、改库存这些写接口一处角色检查都没有，于是一家连锁的
-- 任何一个店员都能改所有大区、所有门店的价格与库存。
--
-- 新增的两个角色都带「管哪几块」：一个大区管理员可以管多个大区，一个门店管理员
-- 可以管多家店。多对多，所以是一张关联表，而不是 staff 上的一列 region_id。
--
-- =============================================================================
-- 形状
-- =============================================================================
--
--   · 一行 = 一个人对一个大区**或**一家门店的管辖。CHECK 保证恰好一个非空：
--     两个都填的一行说不清它授予的是什么（整个大区？还是只有那家店？），
--     两个都空的一行授予的是「空」，却会让「他至少有一个范围」的判据误判成真。
--   · merchant_id NOT NULL：范围只对商家级员工有意义。平台级员工没有范围
--     （他们要么是平台管理员，要么是平台操作员，二者对一家店都是全店范围）。
--   · 三条复合外键（规矩二）。staff 那一条要求 staff 上有 (id, merchant_id) 的
--     唯一索引 —— 本迁移补上 uk_staff_id_merchant。它对 merchant_id 为 NULL 的
--     平台级行也成立（NULL 彼此相异，唯一性由 id 保证），而本表的 merchant_id
--     非空，于是 MATCH SIMPLE 下这条外键**总会**被检查：一个平台级员工的 id
--     配上任何一家店的 merchant_id 都对不上 staff 里那一行，插不进来。
--     也就是说「范围只挂在商家级员工身上、且只挂在他自己那家店的大区与门店上」
--     这件事由数据库钉死，不靠代码记得。
--   · ON DELETE CASCADE：三张父表在业务路径上都只软删，硬删只发生在运维与测试
--     清理里。那时范围行随父行一起消失是唯一合理的语义 —— 一条指向不存在的
--     门店的范围授予的是「空」，留着它只会让清理脚本多一条排序约束。
--   · 软删的门店 / 大区留下的范围行**不删**：业务层判范围时只认存在且未软删的
--     门店（StoreScope 本来就只返回未软删的），于是它们授予的也是「空」。
--     恢复软删（今天没有这条路径）时范围自动回来，这比删掉再重建诚实。
--
-- =============================================================================
-- 这张表只是数据，判据在业务层
-- =============================================================================
--
-- 「大区管理员能改本大区门店的价」这类规则没有一条进得了数据库：RLS 策略里
-- 读不到「当前是哪个员工」（app.merchant_id 之外没有别的会话变量，也不该有 ——
-- 那会让 keel_app 的每一条查询都依赖一个应用层塞进来的身份）。租户之间的隔离
-- 仍然由 RLS 兜底，这一层管的是**同一个租户内部**谁能做什么，落在
-- internal/service/authz.go 那几个函数里。
--
-- 范围每个请求从库里重读（与 staff.role / staff.status 同一条理由，
-- internal/auth/staff_middleware.go 的 StaffIdentity 注释）：会话是 7 天，
-- 一个被收回了某家店的人必须在下一个请求就失去它。

-- +goose Up

-- 供 staff_scopes 做复合外键。
CREATE UNIQUE INDEX uk_staff_id_merchant ON staff(id, merchant_id);

CREATE TABLE staff_scopes (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    staff_id    BIGINT      NOT NULL,
    region_id   BIGINT,
    store_id    BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_staff_scope_one_target
        CHECK ((region_id IS NULL) <> (store_id IS NULL)),
    FOREIGN KEY (staff_id, merchant_id)  REFERENCES staff(id, merchant_id)   ON DELETE CASCADE,
    FOREIGN KEY (region_id, merchant_id) REFERENCES regions(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (store_id, merchant_id)  REFERENCES stores(id, merchant_id)  ON DELETE CASCADE
);

-- 同一个人对同一个大区 / 门店至多一行。两条部分唯一索引而不是一条
-- (staff_id, region_id, store_id)：NULL 彼此相异，那一条对任何一行都不去重。
CREATE UNIQUE INDEX uk_staff_scopes_region ON staff_scopes(merchant_id, staff_id, region_id)
    WHERE region_id IS NOT NULL;
CREATE UNIQUE INDEX uk_staff_scopes_store ON staff_scopes(merchant_id, staff_id, store_id)
    WHERE store_id IS NOT NULL;

ALTER TABLE staff_scopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_scopes FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON staff_scopes
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON staff_scopes TO keel_app;

-- +goose Down

REVOKE ALL ON staff_scopes FROM keel_app;
DROP POLICY tenant ON staff_scopes;
DROP TABLE staff_scopes;
DROP INDEX uk_staff_id_merchant;
