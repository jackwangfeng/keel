-- 后台读退款凭证（契约 GET /admin/uploads/{upload_id}）的判权要回答
-- 「哪几张退款单引用了这个凭证地址」—— refunds.evidence_urls @> ARRAY[地址]。
-- DDL 照抄数据模型设计 §11，那里是唯一真相源。
--
-- 没有这条索引，后台每打开一张凭证图就是一次全店退款单的顺序扫描。
-- GIN 建不出 merchant_id 打头的复合形状（要 btree_gin 扩展，为一条索引引入一个扩展不值当），
-- 所以它是单列的：RLS 的 merchant_id = current_merchant() 在索引命中之后作为过滤条件，
-- 命中的行本来就只有一两张退款单。

-- +goose Up
CREATE INDEX idx_refunds_evidence ON refunds USING GIN (evidence_urls);

-- +goose Down
DROP INDEX idx_refunds_evidence;
