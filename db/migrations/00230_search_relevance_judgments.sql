-- 检索相关度预判：search_relevance_judgments（2026-10-01，语义检索层 §9.1「在线延迟」那一条）。
--
-- 余弦相似度单独当闸门分不开相关与不相关（离线评测：下限 0.40 精确率 0.13），而判别模型（Kev-4B）
-- 判得准但慢（约 25 ms + 14 ms/题），挡不到搜索请求前面。搜索词又高度集中，所以在后台把高频查询的
-- 向量独有候选判好存在这里，搜索时命中就按判断留或去，没命中照旧按余弦下限（service/search.go 的 applyFloor）。
--
-- ① query 存的是**归一化后**的查询（search.NormQuery：去首尾空白、连续空白压成一个、转小写），写与读两边
--   走同一个函数，「连衣裙」与「 连衣裙 」是同一行。
-- ② 判断随商品变：商品改了标题再判一次（后台任务按 products.updated_at > judged_at 找），删了随外键级联。
-- ③ judge 记「模型@版本」。读的一侧不看 judge、只看 relevance；换判别模型时后台任务把 judge 不同的逐步重判覆盖
--   （与向量的 model_version 同一个思路），不会出现新旧两种口径长期混用。
-- ④ 冷数据：judged_at 太久没刷新的（那条查询已经不热了）由保留期任务清掉。
-- +goose Up
CREATE TABLE search_relevance_judgments (
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    query       TEXT        NOT NULL,
    product_id  BIGINT      NOT NULL,
    relevance   REAL        NOT NULL CHECK (relevance >= 0 AND relevance <= 1),
    judge       TEXT        NOT NULL,
    judged_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, query, product_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id) ON DELETE CASCADE
);
CREATE INDEX idx_search_relevance_judgments_judged ON search_relevance_judgments(merchant_id, judged_at);

COMMENT ON TABLE search_relevance_judgments IS
    '检索相关度预判（00230）：高频查询的向量独有候选由判别模型判好，搜索命中就用，替代余弦下限。写见 service/search_judge.go。';
COMMENT ON COLUMN search_relevance_judgments.query IS '归一化后的查询（search.NormQuery）';
COMMENT ON COLUMN search_relevance_judgments.relevance IS '判别模型给「是买家要找的」的概率，0–1';
COMMENT ON COLUMN search_relevance_judgments.judge IS '判别模型 名字@版本';

ALTER TABLE search_relevance_judgments ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_relevance_judgments FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON search_relevance_judgments
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON search_relevance_judgments TO keel_app;

-- +goose Down
DROP TABLE search_relevance_judgments;
