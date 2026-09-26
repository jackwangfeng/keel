-- 后台订单与退款单的列表 / 详情（GET /admin/orders、GET /admin/refunds 及各自的详情）。
-- DDL 照抄数据模型设计 §5 / §11，那里是唯一真相源。
--
-- ===========================================================================
-- 一、索引：后台列表的几种走法各有一条
-- ===========================================================================
--
-- 此前 orders 上的索引全是给别的读路径建的：idx_orders_user（我的订单）、
-- idx_orders_store（按门店聚合，也正好是门店管理员 / 按门店筛的后台列表）、
-- idx_orders_status_expire（超时关单，部分索引只收 10）、idx_orders_refunding
-- （部分索引只收 refund_status = 1）。**全店范围、按下单时间倒序翻页**这条
-- 后台最常见的走法没有一条能用的：没有 merchant_id 打头、created_at 跟着的索引，
-- 于是每翻一页都是「把全店订单按时间排一遍再截 20 行」。
--
--   idx_orders_admin_created   全店、不筛状态：管理员 / 操作员打开订单页的默认视图
--   idx_orders_admin_status    按状态筛：「待发货」（status = 20）是后台每天第一眼
--   idx_orders_receiver_phone  客服按收货人手机号精确找单（表达式索引）
--
-- 按买家账号手机号找单走的是 uk_users_phone + idx_orders_user，不用新索引：
-- 查询把手机号先换成 user_id（标量子查询，只会命中一行），再按 user_id 进订单。
--
-- refunds 同理：
--
--   idx_refunds_admin_created  全店、按申请时间倒序
--   idx_refunds_admin_status   按状态筛：「待审核」（10）与「待确认收到退货」（20）
--
-- 00034 的 idx_refunds_pending 是部分索引（只收 10 / 30），按 updated_at 排 ——
-- 那是给工单与重试任务的；后台列表按申请时间（created_at）排，而且 20 也要能筛，
-- 所以另建一条而不是改它。
--
-- 每条都以 merchant_id 打头：RLS 的谓词 merchant_id = current_merchant() 由规划器
-- 当作普通的等值条件使用，索引前缀对上它才用得上（同 00006 的写法）。
--
-- ===========================================================================
-- 二、refunds 补三列审核记录：audited_by、received_at、received_by
-- ===========================================================================
--
-- 退款单详情要给后台看「审核记录」。00034 只留了 audited_at 与 reject_reason ——
-- 知道什么时候审的、为什么驳回，不知道**谁**审的；退货退款的第二步
-- （确认收到退货，20 → 30）连时间都没有，updated_at 会被后面的入账覆盖掉。
-- 退款是资金动作，「这笔钱是谁放出去的」答不上来，售后纠纷就没有落点。
--
-- 两个 *_by 都是指向 staff 的**单列外键**，与 shipments.created_by 同一个理由
-- （db/tenancy.json 的 fk_single_column_ok）：平台级员工的 merchant_id 是 NULL，
-- 复合外键会让「平台管理员替某家店审一张退款单」写不进去。
--
-- **不加 CHECK 把它们与状态钉在一起**，与 chk_refund_state 的做法不同，理由是存量：
-- 这份迁移之前审过的单没有 audited_by，而那些单是真的被人审过的，
-- 不能为了一条 CHECK 编一个审核人。received_at 也钉不住：30 退款中既可以从 20 来
-- （有 received_at），也可以从 10 直接来（仅退款，没有），状态本身分不出来。
-- 所以两列都由写它们的那条 UPDATE 负责（RejectRefund / ApproveRefund /
-- ReceiveRefundGoods），而不是由数据库兜底 —— 这一点写在 §11 里。

-- +goose Up

CREATE INDEX idx_orders_admin_created ON orders(merchant_id, created_at DESC, id DESC);
CREATE INDEX idx_orders_admin_status  ON orders(merchant_id, status, created_at DESC, id DESC);
CREATE INDEX idx_orders_receiver_phone ON orders(merchant_id, (receiver_snapshot->>'phone'));

CREATE INDEX idx_refunds_admin_created ON refunds(merchant_id, created_at DESC, id DESC);
CREATE INDEX idx_refunds_admin_status  ON refunds(merchant_id, status, created_at DESC, id DESC);

ALTER TABLE refunds ADD COLUMN audited_by  BIGINT REFERENCES staff(id);
ALTER TABLE refunds ADD COLUMN received_at TIMESTAMPTZ;
ALTER TABLE refunds ADD COLUMN received_by BIGINT REFERENCES staff(id);

-- +goose Down
ALTER TABLE refunds DROP COLUMN received_by;
ALTER TABLE refunds DROP COLUMN received_at;
ALTER TABLE refunds DROP COLUMN audited_by;
DROP INDEX idx_refunds_admin_status;
DROP INDEX idx_refunds_admin_created;
DROP INDEX idx_orders_receiver_phone;
DROP INDEX idx_orders_admin_status;
DROP INDEX idx_orders_admin_created;
