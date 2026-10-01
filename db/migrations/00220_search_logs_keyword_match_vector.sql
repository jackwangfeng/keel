-- keyword_match 多一个取值 and+vector（2026-10 复压第十节 ③，service/search.go 的 recallByKeyword）：
-- 多词查询全部命中的不够一页，但向量路的可信命中把这一页凑够了，OR 那条没跑。
-- 记成单独的值而不是混进 and：「AND 自己就够」与「靠向量路补够」是两种结果形态，按走法看
-- 无结果率、点击率时要分得开。
--
-- 换约束：先删旧的、再加新的 NOT VALID（与 00210 相同：旧行都在旧集合里，不为它扫一遍大表）。
-- search_logs 在 BIG_TABLES 里，ALTER 要 ACCESS EXCLUSIVE，先设 lock_timeout；两条语句都只改目录。
-- +goose Up
SET LOCAL lock_timeout = '3s';
ALTER TABLE search_logs DROP CONSTRAINT ck_search_logs_keyword_match;
ALTER TABLE search_logs
    ADD CONSTRAINT ck_search_logs_keyword_match
    CHECK (keyword_match IN ('single', 'and', 'and+or', 'and+vector')) NOT VALID;

COMMENT ON COLUMN search_logs.keyword_match IS
    '关键词召回的走法：single / and / and+or / and+vector（00210、00220）；关键词那一路没跑成时 NULL';

-- +goose Down
SET LOCAL lock_timeout = '3s';
ALTER TABLE search_logs DROP CONSTRAINT ck_search_logs_keyword_match;
ALTER TABLE search_logs
    ADD CONSTRAINT ck_search_logs_keyword_match
    CHECK (keyword_match IN ('single', 'and', 'and+or')) NOT VALID;
COMMENT ON COLUMN search_logs.keyword_match IS
    '关键词召回的走法：single / and / and+or（00210）；关键词那一路没跑成时 NULL';
