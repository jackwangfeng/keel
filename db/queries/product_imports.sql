-- 商品批量导入（契约 /admin/product-imports，数据模型 §3 product_import_batches）。
--
-- 同样刻意不带 WHERE merchant_id，理由见 db/queries/products.sql 的文件头。
-- 注释里一个反引号都不许有（理由见 db/queries/inventories.sql 第三条）。

-- name: ClaimProductImport :one
-- 确认导入事务的第一句：占住「这份文件在这家店导入过」这件事。
--
-- 返回一行 = 占到了，接着建商品；没有返回行（pgx.ErrNoRows）= 同一份文件已经导入过
-- （或者正被另一个事务导入 —— 那时这条语句会在唯一索引上等它结束），
-- 调用方转去读那一行的回执。
--
-- ON CONFLICT 不写冲突目标，理由同 db/queries/jobs.sql 的 EnqueueJob：写出来就得提
-- merchant_id，而这张表上能撞的唯一约束只有 uk_product_import_batches_sha
-- （主键是 GENERATED ALWAYS AS IDENTITY，撞不了），两种写法等价。
INSERT INTO product_import_batches (file_sha256, file_name, file_format, staff_id, total_rows)
VALUES (sqlc.arg(file_sha256), sqlc.arg(file_name), sqlc.arg(file_format),
        sqlc.arg(staff_id), sqlc.arg(total_rows))
ON CONFLICT DO NOTHING
RETURNING id, created_at;

-- name: FinishProductImport :exec
-- 同一个事务的最后一句：写回执与计数。
UPDATE product_import_batches
   SET created_products = sqlc.arg(created_products),
       created_skus     = sqlc.arg(created_skus),
       failed_rows      = sqlc.arg(failed_rows),
       result           = sqlc.arg(result)
 WHERE id = sqlc.arg(id);

-- name: FindProductImportBySHA :one
-- 这份文件在本店导入过没有（RLS 收窄到本店）。预检用它提示「已经导入过」，
-- 确认导入在 ClaimProductImport 没占到时用它取回那一次的回执。
SELECT id, file_sha256, total_rows, created_products, created_skus, failed_rows,
       result, created_at
  FROM product_import_batches
 WHERE file_sha256 = sqlc.arg(file_sha256);

-- name: ExistingSKUCodes :many
-- 给定的编码里哪些在本店已经被占用。**含已软删的 SKU**：uk_skus_code 不是部分索引，
-- 软删的 SKU 仍然占着它的编码 —— 这里漏掉它们的话，预检说「可以导」，
-- 确认时整批在唯一索引上回滚。
SELECT sku_code
  FROM skus
 WHERE sku_code = ANY(sqlc.arg(codes)::text[]);
