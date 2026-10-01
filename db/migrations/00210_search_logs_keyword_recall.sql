-- 关键词召回改成「先 AND、不够再 OR，召回 SQL 先排序截断」之后（2026-10 性能压测第六节 ③，
-- service/search.go 的 recallByKeyword），每次检索的关键词那一路有三种走法：
--
--	single  查询只切出一个词，只跑一条（与改之前相同）；
--	and     多词，全部词都命中的已经够一页，只用 AND；
--	and+or  多词，全部命中的不够一页，又用 OR 补齐（全部命中的排在前面）。
--
-- 走了哪一条直接决定结果长什么样（and 时不会混进只命中部分词的商品），按策略对比线上指标、
-- 回看「这条查询为什么搜出这些」时都要知道。契约的响应体里没有它的位置，所以记在日志行上：
--
--	keyword_match  上面三个串之一；关键词那一路没跑成、或不在服务范围（一路都没跑）时 NULL
--	keyword_limit  召回窗口 N：召回 SQL 先按相关度截到这么多件，再补价格与图片
--	keyword_hits   这一路最终交给融合的件数（≤ keyword_limit）
--
-- 先扩后收的「扩」：三列都可空、无默认值，加列只改目录不重写表。旧行留 NULL —— 那时只有 OR 一种走法，
-- 但窗口与件数没有记过，无从追认。search_logs 在 BIG_TABLES 里，ALTER 要 ACCESS EXCLUSIVE，先设 lock_timeout。
-- +goose Up
SET LOCAL lock_timeout = '3s';
ALTER TABLE search_logs
    ADD COLUMN keyword_match TEXT,
    ADD COLUMN keyword_limit INTEGER,
    ADD COLUMN keyword_hits  INTEGER;

-- 约束 NOT VALID：旧行全是 NULL 本来就满足，不为它扫一遍大表；新写入的行照样被检查。
ALTER TABLE search_logs
    ADD CONSTRAINT ck_search_logs_keyword_match
    CHECK (keyword_match IN ('single', 'and', 'and+or')) NOT VALID;

COMMENT ON COLUMN search_logs.keyword_match IS
    '关键词召回的走法：single / and / and+or（00210）；关键词那一路没跑成时 NULL';
COMMENT ON COLUMN search_logs.keyword_limit IS '关键词召回窗口 N（召回 SQL 先排序截断到的件数，00210）';
COMMENT ON COLUMN search_logs.keyword_hits IS '关键词那一路交给融合的件数（00210）';

-- +goose Down
SET LOCAL lock_timeout = '3s';
ALTER TABLE search_logs
    DROP CONSTRAINT ck_search_logs_keyword_match,
    DROP COLUMN keyword_hits,
    DROP COLUMN keyword_limit,
    DROP COLUMN keyword_match;
