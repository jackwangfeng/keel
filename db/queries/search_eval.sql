-- 离线评测集的取数（cmd/keel-searcheval，语义检索层 §9.1）。
--
-- 这几条只给离线工具用，不在任何请求路径上：按本店全量扫 search_logs / products 都可以接受。
-- 候选**不**按门店可见性、价格过滤：评测问的是「这件商品与这个查询相不相关」，与它今天在哪家门店
-- 上没上架无关。租户隔离照旧只靠 RLS（文件里不出现 merchant_id，与其余 db/queries 同一条规矩）。

-- name: ListSearchEvalQueries :many
-- 本店 @since 之后搜过的查询，按次数从多到少。query 按原样分组：前后空白、大小写的差别留着，
-- 那是买家真实输入的样子，归一化交给评测工具决定。
SELECT l.query, count(*)::bigint AS hits
  FROM search_logs l
 WHERE l.created_at >= @since::timestamptz
 GROUP BY l.query
 ORDER BY hits DESC, l.query
 LIMIT @row_limit::int;

-- name: SearchEvalVectorNeighbors :many
-- 与查询向量最近的 @row_limit 件在售商品（不看门店、不看价格），带类目名与余弦距离。
-- 近邻的尾巴正是相关度下限要切的地方，所以这里不设任何阈值。
SELECT p.id, p.title, p.subtitle, c.name AS category_name,
       (v.embedding <=> @query_embedding::vector)::float8 AS distance
  FROM product_text_vectors v
  JOIN products p ON p.id = v.product_id
  JOIN categories c ON c.id = p.category_id
 WHERE p.deleted_at IS NULL
   AND p.status = 1
 ORDER BY v.embedding <=> @query_embedding::vector
 LIMIT @row_limit::int;

-- name: SearchEvalKeywordHits :many
-- bigram tsquery 命中的在售商品，ts_rank_cd 高的在前，截到 @row_limit 件。
-- 命中集合同样经 keyword_hit_products（00172），理由见 search.sql 的 SearchProductsByKeyword。
SELECT p.id, p.title, p.subtitle, c.name AS category_name,
       ts_rank_cd(p.search_vector, to_tsquery('simple', @tsquery::text))::float8 AS rank
  FROM products p
  JOIN categories c ON c.id = p.category_id
 WHERE p.deleted_at IS NULL
   AND p.status = 1
   AND p.id IN (SELECT keyword_hit_products(to_tsquery('simple', @tsquery::text)))
 ORDER BY rank DESC, p.id
 LIMIT @row_limit::int;

-- name: SearchEvalDistances :many
-- 一批商品与查询向量的余弦距离：只被关键词捞到的候选也要有这个数，校准下限时才能一起看。
-- 没有向量行的商品不出现（还没索引到，或索引失败）。
SELECT v.product_id, (v.embedding <=> @query_embedding::vector)::float8 AS distance
  FROM product_text_vectors v
 WHERE v.product_id = ANY(@product_ids::bigint[]);
