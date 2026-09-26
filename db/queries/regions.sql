-- 大区的 4 条后台接口（契约 /admin/regions*）。数据模型 §4。
--
-- 同样刻意不带 WHERE merchant_id，理由见 db/queries/products.sql 的文件头：
-- 租户由 RLS 在数据库层过滤，应用层再加一遍，「RLS 到底有没有生效」就变得
-- 测不出来了。注释里一个反引号都不许有（见 inventories.sql 文件头第三条）。

-- name: AdminListRegions :many
-- 大区列表。store_count 是名下**未软删**的门店数。
--
-- 它在列表里直接给出来，不是冗余：删大区时会因为它非零而被拒（409），
-- 而「点了删除才知道删不掉」是一次本可以省掉的往返。
--
-- 子查询而不是 LEFT JOIN + GROUP BY：一个大区名下门店数以「几个」计，
-- 而 GROUP BY 会把 regions 的每一列都拖进分组键，改一次 SELECT 列表就要
-- 改一次 GROUP BY —— 那是一处不会报错、只会算错的耦合。
SELECT r.id, r.code, r.name, r.status, r.deleted_at, r.created_at, r.updated_at,
       (SELECT count(*) FROM stores st
         WHERE st.region_id = r.id AND st.deleted_at IS NULL)::int AS store_count
  FROM regions r
 WHERE (sqlc.arg(include_deleted)::boolean OR r.deleted_at IS NULL)
 ORDER BY r.id
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: AdminCountRegions :one
-- 条件必须与 AdminListRegions 逐字一致，否则 total 与 items 各说各话。
SELECT count(*) FROM regions r
 WHERE (sqlc.arg(include_deleted)::boolean OR r.deleted_at IS NULL);

-- name: AdminGetRegion :one
-- 单个大区。**含软删的**：contract 的 PATCH / DELETE 对软删的大区回 404，
-- 而那个判断在 repository 里做 —— 这里把 deleted_at 原样返回，
-- 让「不存在」与「已软删」在上一层还分得开。
SELECT r.id, r.code, r.name, r.status, r.deleted_at, r.created_at, r.updated_at,
       (SELECT count(*) FROM stores st
         WHERE st.region_id = r.id AND st.deleted_at IS NULL)::int AS store_count
  FROM regions r
 WHERE r.id = $1;

-- name: CreateRegion :one
-- 建大区。code 撞了会以 23505（uk_regions_code）失败，由 repository 翻成契约的
-- 409 region-code-conflict。不在这里先查一遍：先查后建之间的窗口里另一个会话
-- 可以把同一个 code 插进去，而唯一索引是唯一真正能挡住它的东西。
INSERT INTO regions (code, name)
VALUES (sqlc.arg(code), sqlc.arg(name))
RETURNING id, code, name, status, deleted_at, created_at, updated_at;

-- name: UpdateRegion :one
-- 部分更新。与 UpdateSKU 同一个形状：两个 CTE、一个快照，把「不可见」与
-- 「没改成」分开回传 —— 拆成「先 SELECT 确认可见、再 UPDATE」是两次快照。
WITH cur AS (
    SELECT r.id FROM regions r WHERE r.id = sqlc.arg(id) AND r.deleted_at IS NULL
), upd AS (
    UPDATE regions u
       SET code   = COALESCE(sqlc.narg(code), u.code),
           name   = COALESCE(sqlc.narg(name), u.name),
           status = COALESCE(sqlc.narg(status), u.status)
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
    RETURNING u.id, u.code, u.name, u.status, u.deleted_at, u.created_at, u.updated_at
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM upd) AS updated_rows,
       w.id, w.code, w.name, w.status, w.deleted_at, w.created_at, w.updated_at
  FROM (SELECT 1) anchor
  LEFT JOIN upd w ON true;

-- name: SoftDeleteRegion :one
-- 软删。名下还有未软删的门店时拒绝（契约的 409 region-has-stores）。
--
-- **不做级联**：级联软删一个大区会连带让它下面所有门店接不到单，
-- 而调用方在点下删除时看到的只是一个大区名。
--
-- 闸门写进 UPDATE 的 WHERE，不是先查后改：两次快照之间另一个会话可以建一家店
-- 进来，于是「删的时候一家都没有」在提交时变成了「删完还挂着一家」。
-- 回传 store_rows 让调用方把 updated_rows = 0 分成 404 与 409 两支。
WITH cur AS (
    SELECT r.id FROM regions r WHERE r.id = sqlc.arg(id) AND r.deleted_at IS NULL
), n AS (
    SELECT count(*) AS cnt FROM stores st
     WHERE st.region_id = sqlc.arg(id) AND st.deleted_at IS NULL
), del AS (
    UPDATE regions u
       SET deleted_at = now()
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
       AND (SELECT cnt FROM n) = 0
    RETURNING u.id
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM del) AS deleted_rows,
       (SELECT cnt FROM n)        AS store_rows;
