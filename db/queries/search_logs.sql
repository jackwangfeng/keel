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
-- user_id / session_id / parsed_intent 与三个行为列不在这里写：搜索是公开接口，
-- 买家身份不在这条路径上；查询理解还没有；行为列由 POST /search/events 回填
-- （下面那几条）。它们留着列的默认值 NULL —— 那是实话，不是占位。
INSERT INTO search_logs (query, recall_ids, ranked_ids, latency_ms, trace_id,
                         strategy, stages, model_name, model_version)
VALUES (@query, @recall_ids::bigint[], @ranked_ids::bigint[], @latency_ms,
        @trace_id, @strategy, @stages::text[],
        sqlc.narg(model_name), sqlc.narg(model_version));

-- 下面四条是 POST /search/events 的落点（数据模型 §8 那段 trace_id 的注）。
--
-- 定位靠 trace_id 上的全局唯一索引（uk_search_logs_trace），一次点查。
-- 没有一处写 merchant_id：别家店的 trace_id 被 RLS 过滤成「查无此行」，
-- 与不存在的 trace_id 是同一个结果 —— service 把两者合成同一个 404。

-- name: GetSearchLogRankedIDs :one
-- 这次检索真正返回的那几条。service 拿它判 product_id 在不在里面（防刷指标）。
SELECT ranked_ids FROM search_logs WHERE trace_id = @trace_id;

-- name: SetSearchLogClicked :execrows
-- 首次为准：已有值就一行都不改（影响行数 0），重放因此与首次效果相同。
-- 条件写在 WHERE 而不是 SET COALESCE：并发的两次回传里后到的那个
-- 会在行锁释放后重新求值 WHERE，于是看得见先到的那一次写下的值。
UPDATE search_logs SET clicked_id = @product_id::bigint
 WHERE trace_id = @trace_id AND clicked_id IS NULL;

-- name: SetSearchLogCarted :execrows
-- 同上，加购。
UPDATE search_logs SET carted_id = @product_id::bigint
 WHERE trace_id = @trace_id AND carted_id IS NULL;

-- name: SetSearchLogOrdered :execrows
-- 同上，下单。
UPDATE search_logs SET ordered_id = @product_id::bigint
 WHERE trace_id = @trace_id AND ordered_id IS NULL;
