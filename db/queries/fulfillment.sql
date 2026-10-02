-- 订单后半程的履约维度：买家取消、后台发货、买家确认收货（数据模型 §5）。
--
-- 每一条改 orders.status 的语句都带着**预期的起点状态**，失配时影响 0 行，
-- 服务层据此回契约里的 409。00033 起库里还有第二道：订单状态机触发器
-- 逐行核对 order_status_transitions，一条忘了写起点状态的 UPDATE 会被它以
-- 23514 拒掉。两道的失效方式不一样，所以两道都要有。
--
-- 租户由 RLS 过滤，这里一处都不写（check_query_tenancy.py）。
-- 注释里不许有反引号，理由见 db/queries/inventories.sql 的第三条说明。

-- name: CancelPendingOrder :execrows
-- 买家取消：10 待支付 → 90 已关闭。
--
-- user_id 进谓词是**越权过滤**（同一家店里 A 买家不能取消 B 买家的单），
-- 不是租户过滤，理由与 GetUserOrderByNo 那条注释一字不差。
--
-- 与超时关单那条 ClaimExpiredPendingOrder 的差别只有两处：不看 expire_at
-- （买家在超时前后都可以主动取消，谁先到由行锁决定），多一个 user_id。
-- 它与支付回调的 SettleOrder 是同一对对手：两者都是带 status = 10 的条件
-- UPDATE，撞在同一行上时恰好一个返回 1。
UPDATE orders SET status = 90
 WHERE order_no = $1 AND user_id = $2 AND status = 10;

-- name: ConfirmOrderReceipt :execrows
-- 买家确认收货：30 已发货 → 40 已完成，记下完成时间。
--
-- 不看 refund_status：部分退款进行中不阻断确认收货（契约明写，两个维度正交）。
UPDATE orders SET status = 40, finished_at = now()
 WHERE order_no = $1 AND user_id = $2 AND status = 30;

-- name: FinishChannelOrder :execrows
-- 渠道单（00320）的 30 → 40：没有 keel 买家，ConfirmOrderReceipt 的 user_id = $2 永远匹配不上，
-- 所以自动确认收货对渠道单走这一条（auto_confirm.go 按 order.Source 分支）。
-- source = 1 在谓词里：这条语句碰不到自营单。
UPDATE orders SET status = 40, finished_at = now()
 WHERE id = $1 AND source = 1 AND status = 30;

-- name: ShipOrder :execrows
-- 后台发货：20 已支付 → 30 已发货，记下发货时间。
--
-- 按 id 而不是 order_no：调用方已经按单号读出订单、判过权限（门店范围），
-- 这里只做那一次原子的状态推进。status = 20 在谓词里，所以「一个订单只发一次」
-- 由这一条保证 —— 第二次发货影响 0 行。
--
-- 不动库存：下单 SAGA 的正向阶段已经扣过了（§5 发货第一条规则）。
UPDATE orders SET status = 30, shipped_at = now()
 WHERE id = $1 AND status = 20;

-- name: InsertShipment :one
-- 落一个发货包裹。**光秃秃的 INSERT，不带 ON CONFLICT**：
-- 运单号重复撞 uk_shipments_tracking，由 Go 侧按约束名挑成
-- ErrTrackingNoDuplicated（契约的 409 tracking-no-duplicated），
-- 其余唯一冲突原样上浮 —— 与 InsertPayment 同一条理由。
INSERT INTO shipments (order_id, carrier_code, tracking_no, created_by)
VALUES ($1, $2, $3, $4)
RETURNING id, carrier_code, tracking_no, status, shipped_at, delivered_at;

-- name: ListOrderShipments :many
-- 这一单的发货包裹（一期每单至多一个），按 id 升序。
SELECT id, carrier_code, tracking_no, status, shipped_at, delivered_at
  FROM shipments
 WHERE order_id = $1
 ORDER BY id;

-- ---------------------------------------------------------------------------
-- 自动确认收货（数据模型 §5 发货第三条规则，00036）
-- ---------------------------------------------------------------------------

-- name: ListAutoConfirmableOrders :many
-- 发货满 N 天、仍停在 30 已发货、而且**没有在途售后**的订单，按发货时间从早到晚。
--
-- 截止时间由调用方算好传进来（now() 减去这家店的 auto_confirm_days）：
-- N 是店铺配置（shop_preferences.auto_confirm_days，00059），由调用方先读出来，不在 SQL 里拼。
--
-- 在途售后（10 待审核 / 20 待买家退货 / 30 退款中）的单**暂停**自动确认：
-- 买家正在退货的时候替他点「确认收货」，等于替他说「货没问题」。
-- 这里的 NOT EXISTS 只是扫描时的预筛，真正的判断在处置事务里、订单行锁之下
-- 再做一次（OrderHasOpenRefund），理由见 service/auto_confirm.go。
--
-- 部分索引 idx_orders_auto_confirm（00036）正好对上这条扫描。
SELECT o.id, o.order_no, o.user_id
  FROM orders o
 WHERE o.status = 30
   AND o.shipped_at < sqlc.arg(cutoff)::timestamptz
   AND NOT EXISTS (SELECT 1 FROM refunds r
                    WHERE r.order_id = o.id AND r.status IN (10, 20, 30))
 ORDER BY o.shipped_at, o.id
 LIMIT sqlc.arg(page_limit);

-- name: OrderHasOpenRefund :one
-- 这一单此刻有没有在途的退款单（10 / 20 / 30）。在订单行锁之下调用才有意义：
-- 申请退款也先锁订单行（LockUserOrderByNo），两边因此串行。
SELECT EXISTS (SELECT 1 FROM refunds r
                WHERE r.order_id = $1 AND r.status IN (10, 20, 30));
