-- 运费模板的查询：后台的增删改查、计价时取模板与规则、每一行商品的重量与挂的模板。
-- 数据模型 §7「运费模板」「运费怎么算」。
--
-- 全文没有一处 WHERE 写租户，也没有一处 INSERT 写 merchant_id：租户由 RLS 过滤，
-- 写入那一列由 DEFAULT current_merchant() 补（00055）。理由见 db/queries/products.sql
-- 与 scripts/check_query_tenancy.py 的文件头。
--
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。

-- ---------------------------------------------------------------------------
-- 计价
-- ---------------------------------------------------------------------------

-- name: ListSKUFreightInfo :many
-- 一批 SKU 的重量与所属商品单独挂的运费模板。试算、下单、购物车共用。
--
-- 与 ListSKUsForPricing 分成两条，而不是往那条里再加两列：那一条决定「可不可售、
-- 多少钱」，是营销活动与券共用的输入；运费只在它之后才用得上（计价顺序的最后一段），
-- 而且只要这两列。调用方只拿**已经定过价**的那些 sku_id 来问，
-- 所以这里不再重复可售性的四个条件。
SELECT s.id, s.weight_gram, p.freight_template_id
  FROM skus s
  JOIN products p ON p.id = s.product_id
 WHERE s.id = ANY(sqlc.arg(sku_ids)::bigint[]);

-- name: ListFreightTemplatesForPricing :many
-- 计价要用到的模板：商品单独挂的那几个、履约门店的门店模板、全店默认模板。
-- 一条查询取齐，由 service/freight.go 按「商品挂的 → 门店模板 → 全店默认」挑。
--
-- 商品挂的模板若已软删（删除接口会拒绝还有商品挂着的模板，所以正常不会发生），
-- 这里取不到它，那一行就按门店模板、全店默认往下落 —— 与「不单独挂」同一个结果。
SELECT id, name, store_id, charge_mode, is_default, undeliverable_region_codes
  FROM freight_templates
 WHERE deleted_at IS NULL
   AND (id = ANY(sqlc.arg(template_ids)::bigint[])
        OR store_id = sqlc.arg(store_id)
        OR (store_id IS NULL AND is_default));

-- name: ListFreightRules :many
-- 一批模板的全部规则。默认规则（region_codes 为空）排在最后，其余按录入顺序 ——
-- 后台编辑页与计价都按这个顺序读。
SELECT template_id, sort_order, region_codes, first_unit, first_fee_cents,
       additional_unit, additional_fee_cents, free_threshold_cents, free_quantity
  FROM freight_template_rules
 WHERE template_id = ANY(sqlc.arg(template_ids)::bigint[])
 ORDER BY template_id, (region_codes = '{}'), sort_order, id;

-- ---------------------------------------------------------------------------
-- 后台
-- ---------------------------------------------------------------------------

-- name: AdminListFreightTemplates :many
-- 后台模板列表，一页。product_count 是挂着它的未删除商品数（走 idx_products_freight_template）。
SELECT t.id, t.name, t.store_id, t.charge_mode, t.is_default, t.undeliverable_region_codes,
       t.created_at, t.updated_at,
       (SELECT count(*) FROM products p
         WHERE p.freight_template_id = t.id AND p.deleted_at IS NULL)::int AS product_count
  FROM freight_templates t
 WHERE t.deleted_at IS NULL
   AND (sqlc.narg(store_id)::bigint IS NULL OR t.store_id = sqlc.narg(store_id)::bigint)
 ORDER BY t.id DESC
 LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: AdminCountFreightTemplates :one
SELECT count(*)
  FROM freight_templates
 WHERE deleted_at IS NULL
   AND (sqlc.narg(store_id)::bigint IS NULL OR store_id = sqlc.narg(store_id)::bigint);

-- name: AdminGetFreightTemplate :one
-- 列与 AdminListFreightTemplates 逐一对齐，行类型可以直接互转。
SELECT t.id, t.name, t.store_id, t.charge_mode, t.is_default, t.undeliverable_region_codes,
       t.created_at, t.updated_at,
       (SELECT count(*) FROM products p
         WHERE p.freight_template_id = t.id AND p.deleted_at IS NULL)::int AS product_count
  FROM freight_templates t
 WHERE t.id = $1
   AND t.deleted_at IS NULL;

-- name: LockFreightTemplate :one
-- 改、删之前锁住模板行，拿到它此刻的归属。与 LockFreightTemplateForLink 的 FOR SHARE 互斥：
-- 「数挂着它的商品」与「给商品挂上它」在这一行锁上串行（00055 文件头第一节）。
SELECT id, store_id
  FROM freight_templates
 WHERE id = $1
   AND deleted_at IS NULL
   FOR UPDATE;

-- name: LockFreightTemplateForLink :one
-- 商品挂模板之前确认它是一个**未删除的全店模板**，并以共享锁钉住它直到事务结束 ——
-- 否则并发的一次「改成门店模板」或「删除」会在数完商品之后、这件商品挂上之前溜过去。
SELECT id
  FROM freight_templates
 WHERE id = $1
   AND deleted_at IS NULL
   AND store_id IS NULL
   FOR SHARE;

-- name: CountProductsOnFreightTemplate :one
SELECT count(*)
  FROM products
 WHERE freight_template_id = $1
   AND deleted_at IS NULL;

-- name: InsertFreightTemplate :one
INSERT INTO freight_templates (name, store_id, charge_mode, is_default, undeliverable_region_codes)
VALUES (sqlc.arg(name), sqlc.narg(store_id), sqlc.arg(charge_mode), sqlc.arg(is_default),
        sqlc.arg(undeliverable_region_codes)::text[])
RETURNING id;

-- name: UpdateFreightTemplate :execrows
UPDATE freight_templates
   SET name = sqlc.arg(name),
       store_id = sqlc.narg(store_id),
       charge_mode = sqlc.arg(charge_mode),
       is_default = sqlc.arg(is_default),
       undeliverable_region_codes = sqlc.arg(undeliverable_region_codes)::text[]
 WHERE id = sqlc.arg(id)
   AND deleted_at IS NULL;

-- name: ClearDefaultFreightTemplate :execrows
-- 设新的全店默认之前先取消旧的（uk_freight_templates_default 是部分唯一索引，
-- 反过来会自己撞自己 —— 与 uk_stores_default 同一个顺序）。except_id 是要设成默认的那一个。
UPDATE freight_templates
   SET is_default = FALSE
 WHERE is_default
   AND deleted_at IS NULL
   AND id <> sqlc.arg(except_id);

-- name: SoftDeleteFreightTemplate :execrows
UPDATE freight_templates
   SET deleted_at = now(), is_default = FALSE
 WHERE id = $1
   AND deleted_at IS NULL;

-- name: DeleteFreightRules :execrows
DELETE FROM freight_template_rules WHERE template_id = $1;

-- name: InsertFreightRule :exec
INSERT INTO freight_template_rules (template_id, sort_order, region_codes, first_unit,
                                    first_fee_cents, additional_unit, additional_fee_cents,
                                    free_threshold_cents, free_quantity)
VALUES (sqlc.arg(template_id), sqlc.arg(sort_order), sqlc.arg(region_codes)::text[],
        sqlc.arg(first_unit), sqlc.arg(first_fee_cents), sqlc.arg(additional_unit),
        sqlc.arg(additional_fee_cents), sqlc.arg(free_threshold_cents), sqlc.arg(free_quantity));
