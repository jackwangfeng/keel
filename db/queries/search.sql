-- 混合检索的两路召回（M3 Task 4）。向量一路、bigram 关键词一路，
-- RRF 融合在应用层（语义检索层 §4），不在 SQL 里。
--
-- 和这个目录里别的文件一样，**一个 merchant_id 都没有** —— 租户由 RLS 在
-- 数据库层过滤。理由见 scripts/check_query_tenancy.py 的文件头。
--
-- 两条查询的**过滤条件必须逐字一致**（deleted_at / status / 三个 filters）。
-- 不一致的后果不是报错，是一件商品在一路里可见、在另一路里不可见 ——
-- 而 RRF 只看名次，它拿到的是两份对「哪些商品存在」意见不同的列表，
-- 融合出来的排序没有意义，且没有任何东西会红。
--
-- **M4 Task 3 起价格区间是现算的**（00019 删掉了 products 上那两列冗余价格），
-- **本轮（00020）它又跟着门店走了**：取价口从 skus.price_cents 换成
-- sku_prices_by_store 视图，检索结果里的价格区间从此是「按当前门店算出来的」。
-- 这条纪律因此多了一项：那个 LEFT JOIN LATERAL 与两处 COALESCE 也必须逐字
-- 一致 —— 价格过滤现在读的是它算出来的值，两边的公式写岔一个字，
-- 同一件商品就会在一路里落进价格区间、在另一路里落在外面。
--
-- 逐字一致这条纪律本轮又多了一项：两层可见性排除（region / store 各一条
-- NOT EXISTS）。
--
-- **in_stock 与 in_stock_only 不在这两条里了**（微服务拆分阶段 1a）：inventories 归库存
-- 服务，召回查询不再 JOIN 它。service/search.go 在两路召回之后，用 ListOnSaleSKUsOfProducts
-- 取候选商品的在售 SKU、向库存服务按**这家门店**批量问一次水位，在 Go 里算 in_stock
-- （任意一个在售 SKU 水位 > 0，判据不变）；in_stock_only 从 SQL 里的过滤变成召回之后的
-- 过滤 —— 代价是一次检索返回的条数可能少于 size（召回窗口里缺货的多时），写在那个文件里。
--
-- COALESCE 到 0 不是随手写的：旧列 min/max_price_cents 的 DEFAULT 是 0，
-- 于是「一个 SKU 都没有」的商品在旧的过滤里表现为 max=0（被 min_price 筛掉）
-- 与 min=0（**通过** max_price 筛选）。裸的 max()/min() 在那种情况下是 NULL，
-- 而 NULL 两个方向都不通过 —— 那就不是「换了个算法」，是悄悄改了一条筛选规则。
-- 留着 COALESCE，现算与旧列在全部取值上逐点相同。
--
-- 两条都带同一个取主图的 LEFT JOIN LATERAL（main_image_upload_id，可空），
-- 写法与 products.sql 的 ListProducts 逐字一致：sort_order 最小、id 次之的那一张。
-- 没有图时是 0 而不是 NULL（sqlc 推不出那一侧可空，理由写在 ListProducts 上）。
-- 它不参与任何过滤，写岔了不会让结果集变样，但会让同一件商品在列表与检索里
-- 顶着两张不同的封面；更隐蔽的是检索内部：两路都命中时 service 合并留的是
-- 关键词路那一行，只命中向量路时留的是向量路那一行（service/search.go 的 byID）
-- —— 两路写岔，同一件商品的封面就取决于这次是哪一路捞到了它。

-- name: SearchProductsByVector :many
-- 向量召回。余弦距离，配 idx_ptv_hnsw（vector_cosine_ops）。
--
-- ## 这条查询就是语义检索层 §2.4 里那个「❌ 危险写法」，一字不差
--
-- 它 JOIN products、带类目与价格过滤、ORDER BY 距离 LIMIT N。文档说规划器
-- 可能先取向量最近的 N 条再过滤，类目稀疏时返回远少于 N 条甚至为空；
-- 而在这个仓库里过滤条件还多一条**看不见的** RLS 谓词
-- 「按商家过滤」那一条（00016 文件头第三节）。
--
-- **不在这里躲它。** 躲的写法是先在 product_text_vectors 上单独召回一批
-- product_id、再在应用层过滤 —— 那样 filters 就变成了纯粹的 post-filter，
-- 用户一加价格区间结果就会少一大截，而且少多少应用自己也不知道。
-- 把条件推进来、让规划器自己决定是先过滤还是先走索引，是**更好**的形状：
-- 小租户它会选精确的顺序扫描（实测 300 件时 13 ms、100% 准），
-- 大租户它会选 HNSW，而 HNSW 被选中时的正确性由
-- repository.withTenantTx 设的那三个 hnsw.* GUC 兜住。完整实测见那里。
--
-- in_stock 在 service 里算（见文件头），判据是「任意一个在售 SKU 水位 > 0」，与
-- ProductDetail.InStock 同一个（service/product.go）—— 不读汇总列：曾经的
-- products.total_stock 没有任何一处在维护（已停用，见 00062）。
SELECT p.id, p.title, p.subtitle,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       p.sales_count, p.status,
       COALESCE(img.upload_id, 0)::bigint AS main_image_upload_id,
       (v.embedding <=> @query_embedding::vector)::float8 AS distance
  FROM product_text_vectors v
  JOIN products p ON p.id = v.product_id
  LEFT JOIN LATERAL (
        SELECT min(pv.price_cents) AS min_price, max(pv.price_cents) AS max_price
          FROM sku_prices_by_store pv
         WHERE pv.store_id = sqlc.arg(store_id) AND pv.product_id = p.id
       ) agg ON TRUE
  LEFT JOIN LATERAL (
        SELECT pi.upload_id
          FROM product_images pi
         WHERE pi.product_id = p.id
         ORDER BY pi.sort_order, pi.id
         LIMIT 1
       ) img ON TRUE
 WHERE p.deleted_at IS NULL
   AND p.status = 1
   AND (sqlc.narg(category_id)::bigint IS NULL
        OR p.category_id = sqlc.narg(category_id)::bigint)
   AND (sqlc.narg(min_price_cents)::bigint IS NULL
        OR COALESCE(agg.max_price, 0) >= sqlc.narg(min_price_cents)::bigint)
   AND (sqlc.narg(max_price_cents)::bigint IS NULL
        OR COALESCE(agg.min_price, 0) <= sqlc.narg(max_price_cents)::bigint)
   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
                    WHERE ro.region_id = sqlc.arg(region_id)
                      AND ro.product_id = p.id AND ro.status = 0)
   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
                    WHERE so.store_id = sqlc.arg(store_id)
                      AND so.product_id = p.id AND so.status = 0)
 ORDER BY v.embedding <=> @query_embedding::vector
 LIMIT @row_limit;

-- name: SearchProductsByKeyword :many
-- bigram 关键词召回（语义检索层 §3）。
--
-- @tsquery 是**应用层**切出来的 tsquery 串，由 internal/search.Bigram 的输出
-- 用竖线拼成（internal/search.TSQueryOr）。这里不做任何分词：
-- to_tsquery('simple', '连衣裙') 会把整句
-- 中文当成一个 token，那正是 §3 开头说的「完全不可用」。
--
-- 为什么 tsquery 是拼出来的字符串而不是绑定参数拼接的隐患：Bigram 的输出
-- 只含字母、数字与表意文字，用空格分隔（其余字符一律当分隔符丢掉），
-- tsquery 的元字符（& | ! ( ) : * ' <->）一个都活不下来。
-- internal/search 里有一条测试专门钉这条性质。
--
-- 排序用 ts_rank_cd 而不是 ts_rank：cd 版按**覆盖密度**算，命中的二元组挨得近
-- 的排前面 —— 「连衣 衣裙」连着出现的商品，比一个只在标题头、一个只在副标题尾
-- 各命中一次的商品更像是真的在说那个词。
--
-- 过滤条件与上面那条逐字一致，理由写在文件头。
SELECT p.id, p.title, p.subtitle,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       p.sales_count, p.status,
       COALESCE(img.upload_id, 0)::bigint AS main_image_upload_id,
       ts_rank_cd(p.search_vector, to_tsquery('simple', @tsquery::text))::float8 AS rank
  FROM products p
  LEFT JOIN LATERAL (
        SELECT min(pv.price_cents) AS min_price, max(pv.price_cents) AS max_price
          FROM sku_prices_by_store pv
         WHERE pv.store_id = sqlc.arg(store_id) AND pv.product_id = p.id
       ) agg ON TRUE
  LEFT JOIN LATERAL (
        SELECT pi.upload_id
          FROM product_images pi
         WHERE pi.product_id = p.id
         ORDER BY pi.sort_order, pi.id
         LIMIT 1
       ) img ON TRUE
 WHERE p.deleted_at IS NULL
   AND p.status = 1
   AND p.search_vector @@ to_tsquery('simple', @tsquery::text)
   AND (sqlc.narg(category_id)::bigint IS NULL
        OR p.category_id = sqlc.narg(category_id)::bigint)
   AND (sqlc.narg(min_price_cents)::bigint IS NULL
        OR COALESCE(agg.max_price, 0) >= sqlc.narg(min_price_cents)::bigint)
   AND (sqlc.narg(max_price_cents)::bigint IS NULL
        OR COALESCE(agg.min_price, 0) <= sqlc.narg(max_price_cents)::bigint)
   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
                    WHERE ro.region_id = sqlc.arg(region_id)
                      AND ro.product_id = p.id AND ro.status = 0)
   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
                    WHERE so.store_id = sqlc.arg(store_id)
                      AND so.product_id = p.id AND so.status = 0)
 ORDER BY ts_rank_cd(p.search_vector, to_tsquery('simple', @tsquery::text)) DESC, p.id
 LIMIT @row_limit;

-- name: ListOnSaleSKUsOfProducts :many
-- 一批商品的在售 SKU（未软删、status = 1）。检索算 in_stock 用：拿这批 sku_id 向库存服务
-- 问一次这家店的水位，「任意一个在售 SKU 水位 > 0」即有货。条件与拆分前那条 EXISTS 里的
-- s.status = 1 AND s.deleted_at IS NULL 逐字一致。
SELECT s.product_id, s.id
  FROM skus s
 WHERE s.product_id = ANY(sqlc.arg(product_ids)::bigint[])
   AND s.status = 1
   AND s.deleted_at IS NULL;
