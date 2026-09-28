-- 经营报表（GET /admin/reports/*，契约 Report tag；索引见迁移 00057）。
--
-- 租户由 RLS 过滤，这里一处都不写（check_query_tenancy.py）。
-- 注释里不许有反引号，理由见 db/queries/inventories.sql 的第三条说明。
--
-- ### 口径（契约 ReportWindow / ReportMetrics 是唯一真相源，这里逐字照做）
--
--   销售：orders.paid_at 落在 [window_start, window_end) 且 status 属于已支付的五种
--         （20 待发货、30 已发货、40 已完成、50 退款中、60 已退款）。
--         草稿 0、待支付 10、已关闭 90 一律不计。paid_at IS NOT NULL 那一句不是多余的：
--         它让规划器认出 idx_orders_paid_at 那条部分索引（WHERE paid_at IS NOT NULL）。
--   退款：refunds.status = 40（已到账）且 refunded_at 落在窗口里。按到账时间，不按申请时间。
--
-- 窗口的起止由 service 按店铺时区算好传进来（半开区间），SQL 里不碰时区：
-- 趋势的分桶也一样 —— 桶的边界是 service 算好的一串 timestamptz（edges），
-- 这里用 width_bucket 把每一行落进第几个桶。于是「今天」「按天」「按小时」只有
-- service 那一份实现，数据库的 TimeZone 设置、服务端与数据库的时区库版本差异
-- 都影响不到结果（夏令时那一天有 23 或 25 个小时，也只在 Go 那一侧算一次）。
--
-- ### 同一租户内的范围过滤
--
-- only_region_ids / only_store_ids 与 db/queries/admin_orders.sql 逐字同一个形状：
-- NULL 即不限（管理员、操作员），空数组即一个都不给；值由 service/authz.go 的
-- orderListScope 给出。大区看门店**此刻**所属的大区（stores.region_id），含已软删的门店。
-- store_id / region_id 是调用方的筛选，与范围取交集。
--
-- ### 同一个指标在几条查询里的谓词必须逐字一致
--
-- ReportOrderTotals / ReportOrderBuckets / ReportStoreComparison 的 paid 段 /
-- ReportProductRanking 共用同一段「已支付且落在窗口里」的谓词；三条退款查询同理。
-- 分叉的症状是概览上的销售额与趋势图加起来、与门店对比加起来对不上 ——
-- internal/handler/report_test.go 逐条核对这三处的合计相等。

-- name: ReportOrderTotals :one
-- 概览：窗口内的支付订单数、支付买家数、支付金额。走 idx_orders_paid_at（只读索引）。
SELECT count(*)::bigint                     AS order_count,
       count(DISTINCT o.user_id)::bigint    AS buyer_count,
       COALESCE(sum(o.paid_cents), 0)::bigint AS paid_cents
  FROM orders o
 WHERE o.paid_at IS NOT NULL
   AND o.paid_at >= sqlc.arg(window_start)::timestamptz
   AND o.paid_at <  sqlc.arg(window_end)::timestamptz
   AND o.status IN (20, 30, 40, 50, 60)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(region_id)::bigint IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st WHERE st.region_id = sqlc.narg(region_id)::bigint))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- name: ReportRefundTotals :one
-- 概览：窗口内到账的退款笔数与金额。走 idx_refunds_refunded_at，再按主键连回订单取履约门店。
SELECT count(*)::bigint                        AS refund_count,
       COALESCE(sum(r.amount_cents), 0)::bigint AS refund_cents
  FROM refunds r
  JOIN orders o ON o.id = r.order_id
 WHERE r.status = 40
   AND r.refunded_at >= sqlc.arg(window_start)::timestamptz
   AND r.refunded_at <  sqlc.arg(window_end)::timestamptz
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(region_id)::bigint IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st WHERE st.region_id = sqlc.narg(region_id)::bigint))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- name: ReportOrderBuckets :many
-- 趋势：按桶汇总支付。bucket 从 1 开始，第 i 桶是 [edges[i], edges[i+1])（edges 按升序，
-- 最后一桶的上界是 window_end，由上面那句窗口谓词兜住）。没有数据的桶不出现，由 service 补 0。
SELECT width_bucket(o.paid_at, sqlc.arg(edges)::timestamptz[])::int AS bucket,
       count(*)::bigint                       AS order_count,
       COALESCE(sum(o.paid_cents), 0)::bigint AS paid_cents
  FROM orders o
 WHERE o.paid_at IS NOT NULL
   AND o.paid_at >= sqlc.arg(window_start)::timestamptz
   AND o.paid_at <  sqlc.arg(window_end)::timestamptz
   AND o.status IN (20, 30, 40, 50, 60)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(region_id)::bigint IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st WHERE st.region_id = sqlc.narg(region_id)::bigint))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 GROUP BY 1
 ORDER BY 1;

-- name: ReportRefundBuckets :many
-- 趋势：按桶汇总到账的退款。分桶规则同上。
SELECT width_bucket(r.refunded_at, sqlc.arg(edges)::timestamptz[])::int AS bucket,
       COALESCE(sum(r.amount_cents), 0)::bigint AS refund_cents
  FROM refunds r
  JOIN orders o ON o.id = r.order_id
 WHERE r.status = 40
   AND r.refunded_at >= sqlc.arg(window_start)::timestamptz
   AND r.refunded_at <  sqlc.arg(window_end)::timestamptz
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(region_id)::bigint IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st WHERE st.region_id = sqlc.narg(region_id)::bigint))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 GROUP BY 1
 ORDER BY 1;

-- name: ReportProductRanking :many
-- 商品排行：窗口内支付了的订单的订单行，按商品汇总。订单行没有自己的时间列，窗口只能从订单那边来。
--
-- 写成三段（00061 那一轮按 130 万单的压测数据改的，数字在那份迁移的文件头）：
--
--   win    窗口内、范围内、付过钱的订单 id。由 idx_orders_paid_at 收窄。
--   lines  这些订单的订单行，先按（商品, 订单）归并一次。
--          多出来的那个 BETWEEN 是这一段的关键：win 里订单 id 的最小值与最大值。
--          它在语义上是多余的（下一行的 win.id = oi.order_id 已经蕴含它），但它让
--          订单行的索引（租户, order_id）变成一段**范围扫描** —— 订单 id 是自增的，与支付时间
--          高度相关，30 天的窗口只扫这家店最近那一截订单行。没有它，规划器会把这家店的
--          全部订单行扫一遍再与窗口做哈希连接（订单 id 与支付时间不相关时它退化成的也正是这个）。
--          先按（商品, 订单）归并，是为了下一段的订单数用 count(*) 而不是 count(DISTINCT)：
--          后者要按订单号整体排序，数据量大时落盘，那一步比连接本身还贵。
--   外层   按商品汇总、连商品表取标题与类目、按类目筛、排序取前 N。
--
-- 类目含子孙：按 path 前缀（与 ListProducts 同一个写法），子孙类目**不**过滤 deleted_at ——
-- 卖出去的时候它是有效类目，删掉之后那笔销售仍然属于它的祖先。起点类目本身必须未删除。
--
-- 排序键用 CASE 切换；第二排序键是另一个指标，第三是商品 id，保证并列时结果稳定。
WITH win AS (
    SELECT o.id
      FROM orders o
     WHERE o.paid_at IS NOT NULL
       AND o.paid_at >= sqlc.arg(window_start)::timestamptz
       AND o.paid_at <  sqlc.arg(window_end)::timestamptz
       AND o.status IN (20, 30, 40, 50, 60)
       AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
       AND (sqlc.narg(region_id)::bigint IS NULL
            OR o.store_id IN (SELECT st.id FROM stores st WHERE st.region_id = sqlc.narg(region_id)::bigint))
       AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
            OR o.store_id IN (SELECT st.id FROM stores st
                               WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
       AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
            OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
), span AS (
    SELECT min(id) AS lo, max(id) AS hi FROM win
), lines AS (
    SELECT oi.product_id,
           sum(oi.quantity)                         AS quantity,
           sum(oi.amount_cents - oi.discount_cents) AS amount_cents,
           sum(oi.refunded_qty)                     AS refunded_qty,
           sum(oi.refunded_cents)                   AS refunded_cents
      FROM span
      JOIN order_items oi ON oi.order_id BETWEEN span.lo AND span.hi
      JOIN win ON win.id = oi.order_id
     GROUP BY oi.product_id, oi.order_id
)
SELECT l.product_id,
       p.title,
       p.category_id,
       sum(l.quantity)::bigint       AS quantity,
       sum(l.amount_cents)::bigint   AS amount_cents,
       count(*)::bigint              AS order_count,
       sum(l.refunded_qty)::bigint   AS refunded_quantity,
       sum(l.refunded_cents)::bigint AS refunded_amount_cents
  FROM lines l
  JOIN products p ON p.id = l.product_id
 WHERE (sqlc.narg(category_id)::bigint IS NULL
        OR p.category_id IN (
             SELECT c.id FROM categories c
              WHERE c.path LIKE (SELECT cc.path FROM categories cc
                                  WHERE cc.id = sqlc.narg(category_id)::bigint
                                    AND cc.deleted_at IS NULL) || '%'))
 GROUP BY l.product_id, p.title, p.category_id
 ORDER BY CASE WHEN sqlc.arg(sort_by)::text = 'quantity'
               THEN sum(l.quantity)::bigint
               ELSE sum(l.amount_cents)::bigint END DESC,
          CASE WHEN sqlc.arg(sort_by)::text = 'quantity'
               THEN sum(l.amount_cents)::bigint
               ELSE sum(l.quantity)::bigint END DESC,
          l.product_id
 LIMIT sqlc.arg(row_limit);

-- name: ReportStoreComparison :many
-- 门店对比：范围内全部未删除的门店（没有成交的是 0），外加窗口内有成交或退款的已删除门店。
-- paid / refunded 两段的谓词与 ReportOrderTotals / ReportRefundTotals 逐字一致，
-- 范围过滤放在外层的 stores 上（门店此刻的大区）—— 与两段里用 o.store_id 过滤是同一件事，
-- 写在外层是为了「没有成交的门店也要出现」。
WITH paid AS (
    SELECT o.store_id,
           count(*)::bigint                       AS order_count,
           COALESCE(sum(o.paid_cents), 0)::bigint AS paid_cents
      FROM orders o
     WHERE o.paid_at IS NOT NULL
       AND o.paid_at >= sqlc.arg(window_start)::timestamptz
       AND o.paid_at <  sqlc.arg(window_end)::timestamptz
       AND o.status IN (20, 30, 40, 50, 60)
     GROUP BY o.store_id
), refunded AS (
    SELECT o.store_id,
           COALESCE(sum(r.amount_cents), 0)::bigint AS refund_cents
      FROM refunds r
      JOIN orders o ON o.id = r.order_id
     WHERE r.status = 40
       AND r.refunded_at >= sqlc.arg(window_start)::timestamptz
       AND r.refunded_at <  sqlc.arg(window_end)::timestamptz
     GROUP BY o.store_id
)
SELECT st.id                                 AS store_id,
       st.name                               AS store_name,
       st.code                               AS store_code,
       st.region_id,
       rg.name                               AS region_name,
       (st.deleted_at IS NOT NULL)::boolean  AS deleted,
       COALESCE(pd.order_count, 0)::bigint   AS order_count,
       COALESCE(pd.paid_cents, 0)::bigint    AS paid_cents,
       COALESCE(rf.refund_cents, 0)::bigint  AS refund_cents
  FROM stores st
  JOIN regions rg       ON rg.id = st.region_id
  LEFT JOIN paid pd     ON pd.store_id = st.id
  LEFT JOIN refunded rf ON rf.store_id = st.id
 WHERE (st.deleted_at IS NULL OR pd.store_id IS NOT NULL OR rf.store_id IS NOT NULL)
   AND (sqlc.narg(region_id)::bigint IS NULL OR st.region_id = sqlc.narg(region_id)::bigint)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[]))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR st.id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY COALESCE(pd.paid_cents, 0) - COALESCE(rf.refund_cents, 0) DESC, st.id;

-- name: ReportAlertStores :many
-- 库存预警的门店范围：未软删、落在筛选（store_id / region_id）与员工范围
-- （only_region_ids / only_store_ids）里的门店，带上补名字要用的店名与大区。
--
-- 拆分前这些条件写在 ReportInventoryAlerts 的 JOIN stores 上。本轮（微服务拆分阶段 1a）
-- 预警行由库存服务按「显式的门店 id 列表」取（inventory_svc.sql 的 InvLowStock），
-- 门店那一半的判断留在 core：条件与拆分前逐字一致。
SELECT st.id, st.name, st.region_id
  FROM stores st
 WHERE st.deleted_at IS NULL
   AND (sqlc.narg(store_id)::bigint IS NULL OR st.id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(region_id)::bigint IS NULL OR st.region_id = sqlc.narg(region_id)::bigint)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[]))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR st.id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY st.id;

-- name: ReportAlertExcludedSKUs :many
-- 不该出现在库存预警里的 SKU：软删的 SKU，与软删商品下的 SKU。
-- 拆分前是 ReportInventoryAlerts 里 s.deleted_at IS NULL 与 p.deleted_at IS NULL 两个条件；
-- 本轮作为排除列表递给库存服务（软删是少数，这个列表通常很短）。
SELECT s.id
  FROM skus s
  JOIN products p ON p.id = s.product_id
 WHERE s.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL;

-- name: ReportAlertSKUInfo :many
-- 给库存预警补名字：货号、规格、所属商品与商品名。只查这一页的 SKU。
SELECT s.id, s.sku_code, s.spec_values, s.product_id, p.title AS product_title
  FROM skus s
  JOIN products p ON p.id = s.product_id
 WHERE s.id = ANY(sqlc.arg(sku_ids)::bigint[]);

-- name: ReportSearchTotals :one
-- 搜索概况：检索次数、无结果次数、有点击的次数。走 idx_search_logs_created。
-- 无结果与 scripts/search_metrics.sql 的 zero_rate 同一个口径（ranked_ids 为空，或只回了低于相关度下限的
-- 「猜你想要」—— fallback，00140）；
-- COALESCE 是给 NULL 的 ranked_ids 兜底 —— cardinality(NULL) 是 NULL，不兜的话
-- 那一行既不算有结果、也不算无结果。
SELECT count(*)::bigint AS search_count,
       count(*) FILTER (WHERE (COALESCE(cardinality(l.ranked_ids), 0) = 0 OR l.fallback))::bigint AS zero_result_count,
       count(*) FILTER (WHERE l.clicked_id IS NOT NULL)::bigint AS click_count
  FROM search_logs l
 WHERE l.created_at >= sqlc.arg(window_start)::timestamptz
   AND l.created_at <  sqlc.arg(window_end)::timestamptz;

-- name: ReportSearchTerms :many
-- 搜索词 Top N：去首尾空白、转小写后归并。only_zero 为真时只要出现过无结果的词，
-- 并按无结果次数排（无结果搜索词 Top N）；否则按搜索次数排（热门搜索词 Top N）。
-- 两种都是第二键另一个次数、第三键词本身，并列时结果稳定。
SELECT lower(btrim(l.query))::text AS term,
       count(*)::bigint AS search_count,
       count(*) FILTER (WHERE (COALESCE(cardinality(l.ranked_ids), 0) = 0 OR l.fallback))::bigint AS zero_result_count,
       count(*) FILTER (WHERE l.clicked_id IS NOT NULL)::bigint AS click_count,
       count(*) FILTER (WHERE l.ordered_id IS NOT NULL)::bigint AS order_count
  FROM search_logs l
 WHERE l.created_at >= sqlc.arg(window_start)::timestamptz
   AND l.created_at <  sqlc.arg(window_end)::timestamptz
 GROUP BY 1
HAVING NOT sqlc.arg(only_zero)::boolean
       OR count(*) FILTER (WHERE (COALESCE(cardinality(l.ranked_ids), 0) = 0 OR l.fallback)) > 0
 ORDER BY CASE WHEN sqlc.arg(only_zero)::boolean
               THEN count(*) FILTER (WHERE (COALESCE(cardinality(l.ranked_ids), 0) = 0 OR l.fallback))
               ELSE count(*) END DESC,
          CASE WHEN sqlc.arg(only_zero)::boolean
               THEN count(*)
               ELSE count(*) FILTER (WHERE (COALESCE(cardinality(l.ranked_ids), 0) = 0 OR l.fallback)) END DESC,
          1
 LIMIT sqlc.arg(row_limit);

-- name: ReportSearchLowClickTerms :many
-- 低点击词 Top N：搜过至少 min_count 次、每次都有可信结果（不是无结果词 —— 那归 ReportSearchTerms），
-- 按点击率从低到高排，并列时搜得多的在前。「有结果却没人点」多半是标题没写清、或者货不对路。
-- 口径与上一条相同（去首尾空白、转小写）；点击率 = 有点击的次数 ÷ 搜索次数（一次检索至多算一次点击）。
SELECT lower(btrim(l.query))::text AS term,
       count(*)::bigint AS search_count,
       0::bigint AS zero_result_count,
       count(*) FILTER (WHERE l.clicked_id IS NOT NULL)::bigint AS click_count,
       count(*) FILTER (WHERE l.ordered_id IS NOT NULL)::bigint AS order_count
  FROM search_logs l
 WHERE l.created_at >= sqlc.arg(window_start)::timestamptz
   AND l.created_at <  sqlc.arg(window_end)::timestamptz
 GROUP BY 1
HAVING count(*) >= sqlc.arg(min_count)::bigint
   AND count(*) FILTER (WHERE (COALESCE(cardinality(l.ranked_ids), 0) = 0 OR l.fallback)) = 0
 ORDER BY count(*) FILTER (WHERE l.clicked_id IS NOT NULL)::float8 / count(*),
          count(*) DESC,
          1
 LIMIT sqlc.arg(row_limit);
