-- 退款与售后（数据模型 §11）：申请、撤回、审核、确认收到退货、渠道回调入账，
-- 以及买家侧的读。
--
-- 租户由 RLS 过滤，这里一处都不写（check_query_tenancy.py）；user_id 出现的地方
-- 都是**越权过滤**（同一家店里 A 买家不能碰 B 买家的退款单），理由与
-- db/queries/orders.sql 买家侧读接口那段一字不差。
--
-- 注释里不许有反引号，理由见 db/queries/inventories.sql 的第三条说明。
--
-- ### 锁的顺序：先订单，后退款单
--
-- 申请退款要在订单行锁之下复算「在途件数」（§11：数据库拦不住在途超退），
-- 而审核、撤回、入账都要改订单（refund_status、50 退款中的进出、refunded_cents）。
-- 所有路径一律**先锁订单行、再锁退款单行**，两笔并发的售后动作因此不会交叉等待。

-- ---------------------------------------------------------------------------
-- 锁
-- ---------------------------------------------------------------------------

-- name: LockUserOrderByNo :one
-- 申请退款的第一步：按单号取当前买家自己的订单并锁住这一行。
--
-- FOR UPDATE 不是可选的：同一个订单上两次并发的退款申请必须串行，
-- 否则两边各自读到「在途 0 件」，各自插一张退款单，合起来超退 ——
-- 而那正是 chk_item_refund 拦不住的那一种（它只看已退，不看在途）。
SELECT id, order_no, user_id, store_id, region_id, status,
       goods_amount_cents, freight_cents, freight_discount_cents,
       discount_cents, payable_cents, paid_cents, refunded_cents, refund_status,
       expire_at, paid_at, shipped_at, finished_at, created_at, user_coupon_id,
       coupon_name
  FROM orders
 WHERE order_no = $1
   AND user_id = $2
   AND status <> 0
   FOR UPDATE;

-- name: LockOrderByID :one
-- 审核、撤回、入账改订单之前先锁它（见文件头「锁的顺序」）。
SELECT id, order_no, user_id, store_id, region_id, status,
       goods_amount_cents, freight_cents, freight_discount_cents,
       discount_cents, payable_cents, paid_cents, refunded_cents, refund_status,
       expire_at, paid_at, shipped_at, finished_at, created_at, user_coupon_id,
       coupon_name
  FROM orders
 WHERE id = $1
   FOR UPDATE;

-- ---------------------------------------------------------------------------
-- 申请
-- ---------------------------------------------------------------------------

-- name: ListOrderItemsForRefund :many
-- 算退款金额要的全部素材：购买件数、行金额、分摊优惠、已退件数与金额。
SELECT id, sku_id, quantity, amount_cents, discount_cents, refunded_qty, refunded_cents
  FROM order_items
 WHERE order_id = $1
 ORDER BY id;

-- name: RefundingQtyByItem :many
-- 这一单每一行的**在途**退款件数（契约 OrderItem.refunding_qty 的定义式，§11）。
-- 在途 = 待审核 / 待买家退货 / 退款中。
SELECT ri.order_item_id, SUM(ri.quantity)::int AS refunding_qty
  FROM refund_items ri
  JOIN refunds r ON r.id = ri.refund_id
 WHERE r.order_id = $1
   AND r.status IN (10, 20, 30)
 GROUP BY ri.order_item_id;

-- name: FindSettledPayment :one
-- 原路退回退到哪一笔：这一单**成功**的那笔支付。
--
-- 付过两次的订单（支付回调在订单已是 20 之后又来了一笔）会有两行成功支付，
-- 取最早的那一笔 —— 它是让订单从 10 走到 20 的那一笔（SettleOrder 与它同一个事务）。
-- 后来那一笔是要人工退的重复付款，不该被售后流程顺手退掉。
SELECT id, payment_no, channel
  FROM payments
 WHERE order_id = $1 AND status = 1
 ORDER BY id
 LIMIT 1;

-- name: InsertRefund :one
-- 落一张 10 待审核的退款单。租户列不出现（00034 的 DEFAULT current_merchant()）。
INSERT INTO refunds (refund_no, order_id, payment_id, user_id, refund_type, reason_code,
                     reason_text, evidence_urls, goods_amount_cents, freight_cents,
                     amount_cents, channel)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING id;

-- name: InsertRefundItem :exec
INSERT INTO refund_items (refund_id, order_item_id, quantity, amount_cents)
VALUES ($1, $2, $3, $4);

-- name: StartWholeOrderRefund :execrows
-- 未发货的整单退款申请：订单 20 已支付 → 50 退款中（§5 两维度那张表的第一行）。
UPDATE orders SET status = 50 WHERE id = $1 AND status = 20;

-- name: RecomputeOrderRefundStatus :exec
-- 按退款单的**当前事实**重算订单的资金维度（§5 refund_status）。
--
-- 写成一条从 refunds 推出来的 UPDATE，而不是让每条路径各自赋一个值：
-- 申请、撤回、驳回、到账四条路径都要改它，各写各的话，「退完一笔、另一笔还在途」
-- 这种组合迟早有一条路径算错。规则只有一份：
--   有在途的退款单            → 1 退款中
--   没有在途、一分没退过       → 0 无退款
--   没有在途、退满了实收       → 3 全额退款完成
--   其余                      → 2 部分退款完成
-- chk_refund_status 是它背后的兜底：算出来的值与 refunded_cents 对不上，写不进去。
UPDATE orders o
   SET refund_status = CASE
         WHEN EXISTS (SELECT 1 FROM refunds r
                       WHERE r.order_id = o.id AND r.status IN (10, 20, 30)) THEN 1
         WHEN o.refunded_cents = 0 THEN 0
         WHEN o.refunded_cents = o.paid_cents THEN 3
         ELSE 2
       END
 WHERE o.id = $1;

-- ---------------------------------------------------------------------------
-- 读
-- ---------------------------------------------------------------------------

-- name: GetRefundByNo :one
-- 按对外编号取退款单（后台与入账用，没有买家过滤；租户由 RLS 管）。
-- store_id 一起带出来：后台判权按订单的履约门店（与发货同一个判据）。
SELECT r.id, r.refund_no, r.order_id, o.order_no, o.store_id, p.payment_no, r.user_id,
       r.refund_type, r.reason_code, r.reason_text, r.evidence_urls,
       r.goods_amount_cents, r.freight_cents, r.amount_cents, r.status, r.channel,
       r.channel_refund_id, r.reject_reason, r.audited_at, r.refunded_at,
       r.created_at, r.updated_at,
       r.return_carrier_code, r.return_tracking_no, r.return_submitted_at
  FROM refunds r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
 WHERE r.refund_no = $1;

-- name: GetUserRefundByNo :one
-- 买家读自己的退款单。查不到与「不是你的」回同一个 404（refund_no 不可枚举，
-- 分开报会把它变成一个存在性判定器）。
SELECT r.id, r.refund_no, r.order_id, o.order_no, o.store_id, p.payment_no, r.user_id,
       r.refund_type, r.reason_code, r.reason_text, r.evidence_urls,
       r.goods_amount_cents, r.freight_cents, r.amount_cents, r.status, r.channel,
       r.channel_refund_id, r.reject_reason, r.audited_at, r.refunded_at,
       r.created_at, r.updated_at,
       r.return_carrier_code, r.return_tracking_no, r.return_submitted_at
  FROM refunds r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
 WHERE r.refund_no = $1
   AND r.user_id = $2;

-- name: LockRefundStatus :one
-- 在订单行锁之下锁住退款单并读回它**此刻**的状态（见文件头「锁的顺序」）。
SELECT status FROM refunds WHERE id = $1 FOR UPDATE;

-- name: ListRefundItems :many
-- 一批退款单的明细，带下单时的标题与图片快照（契约 RefundItem）。
SELECT ri.refund_id, ri.order_item_id, oi.sku_id, ri.quantity, ri.amount_cents,
       oi.title_snapshot, oi.image_snapshot
  FROM refund_items ri
  JOIN order_items oi ON oi.id = ri.order_item_id
 WHERE ri.refund_id = ANY(sqlc.arg(refund_ids)::bigint[])
 ORDER BY ri.refund_id, ri.order_item_id;

-- name: ListOrderRefunds :many
-- 一个订单的全部退款单，按申请时间倒序（契约 GET /orders/{order_no}/refunds
-- 与 OrderDetail.refunds）。同一毫秒的两张按 id 倒序，顺序才是确定的。
SELECT r.id, r.refund_no, r.order_id, o.order_no, o.store_id, p.payment_no, r.user_id,
       r.refund_type, r.reason_code, r.reason_text, r.evidence_urls,
       r.goods_amount_cents, r.freight_cents, r.amount_cents, r.status, r.channel,
       r.channel_refund_id, r.reject_reason, r.audited_at, r.refunded_at,
       r.created_at, r.updated_at,
       r.return_carrier_code, r.return_tracking_no, r.return_submitted_at
  FROM refunds r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
 WHERE r.order_id = $1
 ORDER BY r.created_at DESC, r.id DESC;

-- name: ListUserRefunds :many
-- 我的退款单，一页。status 用可空参数：传 NULL 就是不筛，
-- 与 CountUserRefunds 共用同一套谓词（理由同 ListUserOrders）。
SELECT r.id, r.refund_no, r.order_id, o.order_no, o.store_id, p.payment_no, r.user_id,
       r.refund_type, r.reason_code, r.reason_text, r.evidence_urls,
       r.goods_amount_cents, r.freight_cents, r.amount_cents, r.status, r.channel,
       r.channel_refund_id, r.reject_reason, r.audited_at, r.refunded_at,
       r.created_at, r.updated_at,
       r.return_carrier_code, r.return_tracking_no, r.return_submitted_at
  FROM refunds r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
 WHERE r.user_id = sqlc.arg(user_id)
   AND (sqlc.narg(status)::smallint IS NULL OR r.status = sqlc.narg(status)::smallint)
 ORDER BY r.created_at DESC, r.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountUserRefunds :one
SELECT count(*)
  FROM refunds r
 WHERE r.user_id = sqlc.arg(user_id)
   AND (sqlc.narg(status)::smallint IS NULL OR r.status = sqlc.narg(status)::smallint);

-- ---------------------------------------------------------------------------
-- 状态推进。每一条都带预期的起点状态，失配影响 0 行；00034 的触发器是第二道。
-- ---------------------------------------------------------------------------

-- name: SubmitReturnShipment :execrows
-- 买家填寄回物流（契约 POST /refunds/{refund_no}/return-shipment，00037）。
-- **不改 status**：只有退货退款、只有停在 20 待买家退货时能填，20 期间可以覆盖
-- （填错单号是常事）。user_id 是越权过滤，理由见文件头。
-- 不经过状态机触发器（它挂在 UPDATE OF status 上），「只有 20 能填」由这里的谓词负责；
-- chk_refund_return_shipment 兜住「三列同生同灭、只能在退货退款上」。
UPDATE refunds
   SET return_carrier_code = sqlc.arg(carrier_code),
       return_tracking_no  = sqlc.arg(tracking_no),
       return_submitted_at = now()
 WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
   AND refund_type = 2 AND status = 20;

-- name: CancelRefund :execrows
-- 买家撤回：10 待审核 / 20 待买家退货 → 60 已取消。30 退款中撤不回来（钱在路上）。
UPDATE refunds SET status = 60
 WHERE id = $1 AND user_id = $2 AND status IN (10, 20);

-- name: ListReturnOverdueRefunds :many
-- 退货超时未寄回的候选（service/return_timeout.go，00059）：退货退款、停在 20 待买家退货、
-- 没填寄回物流、审核通过的时间早于截止（now() 减去这家店的 return_ship_days，由调用方算好）。
-- 按审核时间从早到晚，最久的先关。部分索引 idx_refunds_return_due（00060）正好对上这条扫描。
-- 这只是预筛；真正的判断在处置事务里、订单与退款单行锁之下由 ExpireReturnRefund 的谓词再做一次。
SELECT r.id, r.refund_no, r.order_id
  FROM refunds r
 WHERE r.status = 20 AND r.refund_type = 2 AND r.return_submitted_at IS NULL
   AND r.audited_at < sqlc.arg(cutoff)::timestamptz
 ORDER BY r.audited_at, r.id
 LIMIT sqlc.arg(page_limit);

-- name: ExpireReturnRefund :execrows
-- 退货超时未寄回：20 待买家退货 → 60 已取消（状态机里画着的那条 20 → 60，00034）。
-- 谓词把预筛的每一条都重判一遍：仍在 20、是退货退款、**没有填寄回物流**、审核时间早于截止。
-- 买家在预筛之后、处置之前填了物流，这条影响 0 行 —— 已经寄出的货不能被关单。
-- 调用方先锁订单再锁退款单（与填寄回物流、撤回同一个顺序），所以这里读到的是最新版本。
UPDATE refunds SET status = 60
 WHERE id = sqlc.arg(id) AND status = 20 AND refund_type = 2
   AND return_submitted_at IS NULL
   AND audited_at < sqlc.arg(cutoff)::timestamptz;

-- name: RejectRefund :execrows
-- 审核驳回：10 → 50，必须带理由（chk_refund_state 也钉着这一条）。
-- audited_by 记下是谁驳回的（00035 的审核记录）。
UPDATE refunds SET status = 50, reject_reason = sqlc.arg(reject_reason), audited_at = now(),
       audited_by = sqlc.arg(audited_by)
 WHERE id = sqlc.arg(id) AND status = 10;

-- name: ApproveRefund :execrows
-- 审核通过：10 → 20（退货退款）或 10 → 30（仅退款）。
-- 运费由审核裁定（退货退款）或沿用申请时按规则算好的值（仅退款），
-- 实退总额跟着重算 —— chk_refund_amount 要它恒等于货款 + 运费。
UPDATE refunds
   SET status = sqlc.arg(next_status), freight_cents = sqlc.arg(freight_cents),
       amount_cents = goods_amount_cents + sqlc.arg(freight_cents), audited_at = now(),
       audited_by = sqlc.arg(audited_by)
 WHERE id = sqlc.arg(id) AND status = 10;

-- name: ReceiveRefundGoods :execrows
-- 商家确认收到退货：20 待买家退货 → 30 退款中。记下谁、什么时候收的（00035）。
UPDATE refunds SET status = 30, received_at = now(), received_by = sqlc.arg(received_by)
 WHERE id = sqlc.arg(id) AND status = 20;

-- name: CompleteRefund :execrows
-- 渠道回调入账：30 退款中 → 40 已退款，写下渠道流水号、原始报文与到账时间。
--
-- **这是 channel_refund_id 的唯一写入路径**（契约原话）。同一个流水号第二次到来
-- 撞 uk_refunds_channel_txn，由 Go 侧按约束名挑成 ErrDuplicateChannelRefund ——
-- 光秃秃的 UPDATE，不吞任何别的唯一冲突，与支付回调的 InsertPayment 同一条理由。
UPDATE refunds
   SET status = 40, channel_refund_id = $2, notify_payload = $3, refunded_at = $4
 WHERE id = $1 AND status = 30;

-- name: RecordRefundNotify :exec
-- 回调到了但我们不认这笔账（金额对不上）：只留下原始报文，状态不动。
-- 钱的痕迹不能丢 —— 与支付回调「金额不符也落支付单」同一个态度。
UPDATE refunds SET notify_payload = $2 WHERE id = $1;

-- name: RevertWholeOrderRefund :execrows
-- 整单退款被驳回或撤回：订单 50 退款中 → 20 已支付（§5 的 (50,20)）。
UPDATE orders SET status = 20 WHERE id = $1 AND status = 50;

-- name: FinishWholeOrderRefund :execrows
-- 整单退款到账：订单 50 退款中 → 60 已退款（§5 的 (50,60)）。
UPDATE orders SET status = 60 WHERE id = $1 AND status = 50;

-- ---------------------------------------------------------------------------
-- 入账（§11「退款与订单状态的联动」）
-- ---------------------------------------------------------------------------

-- name: WriteBackOrderItemRefund :execrows
-- 回写一行订单项的已退件数与金额。条件更新，与库存扣减同构：
-- 影响 0 行 ⇒ 超退，整个入账事务回滚（§11 原文）。chk_item_refund 是第二道。
UPDATE order_items
   SET refunded_qty   = refunded_qty + sqlc.arg(qty),
       refunded_cents = refunded_cents + sqlc.arg(amount)
 WHERE id = sqlc.arg(id)
   AND refunded_qty + sqlc.arg(qty) <= quantity
   AND refunded_cents + sqlc.arg(amount) <= amount_cents - discount_cents;

-- name: AddOrderRefundedCents :exec
-- 累加订单的已退金额。chk_amount 保证它不超过实收，chk_refund_status 随后由
-- RecomputeOrderRefundStatus 对齐。
UPDATE orders SET refunded_cents = refunded_cents + $2 WHERE id = $1;

-- name: CountOrderItemsNotFullyRefunded :one
-- 还有几行没退完。0 表示整单的货都退了 —— 券回补的判据（§11 末段）。
SELECT count(*) FROM order_items WHERE order_id = $1 AND refunded_qty < quantity;

-- name: ReturnCouponForOrder :execrows
-- 整单退款到账时把券退回「未使用」（§11 末段）：3 已使用 → 1。
--
-- 已过期的不退（valid_end_at 在谓词里）：退回一张已过期的券等于什么都没退，
-- 还会让「我的券」里多出一张用不了的未使用券。那种情形由客服补发。
-- chk_user_coupon_state 要求 1 未使用时 order_id / locked_at / used_at 全空，所以三列一起清。
UPDATE user_coupons
   SET status = 1, order_id = NULL, locked_at = NULL, used_at = NULL
 WHERE order_id = $1 AND status = 3 AND valid_end_at > now();

-- name: OtherRefundFreight :one
-- 这一单**别的**退款单已经占掉（在途或已退）的运费，审核裁定退运费时的上限用。
SELECT COALESCE(SUM(freight_cents), 0)::bigint
  FROM refunds
 WHERE order_id = $1 AND id <> $2 AND status IN (10, 20, 30, 40);
