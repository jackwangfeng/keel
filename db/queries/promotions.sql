-- 营销活动（数据模型 §7「营销活动」，迁移 00058）。
--
-- 与 coupons.sql 同一个分工：这里**只取素材、只做条件更新**，不判定「这个活动在这一单上
-- 减多少、哪几行参与」。那个判定只有一份实现（service/promotion_calc.go），
-- 试算、下单、购物车、商品标签四处都经过它。
--
-- 全文没有一处 WHERE 写租户，也没有一处 INSERT 写那一列（RLS 过滤，DEFAULT 补）。
-- 注释里一个反引号都不许有（inventories.sql 第三条说明）。

-- ---------------------------------------------------------------------------
-- 计价的素材：此刻生效的活动
-- ---------------------------------------------------------------------------

-- name: ListLivePromotions :many
-- 此刻生效（上线、且 starts_at <= now < ends_at）的活动，不含单价类的活动商品明细。
-- 满减满折的阶梯、全部活动的范围另两条查询按 id 批量取。
-- 「此刻」由调用方传入，不用 now()：试算与下单共用 service 那一侧的时钟来源，
-- 测试也能构造「刚过期」的场景。
SELECT id, name, promo_type, threshold_unit, stack_with_coupon, gift_template_id,
       starts_at, ends_at
  FROM promotions
 WHERE status = 1
   AND starts_at <= sqlc.arg(now)
   AND ends_at > sqlc.arg(now)
 ORDER BY id;

-- name: ListPromotionTiers :many
SELECT promotion_id, threshold, discount_cents, discount_rate
  FROM promotion_tiers
 WHERE promotion_id = ANY(sqlc.arg(promotion_ids)::bigint[])
 ORDER BY promotion_id, threshold;

-- name: ListPromotionScopes :many
-- 与 coupons.sql 的 ListCouponScopes 同一个形状、同一段 LEFT JOIN：范围语义只有一套。
SELECT ps.promotion_id, ps.scope_type, ps.target_id, ps.include,
       c.path AS category_path,
       c.name AS category_name, p.title AS product_title, r.name AS region_name,
       st.name AS store_name
  FROM promotion_scopes ps
  LEFT JOIN categories c ON ps.scope_type = 2 AND c.id = ps.target_id AND c.deleted_at IS NULL
  LEFT JOIN products   p ON ps.scope_type = 3 AND p.id = ps.target_id AND p.deleted_at IS NULL
  LEFT JOIN regions    r ON ps.scope_type = 5 AND r.id = ps.target_id AND r.deleted_at IS NULL
  LEFT JOIN stores    st ON ps.scope_type = 6 AND st.id = ps.target_id AND st.deleted_at IS NULL
 WHERE ps.promotion_id = ANY(sqlc.arg(promotion_ids)::bigint[])
 ORDER BY ps.promotion_id, ps.id;

-- name: ListLivePriceOffers :many
-- 一批 SKU 身上此刻生效的限时折扣 / 秒杀。一个 SKU 可以同时在几个活动里，
-- 取哪一个（价低者）是 service 的事。
SELECT ps.promotion_id, ps.sku_id, ps.promo_price_cents, ps.discount_rate,
       ps.per_user_limit, ps.stock_qty, ps.sold_qty
  FROM promotion_skus ps
  JOIN promotions pr ON pr.id = ps.promotion_id
 WHERE ps.sku_id = ANY(sqlc.arg(sku_ids)::bigint[])
   AND pr.status = 1
   AND pr.promo_type IN (3, 4)
   AND pr.starts_at <= sqlc.arg(now)
   AND pr.ends_at > sqlc.arg(now)
 ORDER BY ps.sku_id, ps.promotion_id;

-- name: ListLivePriceOffersForProducts :many
-- 商品列表 / 详情的活动标签：一批商品的全部 SKU 身上此刻生效的单价类活动，
-- 连同这家门店的生效价（活动价 = min(门店价, 特价)，折扣类的特价要按门店价算）。
-- 门店价从 sku_prices_by_store 取 —— 三层定价只许经由视图读（check_query_tenancy.py 第二条）。
SELECT ps.promotion_id, ps.sku_id, s.product_id, v.price_cents AS store_price_cents,
       ps.promo_price_cents, ps.discount_rate, ps.per_user_limit, ps.stock_qty, ps.sold_qty
  FROM promotion_skus ps
  JOIN promotions pr ON pr.id = ps.promotion_id
  JOIN skus s ON s.id = ps.sku_id AND s.status = 1 AND s.deleted_at IS NULL
  JOIN sku_prices_by_store v ON v.sku_id = s.id AND v.store_id = sqlc.arg(store_id)
 WHERE s.product_id = ANY(sqlc.arg(product_ids)::bigint[])
   AND pr.status = 1
   AND pr.promo_type IN (3, 4)
   AND pr.starts_at <= sqlc.arg(now)
   AND pr.ends_at > sqlc.arg(now)
 ORDER BY s.product_id, ps.sku_id, ps.promotion_id;

-- name: ListProductPromotionFacts :many
-- 满减满折的范围按商品 / 分类（含子孙）/ 品牌挑行，标签也要按同一套规则判
-- 「这件商品参不参与」—— 素材与 ListSKUsForPricing 取的那两列同源。
SELECT p.id, p.brand_id, c.path AS category_path
  FROM products p
  LEFT JOIN categories c ON c.id = p.category_id AND c.deleted_at IS NULL
 WHERE p.id = ANY(sqlc.arg(product_ids)::bigint[]);

-- name: ListUserPromotionPurchases :many
-- 这个买家在这些活动里各 SKU 已经买了几件（每人限购的判据，只计未关闭的订单）。
SELECT promotion_id, sku_id, qty
  FROM promotion_purchases
 WHERE user_id = sqlc.arg(user_id)
   AND promotion_id = ANY(sqlc.arg(promotion_ids)::bigint[]);

-- ---------------------------------------------------------------------------
-- 下单 SAGA 的库存分支：活动配额与每人限购（与门店库存扣减同一个事务）
-- ---------------------------------------------------------------------------

-- name: ReservePromotionSku :one
-- 秒杀不超卖的那一条（00058 文件头）：条件 UPDATE，READ COMMITTED 下后到者在最新版本上
-- 重评 WHERE。stock_qty = 0 表示不限配额（限时折扣），此时只是累计已售件数。
-- 受影响 0 行即配额不够（或这个 SKU 已被移出活动）—— :one 变成 pgx.ErrNoRows。
--
-- 不看活动的上下线与有效期：价格在建单那一刻已经定下、写进了订单行，
-- 下单几百毫秒之后活动恰好下线，按已经报给买家的价成交才是对的。
UPDATE promotion_skus
   SET sold_qty = sold_qty + sqlc.arg(qty)::int
 WHERE promotion_id = sqlc.arg(promotion_id)
   AND sku_id = sqlc.arg(sku_id)
   AND (stock_qty = 0 OR sold_qty + sqlc.arg(qty)::int <= stock_qty)
RETURNING per_user_limit;

-- name: AddPromotionPurchase :execrows
-- 每人限购：在 ReservePromotionSku 拿到的那把行锁之下累计。
-- 首次（INSERT 分支）由 SELECT ... WHERE 判「这一单本身就超了没有」；
-- 再次（冲突分支）由 DO UPDATE ... WHERE 判「累计超了没有」。受影响 0 行即超限。
INSERT INTO promotion_purchases (promotion_id, sku_id, user_id, qty)
SELECT sqlc.arg(promotion_id), sqlc.arg(sku_id), sqlc.arg(user_id), sqlc.arg(qty)::int
 WHERE sqlc.arg(qty)::int <= sqlc.arg(per_user_limit)::int
    ON CONFLICT ON CONSTRAINT pk_promotion_purchases
    DO UPDATE SET qty = promotion_purchases.qty + EXCLUDED.qty
 WHERE promotion_purchases.qty + EXCLUDED.qty <= sqlc.arg(per_user_limit)::int;

-- name: ReleasePromotionSku :execrows
-- SAGA 补偿、超时关单、买家取消：把这一行占的配额放回去。
-- 带 sold_qty >= qty：放回不该把计数放成负数；0 行由调用方记日志（SKU 已被移出活动之类）。
UPDATE promotion_skus
   SET sold_qty = sold_qty - sqlc.arg(qty)::int
 WHERE promotion_id = sqlc.arg(promotion_id)
   AND sku_id = sqlc.arg(sku_id)
   AND sold_qty >= sqlc.arg(qty)::int;

-- name: ReleasePromotionPurchase :execrows
UPDATE promotion_purchases
   SET qty = qty - sqlc.arg(qty)::int
 WHERE promotion_id = sqlc.arg(promotion_id)
   AND sku_id = sqlc.arg(sku_id)
   AND user_id = sqlc.arg(user_id)
   AND qty >= sqlc.arg(qty)::int;

-- ---------------------------------------------------------------------------
-- 新人礼
-- ---------------------------------------------------------------------------

-- name: UserHasPlacedOrder :one
-- 「首单前」的判据：这个买家有没有一笔进过 SAGA 且没被关掉的订单。
-- 创建中（0）与已关闭（90）不算 —— 下单失败、超时没付的单子不让人失去新人资格。
SELECT EXISTS (
    SELECT 1 FROM orders WHERE user_id = sqlc.arg(user_id) AND status NOT IN (0, 90)
);

-- name: HasGiftGrant :one
SELECT EXISTS (
    SELECT 1 FROM promotion_gift_grants
     WHERE promotion_id = sqlc.arg(promotion_id) AND user_id = sqlc.arg(user_id)
);

-- name: InsertGiftGrant :execrows
-- 主键（活动 × 买家）是「一人一张」的最终仲裁：两次并发登录都走到这里，只有一次插得进去，
-- 另一次受影响 0 行，调用方回滚整个事务（连同它刚占的名额与刚落的券）。
INSERT INTO promotion_gift_grants (promotion_id, user_id, user_coupon_id)
VALUES (sqlc.arg(promotion_id), sqlc.arg(user_id), sqlc.arg(user_coupon_id))
ON CONFLICT DO NOTHING;

-- name: CountGiftGrants :many
SELECT promotion_id, count(*)::int AS granted
  FROM promotion_gift_grants
 WHERE promotion_id = ANY(sqlc.arg(promotion_ids)::bigint[])
 GROUP BY promotion_id;

-- ---------------------------------------------------------------------------
-- 后台
-- ---------------------------------------------------------------------------

-- name: AdminListPromotions :many
SELECT id, name, promo_type, threshold_unit, stack_with_coupon, gift_template_id,
       starts_at, ends_at, status, created_at, updated_at
  FROM promotions
 WHERE (sqlc.narg(status)::smallint IS NULL OR status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(promo_type)::smallint IS NULL OR promo_type = sqlc.narg(promo_type)::smallint)
 ORDER BY id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: AdminCountPromotions :one
SELECT count(*)
  FROM promotions
 WHERE (sqlc.narg(status)::smallint IS NULL OR status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(promo_type)::smallint IS NULL OR promo_type = sqlc.narg(promo_type)::smallint);

-- name: AdminGetPromotion :one
SELECT id, name, promo_type, threshold_unit, stack_with_coupon, gift_template_id,
       starts_at, ends_at, status, created_at, updated_at
  FROM promotions
 WHERE id = $1;

-- name: AdminLockPromotion :one
-- 修改之前先锁住活动行：「上线中不许改规则」的判定与随后的整组替换必须看到同一个 status。
SELECT id, status
  FROM promotions
 WHERE id = $1
   FOR UPDATE;

-- name: AdminCreatePromotion :one
INSERT INTO promotions (name, promo_type, threshold_unit, stack_with_coupon, gift_template_id,
                        starts_at, ends_at, status)
VALUES (sqlc.arg(name), sqlc.arg(promo_type), sqlc.arg(threshold_unit),
        sqlc.arg(stack_with_coupon), sqlc.narg(gift_template_id),
        sqlc.arg(starts_at), sqlc.arg(ends_at), 0)
RETURNING id;

-- name: AdminUpdatePromotion :execrows
-- 整行写回（service 已经合并过 PATCH）。promo_type 不在这里：类型建了就不改，
-- 改类型等于换一个活动，而子表（阶梯 / 活动商品）的形状会跟着整个变掉。
UPDATE promotions
   SET name = sqlc.arg(name),
       threshold_unit = sqlc.arg(threshold_unit),
       stack_with_coupon = sqlc.arg(stack_with_coupon),
       gift_template_id = sqlc.narg(gift_template_id),
       starts_at = sqlc.arg(starts_at),
       ends_at = sqlc.arg(ends_at),
       status = sqlc.arg(status)
 WHERE id = sqlc.arg(id);

-- name: DeletePromotionTiers :exec
DELETE FROM promotion_tiers WHERE promotion_id = $1;

-- name: InsertPromotionTier :exec
INSERT INTO promotion_tiers (promotion_id, threshold, discount_cents, discount_rate)
VALUES (sqlc.arg(promotion_id), sqlc.arg(threshold), sqlc.arg(discount_cents),
        sqlc.arg(discount_rate));

-- name: DeletePromotionScopes :exec
DELETE FROM promotion_scopes WHERE promotion_id = $1;

-- name: InsertPromotionScope :exec
INSERT INTO promotion_scopes (promotion_id, scope_type, target_id, include)
VALUES (sqlc.arg(promotion_id), sqlc.arg(scope_type), sqlc.narg(target_id), sqlc.arg(include));

-- name: ListPromotionSkusAdmin :many
-- 后台看活动商品：连同标题与 SKU 编码（展示用），以及已售件数。
SELECT ps.promotion_id, ps.sku_id, ps.promo_price_cents, ps.discount_rate,
       ps.per_user_limit, ps.stock_qty, ps.sold_qty, s.sku_code, p.title
  FROM promotion_skus ps
  JOIN skus s ON s.id = ps.sku_id
  JOIN products p ON p.id = s.product_id
 WHERE ps.promotion_id = ANY(sqlc.arg(promotion_ids)::bigint[])
 ORDER BY ps.promotion_id, ps.sku_id;

-- name: UpsertPromotionSku :exec
-- 整组替换活动商品时逐条 upsert：保留下来的 SKU 保留它的 sold_qty（已兑现的配额不清零）。
-- chk_promotion_sku_qty 兜底「配额改到低于已售数」。
INSERT INTO promotion_skus (promotion_id, sku_id, promo_price_cents, discount_rate,
                            per_user_limit, stock_qty)
VALUES (sqlc.arg(promotion_id), sqlc.arg(sku_id), sqlc.arg(promo_price_cents),
        sqlc.arg(discount_rate), sqlc.arg(per_user_limit), sqlc.arg(stock_qty))
    ON CONFLICT ON CONSTRAINT uk_promotion_skus
    DO UPDATE SET promo_price_cents = EXCLUDED.promo_price_cents,
                  discount_rate = EXCLUDED.discount_rate,
                  per_user_limit = EXCLUDED.per_user_limit,
                  stock_qty = EXCLUDED.stock_qty;

-- name: DeletePromotionSkusExcept :exec
-- 删掉不在新名单里的 SKU。已售出过的（sold_qty > 0）不删：service 已经先拒过一次，
-- 这里的条件是第二道 —— 两道的失效方式不一样。
DELETE FROM promotion_skus
 WHERE promotion_id = sqlc.arg(promotion_id)
   AND sku_id <> ALL(sqlc.arg(keep_sku_ids)::bigint[])
   AND sold_qty = 0;

-- name: LiveSkuIDs :many
-- 活动商品的归属校验：在本租户存在且未软删的 SKU（RLS 之下，别家的与不存在的同形）。
SELECT id FROM skus WHERE id = ANY(sqlc.arg(ids)::bigint[]) AND deleted_at IS NULL;

-- name: LiveCouponTemplateIDs :many
-- 新人礼的券模板归属校验。
SELECT id FROM coupon_templates WHERE id = ANY(sqlc.arg(ids)::bigint[]);
