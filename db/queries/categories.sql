-- 买家侧的类目（契约 GET /categories）。后台的那一组在 admin_categories.sql。
--
-- 同样刻意不带 WHERE merchant_id，理由见 db/queries/products.sql 的文件头。
-- 注释里一个反引号都不许有（理由见 db/queries/inventories.sql 第三条）。

-- name: ListVisibleCategories :many
-- 契约原话：只返回 status = 1 启用、且未软删的分类。后台要看停用的那些走
-- GET /admin/categories（扁平，含停用）。
--
-- 返回的是**扁平**的，树在 service 里拼（ProductService.Categories）。
-- 在 SQL 里拼树要么递归 CTE 要么 jsonb_agg 套娃，两者都把「父节点停用了，
-- 子节点怎么办」这条产品规则埋进一段难读的 SQL；而在 Go 里它就是
-- 「只挂到在场的父节点上」一句话，有单测盯着。
--
-- 排序：同一个父节点下按 sort_order，平手按 id。按 level 打头是为了让拼树时
-- 父节点一定先于子节点出现 —— 一趟就能挂完，不用先建索引再回头补。
SELECT id, parent_id, name, level, sort_order
  FROM categories
 WHERE deleted_at IS NULL
   AND status = 1
 ORDER BY level, sort_order, id;
