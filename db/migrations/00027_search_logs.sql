-- 检索日志：search_logs（M5）。DDL 照抄数据模型设计 §8「search_logs（第一天就要埋）」，
-- 那里是唯一真相源。
--
-- ===========================================================================
-- 与 §8 原稿相比，这一份改了什么（文档已同步改）
-- ===========================================================================
--
-- ① 多一列 stages TEXT[] NOT NULL：这一次**真的跑过**的阶段，按执行顺序。
--    原稿靠 strategy 一列归因，而 strategy 说的是「本来要跑哪条流水线」，
--    不是「这一次跑成了哪几段」。两者在降级链上分叉：推理引擎挂了时
--    strategy 仍然是同一个值，向量那一路却整个没跑（语义检索层 §8）。
--    不单独记下来的话，§9.3「按策略分组对比线上指标」那张表会把一批纯关键词
--    结果算进「双路召回」那一桶，而且没有任何东西能把它们挑出来。
--    model_name 为 NULL 可以间接推出「向量没跑」，但推不出「业务重排跑没跑」；
--    一列显式的阶段清单比一组要人去反推的规则便宜。
--
-- ② strategy 去掉 DEFAULT 'default'。
--    'default' 在契约里是一个**别名**（「不指定就用当时的默认策略」），
--    日志要的是「这一次到底跑了哪条」（service.ResolveStrategy 的解析结果）。
--    留着那个 DEFAULT，漏写 strategy 的 INSERT 会静默落进一个语义随时间漂移的桶，
--    而它恰好是 §9.3 那张对比表最不该有的一桶。去掉之后漏写是当场 23502。
--
-- ③ merchant_id 带 DEFAULT current_merchant()，所有索引以 merchant_id 打头
--    （§2 规矩，与 00013 / 00026 同一个理由：db/queries 里不许出现 merchant_id）。
--    唯一的例外是 uk_search_logs_trace，全局唯一的理由写在 §8 与
--    db/tenancy.json 的 unique_global_ok 里。
--
-- ===========================================================================
-- 写它的那条路径不在主链路上
-- ===========================================================================
--
-- POST /search 每次成功返回都写一行（service/search.go 的 recordSearchLog）。
-- 写失败**不让搜索失败**：日志是辅助数据，§8「任何一环故障，搜索都必须仍能
-- 返回结果」对它同样成立 —— 比推理引擎更应该成立，因为它连结果质量都不影响。
-- 失败会留一条 ERROR 日志，那是「埋点断了」在系统里唯一的痕迹。
--
-- user_id 没有外键：搜索是公开接口，未登录访客的这一列是 NULL；日志表也不该
-- 因为用户注销而卡住删除顺序（§8 原文，db/tenancy.json 的 fk_missing_ok）。
-- clicked_id / carted_id / ordered_id 同样没有外键：它们由 POST /search/events
-- 回填（本轮未实现），指向的商品之后被删除时，日志不该跟着被卡住或被级联删掉。

-- +goose Up

CREATE TABLE search_logs (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id   BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    user_id       BIGINT,
    session_id    TEXT,
    query         TEXT        NOT NULL,
    parsed_intent JSONB,                  -- LLM 解析出的结构化意图（查询理解上线前恒为 NULL）
    recall_ids    BIGINT[],               -- 召回结果（RRF 融合后的全部候选，融合序）
    ranked_ids    BIGINT[],               -- 最终排序（真正返回给调用方的那几条）
    clicked_id    BIGINT,
    carted_id     BIGINT,                 -- 加购回传，算「搜索→加购率」
    ordered_id    BIGINT,
    latency_ms    INT,
    trace_id      TEXT        NOT NULL,
    -- 归因：没有这几列，排序策略与模型升级的效果都无法回溯
    strategy      TEXT        NOT NULL,   -- 本次实际使用的排序策略（解析后的，见文件头 ②）
    stages        TEXT[]      NOT NULL,   -- 本次真正跑过的阶段（见文件头 ①）
    model_name    TEXT,                   -- embedding 实际版本；向量路没跑成时为 NULL
    model_version TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_search_logs_strategy
    ON search_logs(merchant_id, strategy, created_at DESC);
-- 行为回传按 trace_id 定位，必须唯一且可索引
CREATE UNIQUE INDEX uk_search_logs_trace ON search_logs(trace_id);

-- ---------------------------------------------------------------------------
-- 行级安全：ENABLE 之外必须再加 FORCE（§2「坑一」）
-- ---------------------------------------------------------------------------
ALTER TABLE search_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_logs FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON search_logs
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

-- GRANT 面：tenant 类的四权（db/tenancy.json）。00005 之后新表默认只有 SELECT。
-- UPDATE 是 /search/events 回填行为列要的，DELETE 是将来按保留期清理要的。
GRANT SELECT, INSERT, UPDATE, DELETE ON search_logs TO keel_app;

-- +goose Down

REVOKE ALL ON search_logs FROM keel_app;
DROP TABLE search_logs;
