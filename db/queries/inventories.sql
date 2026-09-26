-- name: DeductInventory :one
-- 扣减库存。**一条语句同时回答两个问题**：这一行在本租户可见吗？扣成功了吗？
--
-- 本轮（00020）从两种成因扩成**四种**，判据仍然是那一条。数据模型 §4 那张表：
--
--   ① 这个 SKU 在本租户不可见（别家的，或已软删）→ ErrSKUNotInTenant，
--      422 invalid-request。不该有人重试；**告警**，这是 bug 或攻击。
--   ② 这个门店在本租户不可见（别家的，或已软删）→ ErrStoreNotInTenant，同上。
--   ③ 这家店不卖这件商品（门店排除、**大区排除**、或门店已停业）
--      → ErrSKUNotSoldInStore，422 sku-not-sold-in-store。
--      **重试无用，客户端该换一家店**；不告警 —— 这是运营随时会做的动作
--      与下单之间的正常竞态。
--   ④ 库存不足，**含「这家店根本没有这一行」**→ ErrInsufficientStock，
--      409 insufficient-stock。重试有用（补货之后会成功），埋点算缺货。
--
-- **判定顺序是 ① → ② → ③ → ④，不能反。** 一件被下架的商品通常同时也没库存行，
-- 先判 ④ 会报「缺货」，而用户会一直等一个永远不会来的补货。
--
-- **为什么「没有那一行」归进 ④ 而不是 ③。** 缺行 ≡ 可售 0，不等于「这家店
-- 不卖」（数据模型 §4 把这条写死了）。归进 ③ 会让新开的门店在录库存之前对
-- 每一件商品都回「本店不卖」，而那是产品明确不要的形态；证据是
-- db/queries/products.sql 里早就存在的 SKU-NOSTOCKROW 那一路。
--
-- **sku_visible 的来源本轮从 inventories 改成了 skus。** 旧写法里「别家的
-- SKU」与「没有库存行」本来就是混在一起的（internal/repository/inventory.go
-- 的注释自己写着这一点）。按门店分之后「没有库存行」会从罕见变成常态
-- （新店、新品、缺货清零都会缺行），那个混淆不能带进来。所以这次不只是加两种，
-- 同时拆开了已经混着的那一对。
--
-- **两条 WHERE 都多了 store_id，这是本轮最容易漏、后果最重的一处。**
-- 少写它的后果不是报错，是扣到别的门店头上：没有 store_id 的
-- 「WHERE sku_id = $1」 在多门店之后会匹配到该 SKU 在**所有**门店的行，
-- UPDATE 会把它们一起改掉。
--
-- 只看 「UPDATE ... WHERE sku_id = $1 AND available_qty >= $2」 的 rows_affected，
-- 这两种情形的信号**完全一样**，都是 0。混为一谈的后果很具体：一次攥着别家
-- sku_id 的越权尝试会被当成一次正常的缺货去补偿——补偿是幂等的、日志是正常的，
-- 于是它在监控上表现为库存波动，而不是一次安全事件。反过来，哪天调用方把租户
-- 传错了，症状会是「所有商品都缺货」，排查方向从第一步就是错的。
--
-- 所以这里不是「先加一次预检查」，而是把四个问题压进同一条语句、同一个快照：
-- 四个 CTE 看到的是同一个 MVCC 快照，中间没有别的事务能把行删掉、改掉租户
-- 归属、或者把商品下架，于是「可见但没扣成」与「根本不可见」不会因为竞态互换。
-- 拆成「先 SELECT 确认可见、再 UPDATE」是两次快照，那个窗口是真的。
--
-- **第五种**失败不在这些返回值里：完全没设 「app.merchant_id」 时
-- current_merchant() 直接 RAISE，整条语句以 42501 失败，根本走不到返回值
-- （数据模型 §4 按实测改正过这一条）。它是异常，不是这里的任何一支。
--
-- 刻意不带 WHERE merchant_id —— 租户由 RLS 在数据库层过滤，理由见
-- db/queries/products.sql。本表尤其如此：这条语句要区分的正是「RLS 挡住了」，
-- 应用层再加一遍同样的条件，被挡住的那一行就再也分不清是谁挡的。
--
-- ### 两处写法上的将就，都是 sqlc 逼出来的，不是本来想这么写
--
-- 一、两个 CTE 里的 inventories 各带一个别名（inv / upd）。sqlc 的作用域解析
--     把整条语句的关系拍平成一张表，同一张表出现两次就让每个裸列名都报
--     "column reference ... is ambiguous"，生成直接失败。PostgreSQL 自己没有
--     这个问题（两个 CTE 是各自独立的作用域）。
--
-- 二、末尾那个 「FROM (SELECT 1) a LEFT JOIN deducted d ON true」，换成更直白的
--     「(SELECT left_qty FROM deducted) AS after_available」 语义完全一样，但
--     sqlc 会把这个标量子查询推断成**非空** int32；而它在「库存不足」那一支
--     恰恰是 NULL，于是 row.Scan 会在最该被区分开的那条路径上直接报错。
--     LEFT JOIN 让可空性显式地写在 SQL 里，产物才是 *int32。
--     「(SELECT 1) a」 保证外层恒有且只有一行，所以这条查询永远不会返回 0 行。
--
-- 三、注释里一个反引号都不许有。sqlc 会把这些注释原样抄进产物的 Go 文档注释，
--     而 scripts/check_query_tenancy.py 扫产物时用的是「一对反引号之间是 SQL
--     字面量」这条规则——注释里的反引号会被配对成一段假的字面量，于是
--     「完全没设 app.merchant_id 时」这句话被当成一句应用层租户过滤报出来。
--     多报一次是安全的失败方向，所以不改那个脚本，改这里：用「」代替反引号。
WITH sku_visible AS (
    SELECT s.id FROM skus s WHERE s.id = sqlc.arg(sku_id) AND s.deleted_at IS NULL
), store_visible AS (
    SELECT st.id FROM stores st
     WHERE st.id = sqlc.arg(store_id) AND st.deleted_at IS NULL
), sellable AS (
    SELECT 1 AS ok
      FROM stores st2
      JOIN skus s2 ON s2.id = sqlc.arg(sku_id) AND s2.deleted_at IS NULL
     WHERE st2.id = sqlc.arg(store_id) AND st2.deleted_at IS NULL AND st2.status = 1
       AND NOT EXISTS (SELECT 1 FROM store_product_overrides o
                        WHERE o.store_id = st2.id AND o.product_id = s2.product_id
                          AND o.status = 0)
       AND NOT EXISTS (SELECT 1 FROM region_product_overrides o2
                        WHERE o2.region_id = st2.region_id AND o2.product_id = s2.product_id
                          AND o2.status = 0)
), deducted AS (
    UPDATE inventories upd
       SET available_qty = upd.available_qty - sqlc.arg(qty),
           updated_at    = now()
     WHERE upd.sku_id = sqlc.arg(sku_id) AND upd.store_id = sqlc.arg(store_id)
       AND upd.available_qty >= sqlc.arg(qty)
       AND EXISTS (SELECT 1 FROM sellable)
    RETURNING upd.available_qty AS left_qty
)
SELECT (SELECT count(*) FROM sku_visible)   AS sku_visible_rows,
       (SELECT count(*) FROM store_visible) AS store_visible_rows,
       (SELECT count(*) FROM sellable)      AS sellable_rows,
       (SELECT count(*) FROM deducted)      AS deducted_rows,
       d.left_qty                           AS after_available
  FROM (SELECT 1) a
  LEFT JOIN deducted d ON true;

-- name: RestoreInventory :one
-- 补偿：回补库存。没有「数量不够」这一支，所以不需要上面那组 CTE ——
-- 零行就只可能是「这一行在本租户不可见」，而那同样是 bug 或攻击，不是业务分支。
--
-- :one 让零行变成 pgx.ErrNoRows，调用方拿不到一个能被当成「正常」的返回值。
-- 写成 :exec 的话 rows_affected = 0 会被静默吞掉，而补偿路径恰恰是最容易
-- 直接攥着 sku_id 回滚的地方（数据模型 §4 点名说了这一条）。
--
-- **store_id 本轮加上，从 orders 读，不从 gid 解析**（数据模型 §4）：
-- 补偿要回补的是当初扣减的那一家店，而那家店写在订单行上。
--
-- **补偿刻意不重新判 ③。** 一件在下单后被运营下架的商品，它的回补必须照样
-- 执行 —— 补的是一件已经扣掉的货，「现在还卖不卖」与「当时扣没扣」是两个问题。
-- 这一处不对称是有意的，不是漏写。
UPDATE inventories
   SET available_qty = available_qty + sqlc.arg(qty),
       updated_at    = now()
 WHERE sku_id = sqlc.arg(sku_id) AND store_id = sqlc.arg(store_id)
RETURNING available_qty;
