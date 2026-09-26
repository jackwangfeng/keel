-- 后台类目的写路径（契约 /admin/categories*）。M4 Task 2。
--
-- 同样刻意不带 WHERE merchant_id，理由见 db/queries/products.sql 的文件头。
-- 注释里一个反引号都不许有（理由见 db/queries/inventories.sql 第三条）。

-- name: AdminListCategories :many
-- 扁平数组，按 path 升序 —— 后台管的是单个节点（改名、挪位置、停用），
-- 而树形结构在表格里还得再拍平一次。含停用（status = 0），不含软删。
SELECT id, parent_id, name, path, level, sort_order, status,
       deleted_at, created_at, updated_at
  FROM categories
 WHERE deleted_at IS NULL
 ORDER BY path;

-- name: GetCategory :one
-- 取一个未软删的类目。建 / 移动子树之前用它读父节点的 path 与 level ——
-- 读不到就是「父节点不存在或不属于当前租户」，契约要求 404。
SELECT id, parent_id, name, path, level, sort_order, status,
       deleted_at, created_at, updated_at
  FROM categories
 WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateCategoryRow :one
-- 建类目的**第一步**：先插出行拿到 id，path 暂时留空。
--
-- 为什么不是一条语句：path 形如 /1/23/456/，**含自己的 id**，而 id 是
-- GENERATED ALWAYS AS IDENTITY，插入之前不知道。想压进一条语句就要写成
-- 「WITH ins AS (INSERT ... RETURNING id) UPDATE categories SET path = ...
-- WHERE id = (SELECT id FROM ins)」—— 那是 PostgreSQL 里一条**静默无效**的语句：
-- 同一条语句里的各个数据修改 CTE 共享同一个快照，UPDATE 看不到 INSERT 刚插出来
-- 的行，于是它更新 0 行、不报任何错，而 path 永远停在空串上。
-- 空的 path 意味着 idx_categories_path 上的前缀查询对这一支整个失效。
--
-- 所以是两条语句，由 repository 的 CreateCategory 放在同一个事务里。
-- level 由调用方从父节点算出（根为 1），契约明写它不接受写入。
INSERT INTO categories (parent_id, name, path, level, sort_order)
VALUES (sqlc.narg(parent_id), sqlc.arg(name), '', sqlc.arg(level),
        sqlc.arg(sort_order))
RETURNING id;

-- name: SetCategoryPath :one
-- 建类目的第二步，见 CreateCategoryRow。
UPDATE categories SET path = $2
 WHERE id = $1
RETURNING id, parent_id, name, path, level, sort_order, status,
          deleted_at, created_at, updated_at;

-- name: UpdateCategory :one
-- 改类目的非结构字段。**parent_id 不在这里** —— 移动子树要连整棵子树的 path
-- 与 level 一起重写（MoveCategorySubtree），两者必须在同一个事务里，
-- 而把它混进这条 COALESCE 更新会让「只改了 parent_id、忘了重写 path」成为
-- 一句写得出来的代码。那种错不报警：path 是类目查询的主索引，子树的 path
-- 没跟着走，那些商品在新位置下查不出来，而在旧位置下还查得出来。
--
-- 与 UpdateProduct 同一个形状：两个 CTE、一个快照。
WITH cur AS (
    SELECT c.id FROM categories c
     WHERE c.id = sqlc.arg(id) AND c.deleted_at IS NULL
), upd AS (
    UPDATE categories u
       SET name       = COALESCE(sqlc.narg(name), u.name),
           sort_order = COALESCE(sqlc.narg(sort_order), u.sort_order),
           status     = COALESCE(sqlc.narg(status), u.status)
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
    RETURNING u.id, u.parent_id, u.name, u.path, u.level, u.sort_order,
              u.status, u.deleted_at, u.created_at, u.updated_at
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM upd) AS updated_rows,
       w.id, w.parent_id, w.name, w.path, w.level, w.sort_order,
       w.status, w.deleted_at, w.created_at, w.updated_at
  FROM (SELECT 1) anchor
  LEFT JOIN upd w ON true;

-- name: SetCategoryParent :execrows
-- 移动子树的第一步：改节点自身的 parent_id。第二步是 MoveCategorySubtree。
-- 两步必须在同一个事务里，理由见 UpdateCategory 的注释。
UPDATE categories SET parent_id = sqlc.narg(parent_id)
 WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: MoveCategorySubtree :execrows
-- 移动子树的第二步：把自己**以及全部后代**的 path 与 level 一起重写。
--
-- 判据全在 path 上（契约端点描述里的那条语句）：old_prefix 是这棵子树原来的
-- path，凡是以它打头的行都在这棵子树里。成环的检查也用 path ——
-- 「目标.path LIKE 自己.path || '%'」为真即成环 —— 但那一步在调用方做：
-- 让这条语句去判断会把「读两个节点」和「重写一片行」搅进一条 SQL，
-- 而重写这一片行必须在确定不成环之后才能发生。
--
-- 软删的后代也一起重写：它们没有恢复接口，但 path 留着错的更糟 ——
-- 哪天真做了恢复，恢复出来的是一棵挂在旧位置的孤儿子树。
UPDATE categories
   SET path  = sqlc.arg(new_prefix)::text
               || substring(path from length(sqlc.arg(old_prefix)::text) + 1),
       level = (level + sqlc.arg(level_delta)::int)::smallint
 WHERE path LIKE sqlc.arg(old_prefix)::text || '%';

-- name: CountLiveCategoryChildren :one
-- 删类目的闸门之一：还有未软删的子分类就不能删（409 category-has-children）。
-- 复合外键拦不住这件事 —— 软删不删行，父子引用在数据库看来一直是完整的。
SELECT count(*) FROM categories
 WHERE parent_id = $1 AND deleted_at IS NULL;

-- name: CountLiveCategoryProducts :one
-- 删类目的闸门之二：还有未软删的商品挂在它下面就不能删
-- （409 category-has-products）。products.category_id 是 NOT NULL 的复合外键，
-- 商品不可能「没有分类」；放行的话前台目录树里找不到这些商品，
-- 而它们仍在架、仍能被搜到、仍能下单。
SELECT count(*) FROM products
 WHERE category_id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteCategory :one
-- 置 deleted_at。两条 409 闸门由调用方在同一个事务里用上面两条 count 查过之后
-- 才走到这里，所以这条只需要把「不可见」与「已经删过了」分开 ——
-- 两者契约都是 404，但分开回传让上层的日志说得出是哪一种。
WITH cur AS (
    SELECT c.id, c.deleted_at FROM categories c WHERE c.id = sqlc.arg(id)
), del AS (
    UPDATE categories u SET deleted_at = now()
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
    RETURNING u.id
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM del) AS deleted_rows,
       c.deleted_at AS current_deleted_at
  FROM (SELECT 1) anchor
  LEFT JOIN cur c ON true;
