-- 检索日志（数据模型 §8，迁移 00027）。
--
-- 全文没有一处写 merchant_id：租户由 RLS 过滤，写入那一列由
-- DEFAULT current_merchant() 补。理由见 db/queries/products.sql 与
-- scripts/check_query_tenancy.py 的文件头。
--
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。

-- name: InsertSearchLog :exec
-- POST /search 每次成功返回写一行。
--
-- user_id / session_id / parsed_intent 与三个行为列本轮不写：搜索是公开接口，
-- 买家身份不在这条路径上；查询理解还没有；行为列由 POST /search/events 回填
-- （本轮未实现）。它们留着列的默认值 NULL —— 那是实话，不是占位。
INSERT INTO search_logs (query, recall_ids, ranked_ids, latency_ms, trace_id,
                         strategy, stages, model_name, model_version)
VALUES (@query, @recall_ids::bigint[], @ranked_ids::bigint[], @latency_ms,
        @trace_id, @strategy, @stages::text[],
        sqlc.narg(model_name), sqlc.narg(model_version));
