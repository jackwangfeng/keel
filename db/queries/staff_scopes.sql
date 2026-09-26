-- 大区管理员 / 门店管理员的管辖范围（数据模型 §14 staff_scopes，迁移 00025）。
--
-- 不带 WHERE merchant_id（RLS 过滤），注释里不许有反引号。见 products.sql
-- 与 inventories.sql 的文件头。
--
-- **只在租户作用域里调**：这张表是标准的 tenant 类，策略调 current_merchant()，
-- 在平台作用域里那是 RAISE 而不是零行。平台级员工按定义没有范围，
-- 所以 service 那一层对平台级身份根本不走到这里。

-- name: ListStaffScopes :many
-- 一个人的全部范围。每个后台请求都会跑一次（StaffBearer 那一次查库），
-- 走 uk_staff_scopes_region / uk_staff_scopes_store 的前缀。
SELECT ss.region_id, ss.store_id
  FROM staff_scopes ss
 WHERE ss.staff_id = $1
 ORDER BY ss.region_id NULLS LAST, ss.store_id NULLS LAST;

-- name: ListStaffScopesFor :many
-- 一批人的范围，给员工列表拼响应用。一次查完而不是一人一次：
-- 一页 100 个员工就是 100 次往返。
SELECT ss.staff_id, ss.region_id, ss.store_id
  FROM staff_scopes ss
 WHERE ss.staff_id = ANY(sqlc.arg(staff_ids)::bigint[])
 ORDER BY ss.staff_id, ss.region_id NULLS LAST, ss.store_id NULLS LAST;

-- name: DeleteStaffScopes :exec
-- 清空一个人的范围。改范围一律是「整体替换」：先清后插，同一个事务。
DELETE FROM staff_scopes WHERE staff_id = $1;

-- name: InsertStaffRegionScope :exec
-- merchant_id 由 DEFAULT current_merchant() 给出；复合外键保证这个大区与这个人
-- 属于同一家店（也就是本作用域那一家）。
INSERT INTO staff_scopes (staff_id, region_id) VALUES ($1, $2);

-- name: InsertStaffStoreScope :exec
INSERT INTO staff_scopes (staff_id, store_id) VALUES ($1, $2);

-- name: LiveRegionIDs :many
-- 给定的大区里哪些存在且未软删。建 / 改员工时拿它校验请求体里的 region_ids：
-- 外键只挡「不存在」，挡不住「已软删」。
SELECT r.id FROM regions r
 WHERE r.id = ANY(sqlc.arg(ids)::bigint[]) AND r.deleted_at IS NULL
 ORDER BY r.id;

-- name: LiveStoreRegions :many
-- 给定的门店里哪些存在且未软删，连同各自所属的大区。
-- 大区管理员给门店管理员分配门店时，要逐家核「这家店在不在我的大区里」；
-- 门店管理员看大区列表时，要知道自己那几家店在哪几个大区。
SELECT st.id, st.region_id FROM stores st
 WHERE st.id = ANY(sqlc.arg(ids)::bigint[]) AND st.deleted_at IS NULL
 ORDER BY st.id;

-- name: ListManagedStaff :many
-- 大区管理员看得见的员工：他自己，加上「只管本大区门店」的门店管理员。
--
-- 判据与 service.authorizeStaffWrite 里「这个门店管理员归不归我管」逐字一致：
-- role = 4、至少一家门店、**每一家**都在给定的大区里（软删的门店不算在内，
-- 与业务层只认未软删门店同一条口径）、没有大区范围。
-- 一个同时管着华北一家店与华东一家店的店长**不在**华北大区管理员的列表里 ——
-- 他的另一半归别人管，让华北那位改他的范围等于替华东做决定。
--
-- 单独一条而不是给 ListStaff 加一个可空参数：ListStaff 也在平台作用域里跑，
-- 而这里的子查询读 staff_scopes，那张表的策略在平台作用域里是 RAISE。
-- 「参数为空时 OR 的另一支不会执行」不是 PostgreSQL 承诺的事。
SELECT s.id, s.email, s.name, s.role, s.status, s.last_login_at, s.created_at
  FROM staff s
 WHERE s.deleted_at IS NULL
   AND (s.id = sqlc.arg(self_id)::bigint
        OR (s.role = 4
            AND EXISTS (SELECT 1 FROM staff_scopes a
                         JOIN stores st ON st.id = a.store_id AND st.deleted_at IS NULL
                        WHERE a.staff_id = s.id)
            AND NOT EXISTS (SELECT 1 FROM staff_scopes b
                             LEFT JOIN stores st ON st.id = b.store_id AND st.deleted_at IS NULL
                            WHERE b.staff_id = s.id
                              AND (b.region_id IS NOT NULL
                                   OR (st.id IS NOT NULL
                                       AND NOT st.region_id = ANY(sqlc.arg(region_ids)::bigint[]))))))
 ORDER BY s.id
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountManagedStaff :one
-- 条件必须与 ListManagedStaff 逐字一致。
SELECT count(*) FROM staff s
 WHERE s.deleted_at IS NULL
   AND (s.id = sqlc.arg(self_id)::bigint
        OR (s.role = 4
            AND EXISTS (SELECT 1 FROM staff_scopes a
                         JOIN stores st ON st.id = a.store_id AND st.deleted_at IS NULL
                        WHERE a.staff_id = s.id)
            AND NOT EXISTS (SELECT 1 FROM staff_scopes b
                             LEFT JOIN stores st ON st.id = b.store_id AND st.deleted_at IS NULL
                            WHERE b.staff_id = s.id
                              AND (b.region_id IS NOT NULL
                                   OR (st.id IS NOT NULL
                                       AND NOT st.region_id = ANY(sqlc.arg(region_ids)::bigint[]))))));
