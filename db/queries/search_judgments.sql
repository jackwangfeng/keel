-- 检索相关度预判（00230）。写由后台任务 service/search_judge.go 做，读在 service/search.go 的 applyFloor 之前。

-- name: GetSearchJudgments :many
-- 这条（归一化后的）查询对这批商品的预判；没判过的不在结果里。走主键 (merchant_id, query, product_id)。
SELECT j.product_id, j.relevance::float8 AS relevance
  FROM search_relevance_judgments j
 WHERE j.query = @query::text
   AND j.product_id = ANY(@product_ids::bigint[]);

-- name: ListProductsNeedingJudgment :many
-- 这批商品里这条查询还要（重新）判的：没判过、判它的不是 @judge、判过之后商品又改过、或者判得太久（早于 @fresh_after）。
SELECT p.id
  FROM products p
  LEFT JOIN search_relevance_judgments j
         ON j.product_id = p.id AND j.query = @query::text
 WHERE p.id = ANY(@product_ids::bigint[])
   AND (j.product_id IS NULL
        OR j.judge <> @judge::text
        OR j.judged_at < p.updated_at
        OR j.judged_at < @fresh_after::timestamptz)
 ORDER BY p.id;

-- name: UpsertSearchJudgments :exec
-- 一条查询的一批判断，按 (query, product_id) 覆盖。两个数组一一对应、等长（调用方保证）。
-- 两个数组在 SELECT 列表里并排 unnest（PG10 起按位置配对）：sqlc 认不了 FROM unnest(a, b)，同 product_store_stock.sql。
INSERT INTO search_relevance_judgments (query, product_id, relevance, judge)
SELECT @query::text,
       unnest(@product_ids::bigint[]),
       unnest(@relevances::real[]),
       @judge::text
ON CONFLICT ON CONSTRAINT search_relevance_judgments_pkey DO UPDATE
   SET relevance = EXCLUDED.relevance,
       judge     = EXCLUDED.judge,
       judged_at = now();

-- name: PurgeSearchJudgmentsBefore :execrows
-- 太久没刷新的预判（那条查询已经不热了），走 idx_search_relevance_judgments_judged (merchant_id, judged_at)。
DELETE FROM search_relevance_judgments
 WHERE ctid = ANY(ARRAY(SELECT j.ctid FROM search_relevance_judgments j
                         WHERE j.judged_at < sqlc.arg(before)::timestamptz
                         LIMIT sqlc.arg(batch)::int));
