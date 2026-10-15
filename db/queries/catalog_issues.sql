-- 上新整理（catalog_issues，docs/superpowers/specs/2026-10-16-ai-ops-onboarding-restock-coupon-design.md 第一节）：
-- 逐件商品体检要的两条只读查询。判据在 service/catalog_issues.go，这里只取数。
--
-- 与这个目录里别的文件一样，一个 merchant_id 都没有：租户由 RLS 过滤。
-- 注释里不许有反引号，理由见 db/queries/inventories.sql 的第三条说明。

-- name: CatalogIssueProducts :many
-- 未删除、状态在给定集合里的商品，带类目名与商品图张数。按 id 升序，limit 由调用方给。
SELECT p.id, p.title, p.subtitle, p.description, p.status, p.category_id,
       coalesce(c.name, '')::text AS category_name,
       (SELECT count(*) FROM product_images i WHERE i.product_id = p.id)::bigint AS image_count
  FROM products p
  LEFT JOIN categories c ON c.id = p.category_id AND c.deleted_at IS NULL
 WHERE p.deleted_at IS NULL
   AND p.status = ANY(sqlc.arg(statuses)::smallint[])
 ORDER BY p.id
 LIMIT sqlc.arg(row_limit);

-- name: CatalogIssueSKUs :many
-- 全店未删除商品的未删除 SKU 价格，带所属类目：价格异常要拿同类目的中位数比，
-- 所以取的是全店，不只是这一批被体检的商品。
SELECT s.id, s.product_id, s.price_cents, p.category_id
  FROM skus s
  JOIN products p ON p.id = s.product_id AND p.deleted_at IS NULL
 WHERE s.deleted_at IS NULL;

-- name: CatalogLeafCategories :many
-- 启用中的叶子类目（没有启用中的子类目），带从根到自己的名字路径：AI 员工挪类目时从这里挑。
-- 路径由 path（形如 /1/23/456/）里的 id 依次换成名字拼出来。
SELECT c.id,
       (SELECT string_agg(a.name, ' > ' ORDER BY a.level)
          FROM categories a
         WHERE a.deleted_at IS NULL
           AND c.path LIKE a.path || '%')::text AS path_name
  FROM categories c
 WHERE c.deleted_at IS NULL AND c.status = 1
   AND NOT EXISTS (SELECT 1 FROM categories k
                    WHERE k.parent_id = c.id AND k.deleted_at IS NULL AND k.status = 1)
 ORDER BY path_name;
