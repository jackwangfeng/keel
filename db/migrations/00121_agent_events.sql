-- AI 员工的事件（AI 经营 M10 §3，docs/AI经营-M10M11设计.md）：让 agent 被事件叫醒，而不只是每天定时跑一次。
--
-- 四张表：
--   agent_events              事件本身。一行一个「值得 agent 看一眼」的时刻（库存跌破预警线、买家申请售后、
--                             无结果词突增、提案有了结果）。dedupe_key 在店内唯一：扫描类事件
--                             （stock_low / search_zero_spike）每 5 分钟跑一轮，重跑、并发跑都只落一行。
--   agent_event_cursors       拉取的游标：每名 AI 员工确认到哪一条（ack_events）。list_events 不带 after_id 时从这里接着读。
--   agent_webhooks            推送：每名 AI 员工至多一个 webhook。事件写入后由 jobs 队列投递（agent.event.deliver）。
--   agent_webhook_deliveries  每一次投递尝试一行（第几次、HTTP 状态码、错误），排查「为什么没叫醒」用。
--
-- ### webhook 的签名密钥为什么存明文
--
-- 接入密钥（agent_keys）只存 sha256，因为 Keel 只需要**验**它；webhook 密钥方向相反 —— Keel 要拿它**签**
-- 每一次投递（HMAC-SHA256），哈希之后就签不出来了。加密存放需要一把不在库里的主密钥与轮换方案，
-- 本仓库目前没有这套设施（支付渠道的密钥也只在环境变量里）；为一个「只证明这条推送来自 Keel」的密钥
-- 单独造一套，收益小于它的复杂度。所以明文存，但把暴露面收到最小：
--   · 只在创建时、或显式 rotate_secret 时在响应里出现一次；GET 永远不回它；
--   · RLS 照常：别家店读不到；
--   · 泄露的后果是「有人能伪造推给接入方的事件」—— 接入方收到后只是去 list_events 拉，
--     拉取仍要 kagt_ 接入密钥，伪造的推送拿不到任何数据、也做不成任何写操作；
--   · 发现泄露就 rotate_secret，旧密钥立即作废。
--
-- ### 保留期
--
-- 事件与投递记录本期不清理（量级：每家店每天几十到几百条）。需要时按 created_at 加一道与通知保留期同样的分批清理。
-- +goose Up
CREATE TABLE agent_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    type        TEXT        NOT NULL,
    store_id    BIGINT,                                -- NULL = 全店口径的事件（只有全店范围的 AI 员工看得到）
    payload     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    dedupe_key  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_agent_event_type CHECK (type IN ('stock_low', 'refund_created', 'search_zero_spike', 'proposal_decided')),
    CONSTRAINT chk_agent_event_dedupe CHECK (length(dedupe_key) BETWEEN 1 AND 200),
    CONSTRAINT chk_agent_event_payload CHECK (jsonb_typeof(payload) = 'object'),
    UNIQUE (id, merchant_id),
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id)
);
CREATE UNIQUE INDEX uk_agent_events_dedupe ON agent_events(merchant_id, dedupe_key);
CREATE INDEX idx_agent_events_feed ON agent_events(merchant_id, id);
CREATE INDEX idx_agent_events_type ON agent_events(merchant_id, type, created_at DESC);

COMMENT ON TABLE agent_events IS 'AI 员工的事件：库存预警、售后申请、无结果词突增、提案结果（00121，AI 经营 M10）。';

CREATE TABLE agent_event_cursors (
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    agent_staff_id BIGINT      NOT NULL,
    last_acked_id  BIGINT      NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, agent_staff_id),
    CONSTRAINT chk_agent_event_cursor CHECK (last_acked_id >= 0),
    FOREIGN KEY (agent_staff_id, merchant_id) REFERENCES staff(id, merchant_id) ON DELETE CASCADE
);

CREATE OR REPLACE TRIGGER touch_agent_event_cursors_updated_at
    BEFORE UPDATE ON agent_event_cursors FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

COMMENT ON TABLE agent_event_cursors IS 'AI 员工拉取事件的游标：确认到哪一条（00121）。';

CREATE TABLE agent_webhooks (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    agent_staff_id BIGINT      NOT NULL,
    url            TEXT        NOT NULL,
    secret         TEXT        NOT NULL,              -- HMAC 签名密钥，明文（理由见文件头）
    enabled        BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_agent_webhook_url CHECK (length(url) BETWEEN 1 AND 500),
    CONSTRAINT chk_agent_webhook_secret CHECK (length(secret) >= 32),
    UNIQUE (id, merchant_id),
    FOREIGN KEY (agent_staff_id, merchant_id) REFERENCES staff(id, merchant_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX uk_agent_webhooks_agent ON agent_webhooks(merchant_id, agent_staff_id);

CREATE OR REPLACE TRIGGER touch_agent_webhooks_updated_at
    BEFORE UPDATE ON agent_webhooks FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

COMMENT ON TABLE agent_webhooks IS 'AI 员工的事件 webhook：每名 AI 员工至多一个（00121）。';

CREATE TABLE agent_webhook_deliveries (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    event_id     BIGINT      NOT NULL,
    webhook_id   BIGINT      NOT NULL,
    attempt      INT         NOT NULL,
    status_code  INT,                                  -- NULL = 没拿到 HTTP 响应（连不上、超时）
    error        TEXT        NOT NULL DEFAULT '',
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),   -- 这一次尝试的时间（成功与否都记）
    CONSTRAINT chk_agent_webhook_delivery_attempt CHECK (attempt >= 1),
    FOREIGN KEY (event_id, merchant_id)   REFERENCES agent_events(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (webhook_id, merchant_id) REFERENCES agent_webhooks(id, merchant_id) ON DELETE CASCADE
);
CREATE INDEX idx_agent_webhook_deliveries_hook ON agent_webhook_deliveries(merchant_id, webhook_id, delivered_at DESC);
CREATE INDEX idx_agent_webhook_deliveries_event ON agent_webhook_deliveries(merchant_id, event_id);

COMMENT ON TABLE agent_webhook_deliveries IS 'AI 员工事件 webhook 的投递记录：每次尝试一行（00121）。';

ALTER TABLE agent_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_events FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_events
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE agent_event_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_event_cursors FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_event_cursors
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE agent_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_webhooks FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_webhooks
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE agent_webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_webhook_deliveries FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_webhook_deliveries
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_events TO keel_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_event_cursors TO keel_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_webhooks TO keel_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_webhook_deliveries TO keel_app;

-- +goose Down
DROP TABLE agent_webhook_deliveries;
DROP TABLE agent_webhooks;
DROP TABLE agent_event_cursors;
DROP TABLE agent_events;
