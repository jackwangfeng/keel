-- 库存服务自己的查询（微服务拆分阶段 1a，docs/电商系统-微服务拆分方案.md）。
--
-- 这个文件是库存服务仓储（internal/repository/inventory_svc.go 的 InventoryStore）
-- 唯一的 SQL 来源，守一条硬规矩：**只碰 inventories、inventory_logs、activity_stocks 三张表**
-- （外加屏障表 barrier，那条语句在 repository/saga.go，两个库各一张）。
-- 反过来，core 的查询一条都不碰这三张表 —— internal/repository 的 TestQueryFilesStayOnTheirSideOfTheSplit
-- 从 db/queries 源头核对这两个方向。
-- 不 JOIN skus / products / stores / 两张上下架覆盖表 / 价格视图 —— 拆分部署下库存库里
-- 根本没有那些表。凡是要 core 数据才能下的判断（SKU 在不在、软删了没有、这家店卖不卖、
-- 门店是否软删、商品名门店名），一律由 core 先判完、或者拿 id 回来自己补，再调这里。
--
-- 和这个目录里别的文件一样，**一个 merchant_id 都没有**：三张表都带 merchant_id 列、
-- 走 merchant_id = current_merchant() 的 RLS（00020），租户由库存服务按请求头
-- X-Keel-Merchant-ID 开事务时设进去（rpc.RequireTenant → repository.WithTenant）。
--
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。
--
-- 阶段 1b 搬过来的：下单 SAGA 的扣减与补偿（拆分前 inventories.sql 的 DeductInventory /
-- RestoreInventory 与 orders.sql 的 AppendInventoryLog）、关单与退款回补的流水核对
-- （拆分前 orders.sql 的 OrderSkuNetInventoryChange / CountInventoryLogsForOrder）、
-- 活动配额（拆分前 promotions.sql 对 promotion_skus.sold_qty 的扣与放）。
-- 预警通知的上下文不搬：商品名、门店名是 core 的数据，由 core 自己查（notifications.sql 的
-- GetLowStockContext），库存服务只回 id 与数。

-- name: InvStoreStock :many
-- 一家门店、一批 SKU 的水位（详情页 SKU、购物车行、检索的 in_stock、门店库存清单）。
-- **缺行不回**：调用方把「没回来的 sku_id」读成可售 0（数据模型 §4：缺行 ≡ 可售 0），
-- 这里不替它补零行 —— 补了就分不出「录过、是 0」与「从没录过」，而门店库存清单的
-- updated_at 恰恰要靠这一点回落到 SKU 自己的时间。走主键 (sku_id, store_id)。
SELECT inv.sku_id, inv.available_qty, inv.warning_qty, inv.updated_at
  FROM inventories inv
 WHERE inv.store_id = sqlc.arg(store_id)
   AND inv.sku_id = ANY(sqlc.arg(sku_ids)::bigint[]);

-- name: InvSKUTotals :many
-- 一批 SKU 跨全部门店的合计（后台商品 / SKU 页，租户视角）。口径与拆分前
-- admin_skus.sql 的 LATERAL 逐字一致：可售取 sum，预警线取 max（阈值不是总量）。
-- 缺行（一家店都没录过）不回，调用方记 0 / 0。
SELECT inv.sku_id,
       sum(inv.available_qty)::int AS available_qty,
       max(inv.warning_qty)::int   AS warning_qty
  FROM inventories inv
 WHERE inv.sku_id = ANY(sqlc.arg(sku_ids)::bigint[])
 GROUP BY inv.sku_id;

-- name: InvHealthySKUIDs :many
-- 这家店里水位**高于**预警线的 SKU。门店库存清单的 low_stock_only 用它的补集：
-- 「低库存」= 未软删 SKU 里除掉这批的全部 —— 缺行的 SKU 是 0 ≤ 0，算低库存，
-- 与拆分前那条 LEFT JOIN 的判据逐点相同。取补集而不是直接列「低库存的行」，
-- 是因为缺行的那些在这张表里根本没有行可列。
SELECT inv.sku_id
  FROM inventories inv
 WHERE inv.store_id = sqlc.arg(store_id)
   AND inv.available_qty > inv.warning_qty;

-- name: InvLowStock :many
-- 库存预警（报表）。i.available_qty <= i.warning_qty 必须与 idx_inventories_warning
-- 的谓词逐字一致，规划器才认得出那条部分索引。
--
-- 门店范围由 core 给成**显式的 id 列表**（未软删、落在筛选与员工范围内的门店），
-- 软删 SKU / 软删商品下的 SKU 由 core 给成排除列表 —— 拆分前那三个 JOIN 做的判断
-- 就是这两件，库存库里没有那几张表，只能由 core 判完把结果递过来。
-- 排除列表通常很短（软删是少数），门店列表是门店数量级。
SELECT i.store_id, i.sku_id, i.available_qty, i.warning_qty
  FROM inventories i
 WHERE i.available_qty <= i.warning_qty
   AND i.store_id = ANY(sqlc.arg(store_ids)::bigint[])
   AND NOT (i.sku_id = ANY(sqlc.arg(exclude_sku_ids)::bigint[]))
 ORDER BY i.available_qty - i.warning_qty, i.available_qty, i.store_id, i.sku_id
 LIMIT sqlc.arg(row_limit);

-- name: InvCountLowStock :one
-- 条件必须与 InvLowStock 逐字一致。
SELECT count(*)::bigint
  FROM inventories i
 WHERE i.available_qty <= i.warning_qty
   AND i.store_id = ANY(sqlc.arg(store_ids)::bigint[])
   AND NOT (i.sku_id = ANY(sqlc.arg(exclude_sku_ids)::bigint[]));

-- name: InvSetStock :one
-- 比较并设置（后台 PUT .../inventory 两条）。拆分前是两条语句：
-- admin_skus.sql 的 SetInventoryByCAS（纯 UPDATE，缺行 404）与
-- scoped_catalog.sql 的 SetStoreInventoryByCAS（upsert，缺行且 expected = 0 时首次录入）。
-- 合成一条，差别收进 allow_insert 一个开关：
--
--   allow_insert = false  单店捷径那条的语义：缺行 → current_rows = 0，调用方报 404。
--   allow_insert = true   按门店那条的语义：缺行且 expected = 0 → 插入；
--                         缺行且 expected 不为 0 → written_rows = 0，调用方报 409，current 为 0。
--
-- 两条原先各自带的「SKU 可见且未软删」「这家店卖这件商品」判定**不在这里了**：
-- 那要 JOIN skus / stores / 覆盖表，由 core 在调这条之前判完（404）。判与写之间于是
-- 隔着一次服务调用，而不是同一个快照 —— 窗口里被下架的商品会多录一次库存，
-- 那是一个无害的方向（没有地方会用到它），代价写在 service/inventory_admin.go。
--
-- 骨架与拆分前那条 upsert 相同：一条 INSERT ... ON CONFLICT DO UPDATE ... WHERE，
-- 判定与写落在唯一索引的同一个点上；cur 与 wrote 看到的是同一个快照；CTE 里的表带别名、
-- RETURNING 列加 w_ 前缀、末尾 FROM (SELECT 1) anchor LEFT JOIN，都是 sqlc 逼出来的写法
-- （标量子查询会被推断成非空，而它在 CAS 失败那一支恰恰是 NULL）。
WITH cur AS (
    SELECT inv.available_qty, inv.warning_qty, inv.updated_at
      FROM inventories inv
     WHERE inv.sku_id = sqlc.arg(sku_id) AND inv.store_id = sqlc.arg(store_id)
), wrote AS (
    INSERT INTO inventories (sku_id, store_id, available_qty, warning_qty)
    SELECT sqlc.arg(sku_id), sqlc.arg(store_id), sqlc.arg(available_qty),
           COALESCE(sqlc.narg(warning_qty)::int, 0)
     WHERE EXISTS (SELECT 1 FROM cur)
        OR (sqlc.arg(allow_insert)::boolean AND sqlc.arg(expected_available_qty)::int = 0)
    ON CONFLICT (sku_id, store_id) DO UPDATE
       SET available_qty = excluded.available_qty,
           warning_qty   = COALESCE(sqlc.narg(warning_qty)::int, inventories.warning_qty),
           updated_at    = now()
     WHERE inventories.available_qty = sqlc.arg(expected_available_qty)::int
    RETURNING available_qty AS w_available_qty,
              warning_qty   AS w_warning_qty,
              updated_at    AS w_updated_at
)
SELECT (SELECT count(*) FROM cur)   AS current_rows,
       (SELECT count(*) FROM wrote) AS written_rows,
       c.available_qty   AS current_available_qty,
       c.warning_qty     AS current_warning_qty,
       c.updated_at      AS current_updated_at,
       w.w_available_qty AS new_available_qty,
       w.w_warning_qty   AS new_warning_qty,
       w.w_updated_at    AS new_updated_at
  FROM (SELECT 1) anchor
  LEFT JOIN cur   c ON true
  LEFT JOIN wrote w ON true;

-- name: InvAdjustStock :one
-- 相对调整（后台 POST .../inventory/adjustments 两条）：available_qty += delta，结果不得为负。
-- 与拆分前 scoped_catalog.sql 的 AdjustStoreInventory 同一个骨架，去掉了 sellable 那个 CTE
-- （判定挪到 core，理由同 InvSetStock）。三处逼出来的写法照旧：
-- 插入值是 GREATEST(delta, 0)（提议行先过 chk_qty_nonneg，早于冲突判定）；插入的前提是
-- 「快照里有这一行，或者 delta > 0」（缺行扣减一行都不写，不把「从没录过」变成「录过、是 0」）；
-- DO UPDATE 里加的是 inventories.available_qty + delta，WHERE 里的非负判定看的是冲突那一行的
-- 最新提交版本（READ COMMITTED 下 ON CONFLICT 会锁住并重读它）。
--
-- written_rows = 0 → 扣完会变负（409 inventory-insufficient），current 是快照里的水位（缺行为 0）。
-- 流水由仓储在同一个事务里紧接着写（InvAppendManualLog）。
WITH cur AS (
    SELECT inv.available_qty, inv.warning_qty, inv.updated_at
      FROM inventories inv
     WHERE inv.sku_id = sqlc.arg(sku_id) AND inv.store_id = sqlc.arg(store_id)
), wrote AS (
    INSERT INTO inventories (sku_id, store_id, available_qty, warning_qty)
    SELECT sqlc.arg(sku_id), sqlc.arg(store_id), GREATEST(sqlc.arg(delta)::int, 0), 0
     WHERE EXISTS (SELECT 1 FROM cur) OR sqlc.arg(delta)::int > 0
    ON CONFLICT (sku_id, store_id) DO UPDATE
       SET available_qty = inventories.available_qty + sqlc.arg(delta)::int,
           updated_at    = now()
     WHERE inventories.available_qty + sqlc.arg(delta)::int >= 0
    RETURNING available_qty AS w_available_qty,
              warning_qty   AS w_warning_qty,
              updated_at    AS w_updated_at
)
SELECT (SELECT count(*) FROM wrote) AS written_rows,
       c.available_qty   AS current_available_qty,
       c.warning_qty     AS current_warning_qty,
       c.updated_at      AS current_updated_at,
       w.w_available_qty AS new_available_qty,
       w.w_warning_qty   AS new_warning_qty,
       w.w_updated_at    AS new_updated_at
  FROM (SELECT 1) anchor
  LEFT JOIN cur   c ON true
  LEFT JOIN wrote w ON true;

-- name: InvLockBizID :exec
-- 按 biz_id 串行化（事务级 advisory lock，提交或回滚即释放）。手工调整按「adj:员工:幂等键」锁；
-- 阶段 1b 起下单扣减、SAGA 补偿、关单释放按订单号锁，退款回补按退款单号锁 —— 那几条路径都是
-- 「先看这张单的流水、再决定写什么」，两步之间必须没有同一张单的另一次写插进来（见 inventory
-- 包 saga.go 的文件头）。键空间不会撞：订单号、退款单号、「adj:」前缀互不相同。
--
-- 相对调整的幂等落在库存服务这一侧（「同一个 biz_id 只生效一次」）：core 在结果未知时
-- 会拿同一把 Idempotency-Key、也就是同一个 biz_id 再调一次。两次调用可能并发到达
-- （第一次还在路上、第二次已经发出），「先查流水有没有这个 biz_id、没有再写」是两步，
-- 不锁的话两次都会查到「没有」然后各加一遍。
--
-- 不用唯一索引挡：biz_id 在下单那几类流水里本来就不唯一（一单多行），
-- 给手工调整单开一条部分唯一索引要一条迁移，而这把锁只在手工调整这条低频路径上出现。
-- 键是 biz_id 的 64 位哈希：不同租户、不同 biz_id 撞到同一个键只会让两次调整短暂排队。
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(biz_id)::text, 0));

-- name: InvFindManualLog :one
-- 这个 biz_id 的手工流水（biz_type = 5）。相对调整的重放判定用：有就按它回原结果，
-- 不再加一遍。走 idx_inv_logs_biz。
SELECT l.sku_id, l.store_id, l.change_qty, l.after_available, l.created_at
  FROM inventory_logs l
 WHERE l.biz_id = sqlc.arg(biz_id) AND l.biz_type = 5
 ORDER BY l.id
 LIMIT 1;

-- name: InvAppendManualLog :exec
-- 手工调整那一行流水（biz_type = 5）。拆分前在 orders.sql（AppendManualInventoryLog），
-- 随手工改库存一起搬到库存服务。biz_type 写死在语句里：这条语句只为手工调整存在。
-- biz_id 由 core 拼好（相对调整「adj:员工:幂等键」，覆盖「set:员工:随机串」）。
INSERT INTO inventory_logs (sku_id, store_id, change_qty, biz_type, biz_id,
                            before_available, after_available, reason)
VALUES (sqlc.arg(sku_id), sqlc.arg(store_id), sqlc.arg(change_qty), 5, sqlc.arg(biz_id),
        sqlc.arg(before_available), sqlc.arg(after_available), sqlc.narg(reason));

-- name: InvInitSKU :execrows
-- 给一个新 SKU 建它的第一行库存（拆分前是 admin_skus.sql 的 CreateInventoryRow，
-- 在建 SKU 的同一个事务里）。core 在 SKU 那一行**提交之后**调它，门店由 core 定
-- （默认门店；没有默认门店就不调）。
--
-- 两个守卫，都是为了让它可以被放心地重复调用（core 在结果未知、或幂等重放时会再调）：
--
--   ON CONFLICT DO NOTHING   同一家店已经有这一行了（上一次其实成功了）→ 不覆盖。
--                            覆盖的话，第一次建行之后卖掉的那几件会被初始值抹掉。
--   NOT EXISTS 任何一家店     这个 SKU 在**别的**店已经有库存行了 → 不建。防的是
--                            「第一次建在旧默认店、之后换了默认店、再重放」时
--                            在新默认店凭空多出一份初始库存。
INSERT INTO inventories (sku_id, store_id, available_qty, warning_qty)
SELECT sqlc.arg(sku_id), sqlc.arg(store_id), sqlc.arg(available_qty), sqlc.arg(warning_qty)
 WHERE NOT EXISTS (SELECT 1 FROM inventories ex WHERE ex.sku_id = sqlc.arg(sku_id))
ON CONFLICT (sku_id, store_id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 阶段 1b：下单扣减 / 补偿 / 关单释放 / 退款回补（全部按单号幂等，见 inventory 包 saga.go）
-- ---------------------------------------------------------------------------

-- name: InvLockStoreStock :many
-- 下单扣减的第一步：按 sku_id 升序锁住这家店这批 SKU 的库存行，并读出水位。
--
-- 先锁、再判、最后写，而不是逐行「条件 UPDATE、0 行即缺货」：一单多行时，第三行缺货的话前两行
-- 已经扣了，要么靠保存点回滚，要么让整个事务失败 —— 而扣减被拒要**提交**一行拒绝流水
-- （saga.go 的「扣减被拒为什么是成功」）。锁住之后判定与写入之间没有别人能改这几行，
-- 判过了就一定扣得成。缺行不回（缺行 ≡ 可售 0，调用方判成不足）。
--
-- 按 sku_id 升序拿锁：与补偿、关单释放、退款回补同一个全局顺序，两张共享 SKU 的单
-- 一个扣一个补不会互相等成死锁。库存行一律先于活动配额行加锁（InvLockActivity 在其后）。
SELECT inv.sku_id, inv.available_qty, inv.warning_qty
  FROM inventories inv
 WHERE inv.store_id = sqlc.arg(store_id)
   AND inv.sku_id = ANY(sqlc.arg(sku_ids)::bigint[])
 ORDER BY inv.sku_id
   FOR UPDATE;

-- name: InvLockActivity :one
-- 锁住一行活动配额并读出配额与已售。查不到即「还没同步配额」—— 调用方按配额不足拒绝
-- （宁可少卖，不超卖；00075 文件头）。
SELECT a.quota, a.sold
  FROM activity_stocks a
 WHERE a.promotion_id = sqlc.arg(promotion_id) AND a.sku_id = sqlc.arg(sku_id)
   FOR UPDATE;

-- name: InvDeductLocked :one
-- 扣门店库存。只在 InvLockStoreStock 锁住并判过「够」之后调，所以 WHERE 里的
-- available_qty >= qty 是第二道（chk_qty_nonneg 是第三道），正常路径上恒命中一行。
-- :one 让「0 行」变成 pgx.ErrNoRows：那意味着前面的判定与这里不一致，是 bug，不是缺货。
UPDATE inventories
   SET available_qty = available_qty - sqlc.arg(qty)::int,
       updated_at    = now()
 WHERE sku_id = sqlc.arg(sku_id) AND store_id = sqlc.arg(store_id)
   AND available_qty >= sqlc.arg(qty)::int
RETURNING available_qty;

-- name: InvAddStock :one
-- 加回门店库存（SAGA 补偿、关单释放、退款回补）。写成 upsert 而不是纯 UPDATE：
-- 这几条路径加回的都是「已经扣掉的货」，行不在（理论上不会：行从不删除）时凭空报错会让
-- 补偿无限重试、关单任务进死信，而正确的结果就是那一行现在有 qty 件。
-- 插入值是 qty（缺行 ≡ 0，0 + qty）。
INSERT INTO inventories (sku_id, store_id, available_qty, warning_qty)
VALUES (sqlc.arg(sku_id), sqlc.arg(store_id), sqlc.arg(qty)::int, 0)
ON CONFLICT (sku_id, store_id) DO UPDATE
   SET available_qty = inventories.available_qty + sqlc.arg(qty)::int,
       updated_at    = now()
RETURNING available_qty;

-- name: InvAddActivitySold :exec
-- 扣活动配额（已售 + qty）。只在 InvLockActivity 锁住并判过配额之后调。
UPDATE activity_stocks
   SET sold = sold + sqlc.arg(qty)::int
 WHERE promotion_id = sqlc.arg(promotion_id) AND sku_id = sqlc.arg(sku_id);

-- name: InvReleaseActivitySold :execrows
-- 放回活动配额（SAGA 补偿、关单释放）。带 sold >= qty：放回不该把计数放成负数；
-- 0 行由调用方记日志（这个 SKU 已被移出活动之类，正常路径上走不到 —— 卖出过的不能移除）。
UPDATE activity_stocks
   SET sold = sold - sqlc.arg(qty)::int
 WHERE promotion_id = sqlc.arg(promotion_id) AND sku_id = sqlc.arg(sku_id)
   AND sold >= sqlc.arg(qty)::int;

-- name: InvAppendBizLog :exec
-- 下单扣减（1）、SAGA 补偿（2）、超时关单释放（3）、退款回补（4）、买家取消释放（6）、
-- 扣减被拒（7）的流水。biz_id 是订单号（退款回补是退款单号）。reason 只有扣减被拒写（拒绝码）。
-- 手工调整（5）走 InvAppendManualLog，那条把 biz_type 写死在语句里。
INSERT INTO inventory_logs (sku_id, store_id, change_qty, biz_type, biz_id,
                            before_available, after_available, reason)
VALUES (sqlc.arg(sku_id), sqlc.arg(store_id), sqlc.arg(change_qty), sqlc.arg(biz_type),
        sqlc.arg(biz_id), sqlc.arg(before_available), sqlc.arg(after_available), sqlc.narg(reason));

-- name: InvBizTrail :many
-- 一张单（订单号或退款单号）的全部流水，连同那一行库存**此刻**的预警线。
--
-- 三个用途，都是「按流水判」：关单释放与 SAGA 补偿算每个 SKU 还有多少没补回来（净值），
-- 扣减判「这一单已经被关单释放过了没有」（有释放流水就拒绝），core 的收尾分支判扣减被拒与
-- 跌破预警线（拆分前在扣减的同一个事务里判，拆分后 core 在库存分支之后来问，见
-- service/order_saga.go 的收尾分支）。孤儿草稿关单前的核对也用它（有没有任何一行）。
-- 预警线取此刻的值：它只在后台改库存时变，扣减到收尾之间那几毫秒里变过的概率可以忽略。
-- 走 idx_inv_logs_biz。
SELECT l.sku_id, l.store_id, l.biz_type, l.change_qty, l.before_available, l.after_available,
       l.reason, COALESCE(i.warning_qty, 0)::int AS warning_qty
  FROM inventory_logs l
  LEFT JOIN inventories i ON i.sku_id = l.sku_id AND i.store_id = l.store_id
 WHERE l.biz_id = sqlc.arg(biz_id)
 ORDER BY l.id;

-- ---------------------------------------------------------------------------
-- 阶段 1b：活动配额（activity_stocks，00075）
-- ---------------------------------------------------------------------------

-- name: InvActivityBySKUs :many
-- 一批 SKU 身上的活动配额（计价、商品标签、购物车）。一个 SKU 可以在几个活动里，全回。
-- 走 idx_activity_stocks_sku。
SELECT a.promotion_id, a.sku_id, a.quota, a.sold
  FROM activity_stocks a
 WHERE a.sku_id = ANY(sqlc.arg(sku_ids)::bigint[])
 ORDER BY a.promotion_id, a.sku_id;

-- name: InvActivityByPromotions :many
-- 一批活动的全部配额（后台活动列表 / 详情）。走主键前缀（租户之后是 promotion_id）。
SELECT a.promotion_id, a.sku_id, a.quota, a.sold
  FROM activity_stocks a
 WHERE a.promotion_id = ANY(sqlc.arg(promotion_ids)::bigint[])
 ORDER BY a.promotion_id, a.sku_id;

-- name: InvLockPromotionActivity :many
-- 整组设配额的第一步：锁住这个活动现有的全部配额行。「卖出过的 SKU 不能移出活动 /
-- 配额不能低于已售」在这把锁之下判，判与写同一个快照 —— 与之竞争的是下单扣减
-- （InvLockActivity 锁同一批行），扣减要么排在判定之前（判定看到新的已售），要么之后。
SELECT a.sku_id, a.quota, a.sold
  FROM activity_stocks a
 WHERE a.promotion_id = sqlc.arg(promotion_id)
 ORDER BY a.sku_id
   FOR UPDATE;

-- name: InvUpsertActivityQuota :exec
-- 设一个 SKU 的活动配额；已售不动（已兑现的配额不清零）。
-- 冲突目标写约束名而不是列：列里有 merchant_id，这个目录里不许出现那个词。
-- chk_activity_stock_qty 兜底「配额低于已售」（调用方已经先判过、报了人话）。
INSERT INTO activity_stocks (promotion_id, sku_id, quota)
VALUES (sqlc.arg(promotion_id), sqlc.arg(sku_id), sqlc.arg(quota))
    ON CONFLICT ON CONSTRAINT activity_stocks_pkey
    DO UPDATE SET quota = EXCLUDED.quota, updated_at = now();

-- name: InvDeleteActivityExcept :execrows
-- 删掉不在新名单里的 SKU。卖出过的（sold > 0）不删：调用方已经先拒过一次，这里的条件是第二道。
DELETE FROM activity_stocks
 WHERE promotion_id = sqlc.arg(promotion_id)
   AND sku_id <> ALL(sqlc.arg(keep_sku_ids)::bigint[])
   AND sold = 0;

-- ---------------------------------------------------------------------------
-- 阶段 2：对账（core 的 InventoryReconcileService 驱动，只读）
-- ---------------------------------------------------------------------------

-- name: InvListStockKeys :many
-- 本租户全部库存行的键，按主键 (sku_id, store_id) 键集分页：从 (after_sku_id, after_store_id)
-- 之后取 row_limit 行。对账拿去与 core 的 skus / stores 比，找库存库里的孤儿行
-- （拆分之后两边没有外键，SKU / 门店不在了库存行也不会跟着走）。
-- 用行值比较而不是 OFFSET：表在对账期间照常被写，OFFSET 会漏行或重行，键集不会。
SELECT inv.sku_id, inv.store_id
  FROM inventories inv
 WHERE (inv.sku_id, inv.store_id) > (sqlc.arg(after_sku_id)::bigint, sqlc.arg(after_store_id)::bigint)
 ORDER BY inv.sku_id, inv.store_id
 LIMIT sqlc.arg(row_limit);

-- name: InvStockoutDays :many
-- 一家门店、一批 SKU 在最近 days 天里「收盘时可售 ≤ 0」的天数（AI 经营 M9 的补货计算用，service/restock.go）。
--
-- 天按店铺时区切（tz 由 core 传，库存服务不知道店铺时区）：第 i 天的收盘 = 当地午夜往回 i 天再加一天，
-- 今天的收盘取 now()。某天收盘时的水位按下面的顺序取第一个有的：
--   ① 收盘前最后一条流水的 after_available；
--   ② 收盘后第一条流水的 before_available（那天没动过，水位就是下一次变动前的样子）；
--   ③ 现在的可售（一条流水都没有）；④ 0（连库存行都没有 —— 缺行 ≡ 可售 0）。
-- 为什么要它：日均销量的分母只该算有货的天。断过货的 SKU 按日历天数平均，会越断越少补。
WITH req_skus AS (
    SELECT unnest(sqlc.arg(sku_ids)::bigint[]) AS sku_id
), day_idx AS (
    SELECT gs AS i FROM generate_series(0, sqlc.arg(days)::int - 1) AS gs
), day_ends AS (
    SELECT req_skus.sku_id,
           LEAST(now(), ((date_trunc('day', now() AT TIME ZONE sqlc.arg(tz)::text) - make_interval(days => day_idx.i)
                          + interval '1 day') AT TIME ZONE sqlc.arg(tz)::text)) AS day_end
      FROM req_skus CROSS JOIN day_idx
)
SELECT day_ends.sku_id::bigint AS sku_id,
       count(*) FILTER (WHERE COALESCE(
           (SELECT l.after_available FROM inventory_logs l
             WHERE l.store_id = sqlc.arg(store_id) AND l.sku_id = day_ends.sku_id AND l.created_at < day_ends.day_end
             ORDER BY l.created_at DESC, l.id DESC LIMIT 1),
           (SELECT l.before_available FROM inventory_logs l
             WHERE l.store_id = sqlc.arg(store_id) AND l.sku_id = day_ends.sku_id AND l.created_at >= day_ends.day_end
             ORDER BY l.created_at, l.id LIMIT 1),
           (SELECT inv.available_qty FROM inventories inv
             WHERE inv.store_id = sqlc.arg(store_id) AND inv.sku_id = day_ends.sku_id),
           0) <= 0)::int AS stockout_days
  FROM day_ends
 GROUP BY day_ends.sku_id;

-- name: InvPurgeLogsBefore :execrows
-- 库存流水按保留期分批清理（service/retention.go，默认留 180 天），删法同 db/queries/retention.sql
-- 的文件头：带 LIMIT 的 ctid 子查询，走 idx_inv_logs_created (merchant_id, created_at)（00173）。
--
-- 删老流水对别处判断的影响，逐个看过（InvBizTrail 的三个用途 + 另外两处）：
--   · 关单释放 / SAGA 补偿按流水**净值**算还欠多少（inventory/saga.go 的 putBack）：流水删了
--     净值是 0，什么都不补 —— 不会重复加库存；
--   · 退款回补（inventory/local_orders.go 的 RestockForRefund）按「有没有这张退款单的回补流水」
--     判重放：**这是唯一一处删了会出错的**。同一张退款单的回补在它的流水被删之后再来一次，
--     会再加一遍库存。它只来自 outbox 的重试，正常在分钟级完成；死信是永久保留、要人处理的，
--     保留期（默认 180 天）必须长于「死信最晚会被人重新投递」的时间 —— 别配到 30 天以下；
--   · 手工调整判重（InvLockBizID / InvFindManualLog）只在同一次操作的重试窗口里有意义；
--   · InvStockoutDays 取某天收盘水位：收盘前的流水删了，会落到「收盘后第一条流水的
--     before_available」或「现在的可售」，两者都等于那天收盘的水位（中间没有别的变动）。
DELETE FROM inventory_logs
 WHERE ctid = ANY(ARRAY(SELECT l.ctid FROM inventory_logs l
                         WHERE l.created_at < sqlc.arg(before)::timestamptz
                         LIMIT sqlc.arg(batch)::int));
