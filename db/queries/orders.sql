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
--   · s.deleted_at IS NULL  SKU 没被软删（M4 补，00018 给 skus 加了这一列）；
--   · p.status = 1     商品在架（草稿商品的 SKU 不可下单）；
--   · p.deleted_at IS NULL  商品没被软删。
--
-- 少了 s.deleted_at 那一条的后果很具体：商家删掉一个规格之后它仍然下得了单，
-- 而它已经不在任何一个后台视图里 —— 商家看不到这笔订单卖的是什么。
--
-- 过滤掉的行不会出现在结果里，于是调用方拿到的行数少于请求的 sku 数 ——
-- 服务层据此报「这些 SKU 不可售」，而不是悄悄少算一行钱。
--
-- 不在这里 JOIN inventories：试算是**无副作用的金额试算**，不是可售性承诺。
-- 把库存并进来会让试算看起来像一次预留，而 SAGA 的正向阶段才是真正的判定点
-- （架构 §5：超卖为零、少卖存在，正是因为扣减发生在那里而不是这里）。
--
-- ### 本轮（00020）取价口换成了 sku_prices_by_store 视图
--
-- 这是「试算与下单共用同一份定价」那条纪律在三层定价下的兑现：视图是全仓库
-- 唯一一处写 COALESCE(门店价, 大区价, 基准价) 的地方，而商品列表、详情、
-- 检索结果也都经过它。取 skus.price_cents 的话，**列表显示的价与下单成交的价
-- 会在「这家店定了自己的价」的那些商品上分叉** —— 而那正是最不可能被夹具
-- 覆盖到的一种。
--
-- 两条 NOT EXISTS 也一起进来：一件这家店（或它所在大区）不卖的商品
-- 在这里就不该有价。少了它们，试算会给出一个金额，而 SAGA 的库存分支随后以
-- ErrSKUNotSoldInStore 拒掉整单 —— 用户看到的是「试算成功、下单失败」，
-- 而两次调用之间什么都没变。可售性的判定必须和金额在同一条查询里。
--
-- JOIN 视图而不是 LEFT JOIN：视图对每一个 (未软删门店 × 未软删 SKU) 都恰好
-- 有一行，缺行意味着门店或 SKU 已经不在了，而那时这一行本来就不该可售 ——
-- 少掉的行会让调用方拿到「这些 SKU 不可售」，那正是对的答案。
SELECT s.id, s.product_id, s.spec_values, v.price_cents, s.image_url,
       p.title
  FROM skus s
  JOIN products p ON p.id = s.product_id
  JOIN sku_prices_by_store v ON v.sku_id = s.id AND v.store_id = sqlc.arg(store_id)
 WHERE s.id = ANY(sqlc.arg(sku_ids)::bigint[])
   AND s.status = 1
   AND s.deleted_at IS NULL
   AND p.status = 1
   AND p.deleted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
                    WHERE ro.region_id = sqlc.arg(region_id)
                      AND ro.product_id = p.id AND ro.status = 0)
   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
                    WHERE so.store_id = sqlc.arg(store_id)
                      AND so.product_id = p.id AND so.status = 0);

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
--
-- **store_id / region_id / store_snapshot 本轮（00020）一起落下来。**
-- 三列都 NOT NULL，理由在数据模型 §4 末尾：SAGA 分支只拿到三个字符串，
-- 它读回订单行拿到一个 NULL 的 store_id 时无路可走 —— 既不能猜默认店
-- （会把单扣到另一家店去），也不能失败（订单已经落库了）。
--
-- region_id 从 stores 现读一次，**存成订单自己的列，不靠 stores.region_id 推**：
-- 门店可以被调到另一个大区去，而这一单的价格是按当时那个大区算的。
-- 要能事后回答「这个价是怎么来的」，就不能让它取决于一张随后会变的表。
--
-- store_snapshot 只放展示字段（门店名、大区名、地址、电话），**不放 id**——
-- 放了就会有人去 GROUP BY 它，而聚合该走 store_id 那一列（真外键、有索引）。
--
-- 写成 INSERT ... SELECT FROM stores 而不是让应用把这几个值传进来：
-- 快照与外键必须来自**同一行**、同一个快照。应用先查一次门店再把字段拼进
-- INSERT，两步之间那家店可以改名 —— 于是 store_id 指着 A，快照写着 A 的旧名字，
-- 而两者都「看起来正常」。门店不存在或已软删时这条语句插 0 行，
-- 由 :one 变成 pgx.ErrNoRows，调用方翻成 422。
INSERT INTO orders (order_no, user_id, store_id, region_id, store_snapshot,
                    status, goods_amount_cents, freight_cents,
                    discount_cents, payable_cents, receiver_snapshot, remark, expire_at)
SELECT sqlc.arg(order_no), sqlc.arg(user_id), st.id, st.region_id,
       jsonb_build_object('store_name', st.name, 'region_name', r.name,
                          'address', st.address, 'phone', st.phone),
       0, sqlc.arg(goods_amount_cents), sqlc.arg(freight_cents),
       sqlc.arg(discount_cents), sqlc.arg(payable_cents),
       sqlc.arg(receiver_snapshot), sqlc.narg(remark), sqlc.arg(expire_at)
  FROM stores st
  JOIN regions r ON r.id = st.region_id
 WHERE st.id = sqlc.arg(store_id) AND st.deleted_at IS NULL
RETURNING id, order_no, store_id, region_id, status, goods_amount_cents, freight_cents,
          discount_cents, payable_cents, paid_cents, refunded_cents, refund_status,
          expire_at, created_at;

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
--
-- paid_at / shipped_at / finished_at 是买家侧读接口（GET /orders 与
-- GET /orders/{order_no}）补上来的三列。契约的 Order 里它们都声明过，而在此之前
-- 这条查询根本没 SELECT 它们 —— 于是「付款时间」在支付成功之后的响应里
-- 依然缺席，客户端只能显示一个没有时间的「已支付」。
-- 三列一起取而不是只取 paid_at：shipped_at 是「发货后 N 天自动确认收货」倒计时
-- 的起点（契约里明写），finished_at 同理属于同一张时间线，分两次加意味着
-- 这条查询与它的领域类型要被改两遍。
SELECT id, order_no, user_id, store_id, region_id, status,
       goods_amount_cents, freight_cents,
       discount_cents, payable_cents, paid_cents, refunded_cents, refund_status,
       expire_at, paid_at, shipped_at, finished_at, created_at
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
--
-- **store_id 本轮（00020）补上，它不是可选的冗余。** 流水的唯一用途是对账，
-- 而对账口径从「这个商家这个 SKU 扣了多少」变成了「这家店这个 SKU 扣了多少」——
-- before_available / after_available 现在记的是某一家门店的水位，
-- 不写下是哪一家，同一个 SKU 在五家店的流水会交织成一条谁也对不平的序列。
INSERT INTO inventory_logs (sku_id, store_id, change_qty, biz_type, biz_id,
                            before_available, after_available)
VALUES ($1, $2, $3, $4, $5, $6, $7);

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
--
-- 主体是 (subject_kind, subject_id)，不是 user_id：1 买家 users.id /
-- 2 后台 staff.id。两张表的 id 来自同一种自增序列，共用一列的话
-- staff_id = 7 与 user_id = 7 会撞在同一行上（00023 的文件头）。
INSERT INTO idempotency_keys (scope, subject_kind, subject_id, idem_key, request_hash, expire_at)
VALUES ($1, $2, $3, $4, $5, now() + interval '24 hours')
ON CONFLICT (scope, subject_kind, subject_id, idem_key) DO NOTHING;

-- name: GetIdempotencyKey :one
-- 读出已存在的那一行，用于判定重放 / 409 处理中 / 422 键被复用。
SELECT request_hash, status, response_code, response_body
  FROM idempotency_keys
 WHERE scope = $1 AND subject_kind = $2 AND subject_id = $3 AND idem_key = $4;

-- name: FinishIdempotencyKey :exec
-- 把存档写回去。status：1 成功 / 2 失败。
--
-- 失败也存档并回放（§12 的边界选择：最保守，绝不会重复扣款）。
-- 确需重试的场景让客户端换一个新键。
UPDATE idempotency_keys
   SET status = $5, response_code = $6, response_body = $7
 WHERE scope = $1 AND subject_kind = $2 AND subject_id = $3 AND idem_key = $4;

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
-- store_id 一起取：回补要回补到**当初扣减的那一家店**，而这条清扫路径跑在
-- 任何请求之外（定时任务），它对那一单的记忆只有这几列。
--
-- 走 idx_orders_status_expire（00006，WHERE status = 10）。
-- 按 expire_at 升序：过期最久的先处理，否则一个持续入单的租户能让最老的那批
-- 永远排在后面。
SELECT id, order_no, store_id
  FROM orders
 WHERE status = 10 AND expire_at < now()
 ORDER BY expire_at
 LIMIT $1;

-- name: ListExpiredDraftOrders :many
-- 第二类：孤儿草稿（00013 文件头那笔明写的欠账）。它们**没进过 SAGA**，
-- 一件库存都没扣，所以只关单、不回补。
--
-- 走 idx_orders_draft_expire（00015，WHERE status = 0）。
SELECT id, order_no, store_id
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

-- name: ReleaseIdempotencyKey :execrows
-- 撤销一次幂等键抢占。**只撤处理中的那些**（status = 0）。
--
-- 它只有一个调用点：SubmitSaga 失败时（service/order.go 第二段）。那一刻
-- 抢占记录与订单草稿都已经提交了，而 SAGA 一个分支都没跑过 —— 不撤的话，
-- 客户端拿同一把钥匙重试会一直撞 409 处理中，直到 24 小时后 expire_at 过期。
-- 而 Idempotency-Key 的语义恰恰是「同一个逻辑请求的重试用同一把钥匙」，
-- 所以那是把客户端**正确的**行为锁死了。
--
-- 用 DELETE 而不是「写一条失败存档」：存档是给「业务真的执行过」的失败用的
-- （§12），而这里业务一次都没执行。留一条失败存档会让重试拿到一个回放的错误，
-- 那比 409 更糟 —— 客户端会以为下单真的失败过。
--
-- `status = 0` 在谓词里不是冗余：撤销与「另一个并发请求刚刚把它推到成功」
-- 之间有窗口（虽然同一把钥匙上不该有两个并发请求），而删掉一条已成功的存档
-- 等于把一笔已经建成的订单的幂等证据抹掉，下一次重试会建出第二笔订单。
DELETE FROM idempotency_keys
 WHERE scope = $1 AND subject_kind = $2 AND subject_id = $3 AND idem_key = $4 AND status = 0;

-- ---------------------------------------------------------------------------
-- 支付回调（Task 7）。
-- ---------------------------------------------------------------------------

-- name: SettleOrder :execrows
-- 支付成功：10 待支付 → 20 已支付，记下实收与到账时间。
--
-- `status = 10` 在谓词里，与上面那条关单互为对手：两者都是条件 UPDATE，
-- 撞在同一行上时由行锁排队，**恰好一个返回 1**。这就是「超时任务与支付回调
-- 撞车」那条竞态的全部处理 —— 不是在应用层加锁，是让数据库回答「谁赢了」。
--
-- 返回 0 行不是错误，是一种要**大声记下来**的事实：钱已经到账了，而这一单
-- 已经不在待支付上（被超时关掉了，或者已经付过一次了）。处置见 service/payment.go。
--
-- paid_cents 直接赋值而不是累加：本轮不支持部分支付（契约里也没有），
-- 累加会让一笔重复到账把 chk_amount 的 `refunded_cents <= paid_cents` 撑出
-- 一个不该有的空间。
UPDATE orders SET status = 20, paid_cents = $2, paid_at = $3
 WHERE order_no = $1 AND status = 10;

-- name: InsertPayment :one
-- 支付单落库。**一条光秃秃的 INSERT，刻意不带 ON CONFLICT**。
--
-- 幂等由 uk_payments_channel_txn（00014）兜底：重复回调撞唯一冲突，
-- 由 Go 侧按约束名把它挑成 repository.ErrDuplicateChannelTxn，其余 23505
-- 原样上浮。写成 `ON CONFLICT DO NOTHING` 会顺带吞掉 payment_no 撞车 ——
-- 那是熵源坏了，必须炸出来。完整论证在 00014 的文件头。
--
-- 租户列不出现在这条语句里（00014 的 DEFAULT current_merchant()）。
INSERT INTO payments (payment_no, order_id, channel, amount_cents, status,
                      channel_txn_id, notify_payload, paid_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, payment_no, status;

-- ---------------------------------------------------------------------------
-- 买家侧读接口：GET /orders（我的订单）与 GET /orders/{order_no}（订单详情）。
--
-- 这一段里每一条查询都带 user_id，而这**不是**租户过滤，别把它和
-- check_query_tenancy.py 挡的那件事混起来：
--
--   · 租户过滤（merchant_id）由 RLS 做，应用层再加一遍会让 RLS 变得测不出来；
--   · 买家过滤（user_id）RLS 做不了 —— 策略只认 current_merchant()，
--     它管不到「同一家店里 A 买家和 B 买家」这一层。
--
-- 也就是说：去掉 user_id，跨租户仍然是安全的（RLS 还在），而**同一家店的任意
-- 买家可以凭一个自增页码翻遍全店的订单**。这是本组接口唯一一处只能靠查询条件
-- 守住的越权面，与 GetUserAddress 上那条注释说的是同一件事（数据模型 §9 约定 4）。
-- ---------------------------------------------------------------------------

-- name: ListUserOrders :many
-- 我的订单，一页。
--
-- status <> 0 不是可选的：0 是「创建中」，是 SAGA 还没跑完的中间态
-- （00013 与 service/order.go 的文件头）。它一旦出现在「我的订单」里，用户会看到
-- 一笔既不能支付也不能取消的订单，而它可能在下一秒被补偿关掉。
--
-- 两个筛选条件用 sqlc.narg 做成可空参数：传 NULL 就是不筛。写成两条查询
-- （筛的和不筛的）的话，「total 与 items 用的是同一套谓词」这条性质要靠人维护，
-- 而它一旦破掉的症状是客户端一直翻到一页空的。
--
-- 排序 created_at DESC 之后再按 id DESC：同一毫秒内建的两笔订单在只按时间排序时
-- 顺序是不确定的，而不确定的顺序会让同一页在两次请求之间变样 —— 分页最经典的
-- 那种「第二页又看到了第一页的那一单」。
SELECT id, order_no, user_id, store_id, region_id, status,
       goods_amount_cents, freight_cents,
       discount_cents, payable_cents, paid_cents, refunded_cents, refund_status,
       expire_at, paid_at, shipped_at, finished_at, created_at
  FROM orders
 WHERE user_id = $1
   AND status <> 0
   AND (sqlc.narg(status)::smallint IS NULL OR status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(refund_status)::smallint IS NULL
        OR refund_status = sqlc.narg(refund_status)::smallint)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountUserOrders :one
-- 契约里 PageMeta.total 是必填字段，而它是**全部条数**，不是本页条数。
--
-- 谓词必须与 ListUserOrders 逐字一致（user_id、status <> 0、两个可空筛选），
-- 理由与 CountProducts 那条一样，只是后果更重：这里少一个 user_id，
-- total 数的就是全店的订单数，于是「我的订单」页脚会告诉每个买家这家店一共
-- 有多少单 —— 一次不需要读到任何一行别人的数据就完成的信息泄露。
SELECT count(*)
  FROM orders
 WHERE user_id = $1
   AND status <> 0
   AND (sqlc.narg(status)::smallint IS NULL OR status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(refund_status)::smallint IS NULL
        OR refund_status = sqlc.narg(refund_status)::smallint);

-- name: GetUserOrderByNo :one
-- 订单详情 / 发起支付共用：按单号取**当前买家自己**的订单。
--
-- 与上面那条 GetOrderByNo 分开而不是加个参数：那一条是 SAGA 分支与支付回调用的，
-- 它们跑在没有买家身份的上下文里（协调器重放、渠道回调），给它加一个 user_id
-- 参数只会逼那两处传一个假的。两条查询、两个调用面，谁也伪造不了对方的条件。
--
-- 查不到与「不是你的」回同一个 404：order_no 是 72 bit 随机不可枚举的，
-- 分开报会把它变成一个「这个单号存不存在」的判定器。
SELECT id, order_no, user_id, store_id, region_id, status,
       goods_amount_cents, freight_cents,
       discount_cents, payable_cents, paid_cents, refunded_cents, refund_status,
       expire_at, paid_at, shipped_at, finished_at, created_at
  FROM orders
 WHERE order_no = $1
   AND user_id = $2
   AND status <> 0;

-- name: ListOrderItemsForDetail :many
-- 订单详情里的行。与 ListOrderItemsForBranch 分开：那一条只取 (sku_id, quantity)
-- 并按 sku_id 排序，因为它是**库存分支拿行锁的顺序**（改了会死锁）。
-- 这一条取的是展示用的全部快照列，按 id 排序 —— 两者的排序有完全不同的理由，
-- 合成一条之后改其中一个会静默改掉另一个。
SELECT id, sku_id, product_id, title_snapshot, spec_snapshot, image_snapshot,
       price_cents, quantity, amount_cents, discount_cents, refunded_qty
  FROM order_items
 WHERE order_id = $1
 ORDER BY id;

-- name: GetOrderReceiver :one
-- 下单时拍下的收货信息快照（契约的 OrderDetail.receiver）。
--
-- 单独一条查询而不是并进 GetUserOrderByNo：receiver_snapshot 是一整块 JSONB，
-- 而那条查询被订单列表**逐行**复用 —— 列表里没有任何地方要展示收货地址，
-- 每页多搬 20 份快照是白搬的。
SELECT receiver_snapshot
  FROM orders
 WHERE id = $1;

-- name: GetOrderStoreSnapshot :one
-- 下单时拍下的门店 / 大区展示快照（契约的 OrderDetail.store）。
--
-- 与 GetOrderReceiver 分开、也与 GetUserOrderByNo 分开，理由一字不差：
-- 它是一整块 JSONB，而订单列表逐行复用那条查询，列表里没有任何地方要展示
-- 门店地址与电话。
--
-- **读快照而不是 JOIN stores**：门店会改名、会搬家、会换大区，
-- 三个月前那单的详情页要显示当时那个名字（数据模型 §5）。
-- JOIN 出来的是今天的名字，而那正是快照存在要避免的东西。
SELECT store_snapshot
  FROM orders
 WHERE id = $1;

-- name: ListPaymentsForOrder :many
-- 这一单的全部支付尝试（契约的 OrderDetail.payments）。
--
-- 走 idx_payments_order（00014）。按 id 升序：对账时人要看的是「先发生了什么」。
SELECT payment_no, channel, amount_cents, status, paid_at
  FROM payments
 WHERE order_id = $1
 ORDER BY id;
