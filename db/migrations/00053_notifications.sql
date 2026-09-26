-- 消息通知：站内消息 + 外发渠道的投递记录（数据模型 §16）。
-- DDL 照抄数据模型设计 §16，那里是唯一真相源。
--
-- ===========================================================================
-- 一、可靠性：通知与状态变化同一个事务（outbox）
-- ===========================================================================
--
-- 订单与售后的每一次需要告诉别人的状态变化（支付成功、发货、审核、退款到账……），
-- 在**做这次状态变化的那个事务里**写一行 notifications，同时往 jobs 入一条
-- notification.deliver 任务（外发渠道的投递）。两者与业务写同生同灭：
--
--   · 状态改了、事务提交了 → 站内消息一定在，外发任务一定在队列里；
--   · 事务回滚了 → 两者一起消失，不存在「回滚了消息却发出去了」。
--
-- 站内消息**就是** notifications 这一行本身 —— 它不需要再被谁投递一次，
-- 买家消息中心与后台铃铛直接读它。外发渠道（微信订阅消息 / 短信 / 邮件）
-- 要调外部接口，不能放进业务事务（网络调用会把订单行锁拖长，而且失败了没法回滚
-- 一条已经发出去的短信），所以只在事务里留一条任务，由 worker 事后投递 ——
-- 这才是 outbox 的本义。worker 复用 jobs 队列（§12）的出队 / 退避 / 死信 / 公平调度，
-- 不另造一套。
--
-- ===========================================================================
-- 二、三张表
-- ===========================================================================
--
--   notifications            一条通知。audience = 1 发给买家（user_id），
--                            = 2 发给商家（store_id：哪家门店的事，后台按员工范围过滤）。
--                            买家的已读状态就在这一行上（read_at），因为收件人只有一个。
--   notification_reads       商家通知的**每个员工**的已读状态。一条「新订单待发货」
--                            是这家门店所有管得着它的员工共享的，谁读了只算谁读了。
--   notification_deliveries  外发渠道的每一次尝试：发出 / 未配置跳过 / 失败。
--
-- ===========================================================================
-- 三、dedupe_key：同一件事只通知一次
-- ===========================================================================
--
-- uk_notifications_dedupe (merchant_id, dedupe_key)。形如 order_paid:<order_no>。
-- 两个用处：
--   · 「自动确认收货即将到期」由定时任务每轮扫描产生，没有这条索引它每十分钟发一次；
--   · 同一个状态迁移被重放时（回调重推被别的闸门挡住之前、SAGA 分支重试被屏障挡住之前），
--     这里是最后一道：撞上就什么都不写，外发任务也不入队。
--
-- ===========================================================================
-- 四、外键一律 ON DELETE CASCADE
-- ===========================================================================
--
-- 通知是派生数据：它指向的买家、门店、SKU 在生产里都是软删（deleted_at），
-- 物理删除只发生在测试清理与数据修复里。那时通知跟着走，而不是反过来挡住
-- 父表的删除 —— 一条三个月前的「已发货」不该让一个用户删不掉。
-- notification_reads.staff_id 是单列外键：staff.merchant_id 可空（平台级员工
-- 用 X-Keel-Merchant 切进一家店读铃铛，他的已读状态也要记），理由与
-- shipments.created_by 相同，登记在 db/tenancy.json 的 fk_single_column_ok。
--
-- ===========================================================================
-- 五、保留期
-- ===========================================================================
--
-- 90 天（service.NotificationRetention），由通知 worker 按租户分批删
-- （idx_notifications_created 撑着那条 DELETE）。reads / deliveries 跟着级联。

-- +goose Up

CREATE TABLE notifications (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    audience    SMALLINT    NOT NULL,          -- 1 买家 2 商家
    user_id     BIGINT,                        -- audience = 1 的收件人
    store_id    BIGINT,                        -- audience = 2：这件事属于哪家门店
    kind        TEXT        NOT NULL,          -- order_paid / refund_rejected / ...
    title       TEXT        NOT NULL,          -- 服务端渲染好的中文标题
    body        TEXT        NOT NULL,          -- 服务端渲染好的中文正文
    target_type TEXT        NOT NULL,          -- order / refund / inventory：点了跳哪里
    order_no    TEXT,
    refund_no   TEXT,
    sku_id      BIGINT,                        -- target_type = inventory 时的 SKU
    dedupe_key  TEXT        NOT NULL,
    read_at     TIMESTAMPTZ,                   -- 只对 audience = 1 有意义
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_notification_audience CHECK (
        (audience = 1 AND user_id IS NOT NULL AND store_id IS NULL)
        OR (audience = 2 AND user_id IS NULL AND store_id IS NOT NULL AND read_at IS NULL)
    ),
    CONSTRAINT chk_notification_target CHECK (
        (target_type = 'order'     AND order_no IS NOT NULL)
        OR (target_type = 'refund' AND refund_no IS NOT NULL)
        OR (target_type = 'inventory' AND store_id IS NOT NULL AND sku_id IS NOT NULL)
    ),
    UNIQUE (id, merchant_id),
    FOREIGN KEY (user_id, merchant_id)  REFERENCES users(id, merchant_id)  ON DELETE CASCADE,
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (sku_id, merchant_id)   REFERENCES skus(id, merchant_id)   ON DELETE CASCADE
);
CREATE UNIQUE INDEX uk_notifications_dedupe ON notifications(merchant_id, dedupe_key);
-- 买家消息中心：我的通知按时间倒序。
CREATE INDEX idx_notifications_user ON notifications(merchant_id, user_id, created_at DESC, id DESC)
    WHERE audience = 1;
-- 买家未读数：只收未读的那一小撮。
CREATE INDEX idx_notifications_user_unread ON notifications(merchant_id, user_id)
    WHERE audience = 1 AND read_at IS NULL;
-- 后台铃铛：这家店的商家通知按时间倒序，门店范围在索引之后过滤。
CREATE INDEX idx_notifications_merchant ON notifications(merchant_id, created_at DESC, id DESC)
    WHERE audience = 2;
-- 保留期清理。
CREATE INDEX idx_notifications_created ON notifications(merchant_id, created_at);

CREATE TABLE notification_reads (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id     BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    notification_id BIGINT      NOT NULL,
    staff_id        BIGINT      NOT NULL REFERENCES staff(id) ON DELETE CASCADE,
    read_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (notification_id, merchant_id)
        REFERENCES notifications(id, merchant_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX uk_notification_reads ON notification_reads(merchant_id, staff_id, notification_id);
CREATE INDEX idx_notification_reads_notification ON notification_reads(merchant_id, notification_id);

CREATE TABLE notification_deliveries (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id     BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    notification_id BIGINT      NOT NULL,
    channel         TEXT        NOT NULL,      -- wechat_subscribe / sms / email
    status          SMALLINT    NOT NULL,      -- 1 已发出 2 未配置跳过 3 失败
    attempt         INT         NOT NULL,      -- 第几次投递（jobs.attempts）
    detail          TEXT,                      -- 跳过 / 失败的原因，渠道回执号
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_notification_delivery CHECK (
        channel IN ('wechat_subscribe', 'sms', 'email')
        AND status IN (1, 2, 3) AND attempt >= 1
    ),
    FOREIGN KEY (notification_id, merchant_id)
        REFERENCES notifications(id, merchant_id) ON DELETE CASCADE
);
CREATE INDEX idx_notification_deliveries ON notification_deliveries(merchant_id, notification_id, channel);

ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON notifications USING (merchant_id = current_merchant());

ALTER TABLE notification_reads ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_reads FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON notification_reads USING (merchant_id = current_merchant());

ALTER TABLE notification_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_deliveries FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON notification_deliveries USING (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON notifications, notification_reads, notification_deliveries TO keel_app;

-- +goose Down
REVOKE ALL ON notifications, notification_reads, notification_deliveries FROM keel_app;
DROP POLICY tenant ON notification_deliveries;
DROP POLICY tenant ON notification_reads;
DROP POLICY tenant ON notifications;
DROP TABLE notification_deliveries;
DROP TABLE notification_reads;
DROP TABLE notifications;
