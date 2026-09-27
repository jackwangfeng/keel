-- 后台商品域的写路径（契约 /admin/products*）。M4 Task 2。
--
-- 全文**刻意不带 WHERE merchant_id** —— 租户由 RLS 在数据库层过滤，理由见
-- db/queries/products.sql 的文件头。写路径上这条纪律比读路径还重要一层：
-- 入库的 merchant_id 由列默认值 current_merchant() 填（迁移 00018），
-- 于是生成出来的 Go 函数签名里**根本没有这个参数**，
-- 「拿 A 店的上下文往 B 店名下建商品」连编译都编不出来。
--
-- 注释里一个反引号都不许有：sqlc 会把它们原样抄进产物的 Go 文档注释，
-- 而 scripts/check_query_tenancy.py 扫产物时按「一对反引号之间是 SQL 字面量」
-- 配对，注释里的反引号会拼出一段假的字面量。用「」代替。

-- name: AdminListProducts :many
-- 后台列表。与前台的 ListProducts 是两条查询，不是一条加几个参数：
-- 前台那条把 「status = 1 AND deleted_at IS NULL」 写死在 SQL 里，
-- 而后台的默认视图恰恰相反 —— 不传筛选条件时返回**全部未软删**的商品，
-- 含草稿与已下架。让前台那条长出可选筛选，等于给一条 security 为空的接口
-- 开一个「传个参数就能看草稿」的口子。
--
-- 三个筛选都用 sqlc.narg + 「IS NULL OR」，缺省即不筛。契约刻意没给 status
-- 默认值，理由写在端点上：默认值被代入会静默改变「返回哪些行」。
--
-- 价格区间与总库存都是**现算**的（00019 删掉了那两列冗余价格，
-- 并连带删掉了维护它们的 RecalcProductAggregates；total_stock 那一列还在，
-- 但从此没有任何一处写它，所以后台这三个数一律从 skus / inventories 现算）。
-- 公式与前台 ListProducts 那条逐字一致：未软删的 SKU，COALESCE 到 0。
--
-- **total_stock 本轮（微服务拆分阶段 1a）搬出了这条查询**：inventories 归库存服务，
-- service/admin_catalog.go 用 AdminListLiveSKUsOfProducts 取这一页商品的未软删 SKU、
-- 向库存服务要这批 SKU 的跨门店合计，再按商品加总 —— 公式不变（未软删 SKU 的
-- available_qty 之和，缺行记 0），只是加总从 SQL 挪到了 Go。
--
-- 为什么 total_stock 也跟着现算而不是继续读列：RecalcProductAggregates 一删，
-- 那一列就回到了 db/queries/search.sql 文件头点名的状态 ——
-- 「一句永远不会被纠正的谎」。后台那个数是商家用来决定要不要补货的，
-- 让它读一列没人维护的汇总比不显示更糟。
SELECT p.id, p.category_id, p.brand_id, p.title, p.subtitle, p.description,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       p.sales_count,
       p.status, p.published_at, p.deleted_at, p.created_at, p.updated_at,
       p.freight_template_id
  FROM products p
  LEFT JOIN LATERAL (
        SELECT min(s.price_cents) AS min_price, max(s.price_cents) AS max_price
          FROM skus s
         WHERE s.product_id = p.id AND s.deleted_at IS NULL
       ) agg ON TRUE
 WHERE (sqlc.narg(status)::smallint IS NULL OR p.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(category_id)::bigint IS NULL
        OR p.category_id = sqlc.narg(category_id)::bigint)
   AND (sqlc.arg(include_deleted)::boolean OR p.deleted_at IS NULL)
 ORDER BY p.id DESC
 LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: AdminCountProducts :one
-- 条件必须与 AdminListProducts 逐字一致：两边只要有一处不同，total 数的就不是
-- 列表实际会分出来的那批行，而客户端据此算出的总页数会少，最后几页谁也翻不到。
--
-- 它**不带**那个 LATERAL：价格与库存是 SELECT 出来的东西，不是筛选条件，
-- 而这条查询一列都不返回。「逐字一致」说的是 WHERE 子句，不是 FROM。
SELECT count(*)
  FROM products
 WHERE (sqlc.narg(status)::smallint IS NULL OR status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(category_id)::bigint IS NULL
        OR category_id = sqlc.narg(category_id)::bigint)
   AND (sqlc.arg(include_deleted)::boolean OR deleted_at IS NULL);

-- name: AdminGetProduct :one
-- 后台详情**不过滤 deleted_at**：AdminProduct 有 deleted_at 字段，
-- 后台列表也有 include_deleted 参数 —— 软删商品在后台是看得见的一等公民。
-- 前台那条 GetProduct 的谓词（status = 1 AND deleted_at IS NULL）一个都不能少，
-- 两者的差别正是这一层存在的理由。
--
-- 价格与库存现算，公式与 AdminListProducts 逐字一致（00019）。
-- 这条查询同时是后台**唯一**一处把一件商品完整读出来的地方：
-- CreateProduct / UpdateProduct / SetProductPublication 写完都回读它，
-- 于是「后台看到的商品长什么样」只有一个答案。
SELECT p.id, p.category_id, p.brand_id, p.title, p.subtitle, p.description,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       p.sales_count,
       p.status, p.published_at, p.deleted_at, p.created_at, p.updated_at,
       p.freight_template_id
  FROM products p
  LEFT JOIN LATERAL (
        SELECT min(s.price_cents) AS min_price, max(s.price_cents) AS max_price
          FROM skus s
         WHERE s.product_id = p.id AND s.deleted_at IS NULL
       ) agg ON TRUE
 WHERE p.id = $1;

-- name: AdminListLiveSKUsOfProducts :many
-- 一批商品的未软删 SKU（含停售）。后台商品列表 / 详情算 total_stock 用：
-- 与拆分前 LATERAL 里 「s.deleted_at IS NULL」 那个条件逐字一致。
SELECT s.product_id, s.id
  FROM skus s
 WHERE s.product_id = ANY(sqlc.arg(product_ids)::bigint[])
   AND s.deleted_at IS NULL;

-- name: CreateProduct :one
-- 新建即草稿：status 走列默认值 0，published_at 保持 NULL。
-- **请求体里没有 status**（契约），所以这里也不给它参数位 ——
-- 一个能在创建时指定状态的入参，就是一条绕过 publication 端点的路。
--
-- **只 RETURNING id**，整行由 repository 紧接着用 AdminGetProduct 回读
-- （同一个事务，读到的就是刚写的）。这是 00019 之后的形状：价格与库存要
-- LEFT JOIN LATERAL 才算得出来，而 INSERT ... RETURNING 里没有那个 join 的
-- 位置。硬填两个 0 也能过 —— 新建的商品确实一个 SKU 都没有 —— 但那会让
-- 「价格区间怎么来的」在这个仓库里有两个答案，而第二个答案是一个常量。
INSERT INTO products (category_id, brand_id, title, subtitle, description, freight_template_id)
VALUES (sqlc.arg(category_id), sqlc.narg(brand_id), sqlc.arg(title),
        sqlc.narg(subtitle), sqlc.narg(description), sqlc.narg(freight_template_id))
RETURNING id;

-- name: UpdateProduct :one
-- 部分更新，**一条语句同时回答两个问题**：这一行在本租户可见吗？改成功了吗？
--
-- 契约把这两件事分成两个响应码：查不到（或不是本租户的）是 404，
-- 已软删是 409 product-deleted。只看 rows_affected 的话两者的信号一样都是 0，
-- 而混掉的代价是调用方对着一个永远不会成功的请求反复重试 ——
-- 这与 db/queries/inventories.sql 里 DeductInventory 防的是同一类事。
--
-- 拆成「先 SELECT 确认可见、再 UPDATE」是两次快照，那个窗口是真的：
-- 中间这条商品可能刚好被另一个后台会话软删掉。两个 CTE 看到的是同一个快照。
--
-- brand_id 需要一个单独的 set_brand_id 开关：它在契约里是 integer | null，
-- 「传 null 表示清空品牌」与「没传这个字段」是两件事，而 COALESCE 表达不了 ——
-- COALESCE(NULL, brand_id) 会把「清空」悄悄变成「不动」。
-- 其余字段没有这个问题：title / category_id 非空，subtitle / description
-- 传 NULL 即不动（契约里它们没有 null 取值）。
--
-- 两个 CTE 里的 products 各带一个别名（cur / upd）。sqlc 的作用域解析把整条
-- 语句的关系拍平成一张表，同一张表出现两次就让每个裸列名都报 ambiguous。
--
-- **回传的行只剩一个 id**，整行由 repository 紧接着用 AdminGetProduct 回读，
-- 理由同 CreateProduct（00019 之后价格与库存要 LATERAL 才算得出来）。
-- 那个 id 不是多余的：它把「改成功了」与「updated_rows 数对了但没有行」
-- 分开 —— 后者只可能是这条 SQL 被改坏了，而静默返回零值会让一件全零的商品
-- 被序列化出去。
WITH cur AS (
    SELECT p.id, p.deleted_at FROM products p WHERE p.id = sqlc.arg(id)
), upd AS (
    UPDATE products u
       SET title       = COALESCE(sqlc.narg(title), u.title),
           subtitle    = COALESCE(sqlc.narg(subtitle), u.subtitle),
           description = COALESCE(sqlc.narg(description), u.description),
           category_id = COALESCE(sqlc.narg(category_id), u.category_id),
           brand_id    = CASE WHEN sqlc.arg(set_brand_id)::boolean
                              THEN sqlc.narg(brand_id)::bigint ELSE u.brand_id END,
           freight_template_id = CASE WHEN sqlc.arg(set_freight_template_id)::boolean
                              THEN sqlc.narg(freight_template_id)::bigint
                              ELSE u.freight_template_id END
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
    RETURNING u.id
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM upd) AS updated_rows,
       w.id
  FROM (SELECT 1) anchor
  LEFT JOIN upd w ON true;

-- name: SoftDeleteProduct :one
-- 置 deleted_at，**不删行**：cart_items 与四张派生表（向量 / 理解结果 / 同款簇）
-- 都对 products 有复合外键，硬删会被数据库直接拒绝。
--
-- 同样一条语句回答两个问题。契约这里的两支是：查不到 / 已软删 → 404，
-- 仍在架（status = 1）→ 409 product-still-published。所以除了 visible_rows
-- 还要回传 current_status —— 没有它，调用方分不清「已经删过了」和「还在架」。
WITH cur AS (
    SELECT p.id, p.status, p.deleted_at FROM products p WHERE p.id = sqlc.arg(id)
), del AS (
    UPDATE products u
       SET deleted_at = now()
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL AND u.status <> 1
    RETURNING u.id
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM del) AS deleted_rows,
       c.status     AS current_status,
       c.deleted_at AS current_deleted_at
  FROM (SELECT 1) anchor
  LEFT JOIN cur c ON true;

-- name: PublishProduct :one
-- 上架。三个数一起回传，因为契约在这里有三条出路：
--
--   visible_rows = 0            → 404（不存在 / 不是本租户的 / 已软删）
--   sku_rows     = 0            → 409 product-has-no-sku
--   updated_rows = 1            → 200
--
-- published_at 只在第一次上架时置位（COALESCE），之后永不覆盖：它是「首次发布
-- 时间」，前台按它倒序排；每次上架都覆盖的话，一次临时下架再上架就能把一件
-- 老商品顶到列表最前面。
--
-- 「没有 SKU 不能上架」这条闸门写进 UPDATE 的 WHERE 而不是先查后改：
-- 后者两次快照之间，最后一个 SKU 可能刚好被另一个会话软删掉。
--
-- **诚实交代一句**：这条 WHERE 在今天是 defense in depth，删掉它测试不会红。
-- 原因是 sku_count 与 UPDATE 在同一条语句、同一个快照里算出来，Go 那一侧
-- 拿 sku_rows 判出 409 之后 WithTenant 会回滚整个事务 —— 于是「UPDATE 其实
-- 执行了」这件事观察不到。本轮的变异验证把它标成了等价变异，没有为它编造一条
-- 测得出来的断言。留着它的理由是：哪天有人让那条路径返回 nil 而不是错误
-- （比如把「没有 SKU」改成一次静默的 no-op），承重的就只剩这一行了。
WITH cur AS (
    SELECT p.id FROM products p WHERE p.id = sqlc.arg(id) AND p.deleted_at IS NULL
), sku_count AS (
    SELECT count(*) AS n FROM skus s
     WHERE s.product_id = sqlc.arg(id) AND s.deleted_at IS NULL
), upd AS (
    UPDATE products u
       SET status       = 1,
           published_at = COALESCE(u.published_at, now())
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
       AND (SELECT n FROM sku_count) > 0
    RETURNING u.id, u.status, u.published_at
)
SELECT (SELECT count(*) FROM cur)      AS visible_rows,
       (SELECT n FROM sku_count)       AS sku_rows,
       (SELECT count(*) FROM upd)      AS updated_rows,
       w.published_at                  AS published_at
  FROM (SELECT 1) anchor
  LEFT JOIN upd w ON true;

-- name: UnpublishProduct :one
-- 下架。没有「没有 SKU」那条闸门 —— 把一个没有规格的商品从前台撤下来
-- 永远是安全的动作。只剩可见性一支要分开。
WITH cur AS (
    SELECT p.id FROM products p WHERE p.id = sqlc.arg(id) AND p.deleted_at IS NULL
), upd AS (
    UPDATE products u SET status = 2
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
    RETURNING u.id
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM upd) AS updated_rows;

-- RecalcProductAggregates 在 M4 Task 3 被删掉了，这里留一句话说明去向 ——
-- 它在 git 历史里，而「为什么没有了」只在这里。
--
-- 它重算的三个冗余字段中，min_price_cents / max_price_cents 两列已经由
-- 00019 从表上删掉（那个迁移的文件头写了完整理由）；total_stock 那一列还在，
-- 但从此没有任何一处写它。三个数现在都在读的时候现算，公式写在
-- AdminListProducts / AdminGetProduct 以及前台那两条读路径的 LATERAL 里。
--
-- 删掉它同时删掉了一整类故障：一个同步器只要有一条写路径忘了调用，
-- 冗余列就开始撒谎，而撒谎不报错、不变慢、不留痕迹。

-- ---------------------------------------------------------------------------
-- 商品图：整组替换（契约 PUT /admin/products/{product_id}/images）
-- ---------------------------------------------------------------------------

-- name: ClearProductImages :execrows
-- 整组替换的第一步。整组替换不是三个端点（加一张 / 删一张 / 调顺序）的语法糖：
-- 顺序是一个整体，增量模型下「把第 3 张挪到第 1 位」与「删掉第 2 张」两个并发
-- 请求会互相踩出一个有空洞的顺序，而那个中间态在增量模型里是可表达的。
DELETE FROM product_images WHERE product_id = $1;

-- name: InsertProductImage :one
-- 第二步，按数组下标逐条插。merchant_id 走列默认值 current_merchant()，
-- 于是复合外键 (upload_id, merchant_id) → uploads(id, merchant_id) 成了一道
-- **物理**闸门：挂别家租户的文件写不进去，不靠应用层记得检查。
INSERT INTO product_images (product_id, upload_id, sort_order)
VALUES ($1, $2, $3)
RETURNING id, product_id, upload_id, sort_order, created_at;

-- name: ListProductImages :many
-- 顺序即展示顺序，sort_order 最小的那一张就是主图（没有 is_primary 布尔）。
-- 第二排序键是 id：sort_order 撞了的时候也要有一个确定的顺序，
-- 否则同一组图在两次请求里可能换位，而那看起来像是「保存没生效」。
SELECT id, product_id, upload_id, sort_order, created_at
  FROM product_images
 WHERE product_id = $1
 ORDER BY sort_order, id;
