-- 下单主链路的查询：试算、建单、SAGA 两个分支各自要读写的东西。
--
-- 全文没有一处 WHERE 写租户，也没有一处 INSERT 写 merchant_id：租户由 RLS 在
-- 数据库层过滤，写入那一列由 DEFAULT current_merchant() 补（00013）。理由见
-- db/queries/products.sql 与 scripts/check_query_tenancy.py 的文件头 ——
-- 应用层再加一遍条件之后，「RLS 到底有没有生效」就再也测不出来了。
--
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。

-- name: ListSKUsForPricing :many
-- 按一批 sku_id 取定价与快照素材。**试算与真下单共用这一条**。
--
-- 共用不是为了少写代码，是为了让「试算的钱」与「成交的钱」不可能不一样：
-- 两条查询各写一份的话，哪天有人只给其中一条补上「下架商品不可售」，
-- 用户会在试算里看到一个价，在订单里看到另一个价，而两边各自都是自洽的。
--
-- 三个过滤条件决定了「什么叫可售」：
--   · s.status = 1     SKU 本身在售；
--   · p.status = 1     商品在架（草稿商品的 SKU 不可下单）；
--   · p.deleted_at IS NULL  商品没被软删。
--
-- 过滤掉的行不会出现在结果里，于是调用方拿到的行数少于请求的 sku 数 ——
-- 服务层据此报「这些 SKU 不可售」，而不是悄悄少算一行钱。
--
-- 不在这里 JOIN inventories：试算是**无副作用的金额试算**，不是可售性承诺。
-- 把库存并进来会让试算看起来像一次预留，而 SAGA 的正向阶段才是真正的判定点
-- （架构 §5：超卖为零、少卖存在，正是因为扣减发生在那里而不是这里）。
SELECT s.id, s.product_id, s.spec_values, s.price_cents, s.image_url,
       p.title
  FROM skus s
  JOIN products p ON p.id = s.product_id
 WHERE s.id = ANY(sqlc.arg(sku_ids)::bigint[])
   AND s.status = 1
   AND p.status = 1
   AND p.deleted_at IS NULL;

-- name: GetUserAddress :one
-- 取当前买家名下的一条收货地址，用于拍成 orders.receiver_snapshot。
--
-- user_id 进 WHERE 是**越权过滤**，不是租户过滤：RLS 只保证这一行属于本店，
-- 不保证它属于这个买家。契约里 address_id 用自增 id 对外，防越权靠的正是
-- 服务端按 user_id 强制过滤（数据模型 §9 的约定 4）。
--
-- deleted_at IS NULL：地址是软删的，删掉的地址不能再用来下单。
-- 历史订单不受影响 —— 它存的是快照，不是外键。
SELECT id, receiver_name, phone, province, city, district, street, detail,
       region_code, postal_code
  FROM user_addresses
 WHERE id = $1
   AND user_id = $2
   AND deleted_at IS NULL;

-- name: CreateOrderDraft :one
-- 落一笔**创建中**的订单（status = 0），在提交 SAGA 之前。
--
-- 顺序的理由写在 00013 的文件头：分支只拿到 (gid, branch_id, op) 三个字符串，
-- 「扣哪些 SKU、各扣几件」推不出来，只能先落库。而 0 这个状态让「已落库」
-- 不等于「已成交」—— 状态 0 的订单永远不会出现在任何响应里。
--
-- 租户列不出现在这条语句里（00013 的 DEFAULT current_merchant()）。
INSERT INTO orders (order_no, user_id, status, goods_amount_cents, freight_cents,
                    discount_cents, payable_cents, receiver_snapshot, remark, expire_at)
VALUES ($1, $2, 0, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, order_no, status, goods_amount_cents, freight_cents, discount_cents,
          payable_cents, paid_cents, refunded_cents, refund_status, expire_at, created_at;

-- name: CreateOrderItem :exec
-- 订单项快照（数据模型 §5：下单即快照）。商品改价改名不影响历史订单。
INSERT INTO order_items (order_id, sku_id, product_id, title_snapshot, spec_snapshot,
                         image_snapshot, price_cents, quantity, amount_cents, discount_cents)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: GetOrderByNo :one
-- 按对外编号取订单。SAGA 的两个分支都靠它把「自己要处理哪一单」找回来 ——
-- order_no 是从 gid 里解出来的，而 gid 是协调器重放时唯一还在的东西。
--
-- 它在 RLS 之下：分支跑在 dtm.TenantContextFromGID 造出来的租户上下文里，
-- 所以拿着 A 店的 gid 去查 B 店的订单，结果是查不到，而不是查到了别人的单。
SELECT id, order_no, user_id, status, goods_amount_cents, freight_cents,
       discount_cents, payable_cents, paid_cents, refunded_cents, refund_status,
       expire_at, created_at
  FROM orders
 WHERE order_no = $1;

-- name: ListOrderItemsForBranch :many
-- 取一笔订单的全部行，供库存分支重建「扣减意图」。
--
-- 这条查询就是硬约束二的落点：分支拿不到业务载荷，扣减意图只能从库里读回来。
-- 按 sku_id 排序而不是 id：两个分支（正向与补偿）必须按同一个顺序拿行锁，
-- 否则两笔互相交叉的订单在高并发下能互相死锁。
SELECT sku_id, quantity
  FROM order_items
 WHERE order_id = $1
 ORDER BY sku_id;

-- name: PromoteOrderDraft :execrows
-- 建单分支的正向：0 创建中 → 10 待支付。
--
-- WHERE 带上 status = 0 而不是无条件赋值：这条语句会被协调器重放，
-- 而一笔已经被补偿关到 90 的订单不该被一次迟到的正向重新开成待支付。
-- 屏障已经挡住重复执行，这一条是第二道 —— 两道的失效方式不一样。
--
-- 返回受影响行数：0 行说明这一单不在本租户可见，或者状态已经不是 0，
-- 两者都不该被当成成功。
UPDATE orders SET status = 10 WHERE order_no = $1 AND status = 0;

-- name: CloseOrder :execrows
-- 建单分支的补偿：把订单关掉。
--
-- 允许从 0 和 10 两个状态进来。10 是正常的补偿路径（正向已经把它推到待支付），
-- 0 是正向分支本身失败时的路径。已经是 90 的行不再动 —— 关单是幂等的，
-- 而重放一次补偿不该把行数变成 0 之外的任何东西去误导调用方。
UPDATE orders SET status = 90 WHERE order_no = $1 AND status IN (0, 10);

-- name: AppendInventoryLog :exec
-- 库存流水（数据模型 §4，对账用）。
--
-- 它不是可选的装饰：正向扣减与补偿回补跑完之后，available_qty 回到了原值，
-- 和「从来没扣过」一模一样。只有这两行流水能把这两种情形分开 ——
-- 而「补偿到底跑没跑」正是 SAGA 最需要能被证伪的那件事。
--
-- biz_type：1 下单扣减 / 2 SAGA 补偿回补 / 3 超时关单释放 / 4 退款回补 / 5 手工调整。
-- biz_id 存订单号。
INSERT INTO inventory_logs (sku_id, change_qty, biz_type, biz_id,
                            before_available, after_available)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ClaimIdempotencyKey :execrows
-- 抢占式插入（数据模型 §12）。**主键就是那把锁。**
--
-- 返回 1 = 抢到了，执行业务；返回 0 = 已存在，调用方读出那一行按三态处理。
--
-- 这里**要**写冲突目标，和 barrier 那条相反（00008：keel_app 在 barrier 上只有
-- INSERT，带冲突目标要额外的 SELECT 权，会当场 42501）。这张表四权齐全，
-- 而且必须带：不带的话，任何别的约束冲突也会被静默吞掉，
-- 而抢占插入唯一想沉默跳过的是主键撞车。
--
-- 24 小时足够覆盖客户端的重试窗口（§12）。
INSERT INTO idempotency_keys (scope, user_id, idem_key, request_hash, expire_at)
VALUES ($1, $2, $3, $4, now() + interval '24 hours')
ON CONFLICT (scope, user_id, idem_key) DO NOTHING;

-- name: GetIdempotencyKey :one
-- 读出已存在的那一行，用于判定重放 / 409 处理中 / 422 键被复用。
SELECT request_hash, status, response_code, response_body
  FROM idempotency_keys
 WHERE scope = $1 AND user_id = $2 AND idem_key = $3;

-- name: FinishIdempotencyKey :exec
-- 把存档写回去。status：1 成功 / 2 失败。
--
-- 失败也存档并回放（§12 的边界选择：最保守，绝不会重复扣款）。
-- 确需重试的场景让客户端换一个新键。
UPDATE idempotency_keys
   SET status = $4, response_code = $5, response_body = $6
 WHERE scope = $1 AND user_id = $2 AND idem_key = $3;

-- ---------------------------------------------------------------------------
-- 超时补偿定时任务（Task 6）。
--
-- 两类行、两条扫描、两种处置，逐条的理由写在 internal/service/sweep.go。
-- 这里要说的只有一条形状上的：**扫描与处置是分开的两步，而处置那一步带着
-- 与扫描完全相同的谓词**。扫出来的订单号在被处理之前可能已经被支付回调改掉了
-- （那是一次真实的竞态，不是理论），所以真正决定「这一单归谁」的是处置那条
-- UPDATE 的 rows_affected，不是扫描的结果。
-- ---------------------------------------------------------------------------

-- name: ListExpiredPendingOrders :many
-- 第一类：正常的超时未支付。它们进过 SAGA，库存已经真实扣减，要回补。
--
-- 走 idx_orders_status_expire（00006，WHERE status = 10）。
-- 按 expire_at 升序：过期最久的先处理，否则一个持续入单的租户能让最老的那批
-- 永远排在后面。
SELECT id, order_no
  FROM orders
 WHERE status = 10 AND expire_at < now()
 ORDER BY expire_at
 LIMIT $1;

-- name: ListExpiredDraftOrders :many
-- 第二类：孤儿草稿（00013 文件头那笔明写的欠账）。它们**没进过 SAGA**，
-- 一件库存都没扣，所以只关单、不回补。
--
-- 走 idx_orders_draft_expire（00015，WHERE status = 0）。
SELECT id, order_no
  FROM orders
 WHERE status = 0 AND expire_at < now()
 ORDER BY expire_at
 LIMIT $1;

-- name: ClaimExpiredPendingOrder :execrows
-- 原子占位：把一笔超时未支付的订单关到 90。**返回 1 才算这一单归我。**
--
-- 谓词里 status = 10 与 expire_at < now() 两个条件缺一不可：
--   · status = 10  —— 支付回调刚把它推到 20 时，这里必须落空。少了它，
--     一笔已付款的订单会被关掉，而库存还会被「回补」一次 —— 超卖。
--   · expire_at    —— 不是冗余：扫描与处置之间订单的 expire_at 理论上可被延长
--     （续期），而一条无条件的关单会把续期后的订单照关不误。
--
-- 回补库存与这条 UPDATE 在**同一个事务**里，所以「关了单却没回补」与
-- 「回补了却没关单」两种半成品都不存在。
UPDATE orders SET status = 90
 WHERE order_no = $1 AND status = 10 AND expire_at < now();

-- name: CloseExpiredDraftOrder :execrows
-- 孤儿草稿的处置：0 → 90。**不回补库存**，理由见 sweep.go。
--
-- 同样带条件：一笔草稿在扫描与处置之间可能被 SAGA 的建单分支推到 10
-- （一个跑了很久才回来的重放），那时它不再是孤儿，这里必须落空。
UPDATE orders SET status = 90
 WHERE order_no = $1 AND status = 0 AND expire_at < now();

-- name: CountInventoryLogsForOrder :one
-- 这一单在库存流水上留下过几行。
--
-- 它只有一个用途，而且是**不变量的守卫**，不是业务查询：关闭孤儿草稿之前，
-- 核对一下这一单真的一件库存都没扣过。
--
-- 那条不变量今天由编排顺序保证（建单在前、库存在后，见 service/order.go 的
-- 文件头）：订单还停在 0，说明建单分支的正向没成功，而库存分支排在它后面，
-- 连开始都没开始。**但这是一条靠「另一个文件里的常量」维持的不变量** ——
-- 哪天有人把 sagaSteps 的两行对调，孤儿清理就会开始静默地漏掉库存回补，
-- 而水位、订单状态、日志全都正常。所以这里花一次点查把它变成一次响亮的失败。
SELECT count(*) FROM inventory_logs WHERE biz_id = $1;
