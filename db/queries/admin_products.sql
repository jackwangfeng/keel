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
SELECT id, category_id, brand_id, title, subtitle, description,
       min_price_cents, max_price_cents, total_stock, sales_count,
       status, published_at, deleted_at, created_at, updated_at
  FROM products
 WHERE (sqlc.narg(status)::smallint IS NULL OR status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(category_id)::bigint IS NULL
        OR category_id = sqlc.narg(category_id)::bigint)
   AND (sqlc.arg(include_deleted)::boolean OR deleted_at IS NULL)
 ORDER BY id DESC
 LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: AdminCountProducts :one
-- 条件必须与 AdminListProducts 逐字一致：两边只要有一处不同，total 数的就不是
-- 列表实际会分出来的那批行，而客户端据此算出的总页数会少，最后几页谁也翻不到。
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
SELECT id, category_id, brand_id, title, subtitle, description,
       min_price_cents, max_price_cents, total_stock, sales_count,
       status, published_at, deleted_at, created_at, updated_at
  FROM products
 WHERE id = $1;

-- name: CreateProduct :one
-- 新建即草稿：status 走列默认值 0，published_at 保持 NULL。
-- **请求体里没有 status**（契约），所以这里也不给它参数位 ——
-- 一个能在创建时指定状态的入参，就是一条绕过 publication 端点的路。
INSERT INTO products (category_id, brand_id, title, subtitle, description)
VALUES (sqlc.arg(category_id), sqlc.narg(brand_id), sqlc.arg(title),
        sqlc.narg(subtitle), sqlc.narg(description))
RETURNING id, category_id, brand_id, title, subtitle, description,
          min_price_cents, max_price_cents, total_stock, sales_count,
          status, published_at, deleted_at, created_at, updated_at;

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
WITH cur AS (
    SELECT p.id, p.deleted_at FROM products p WHERE p.id = sqlc.arg(id)
), upd AS (
    UPDATE products u
       SET title       = COALESCE(sqlc.narg(title), u.title),
           subtitle    = COALESCE(sqlc.narg(subtitle), u.subtitle),
           description = COALESCE(sqlc.narg(description), u.description),
           category_id = COALESCE(sqlc.narg(category_id), u.category_id),
           brand_id    = CASE WHEN sqlc.arg(set_brand_id)::boolean
                              THEN sqlc.narg(brand_id)::bigint ELSE u.brand_id END
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
    RETURNING u.id, u.category_id, u.brand_id, u.title, u.subtitle, u.description,
              u.min_price_cents, u.max_price_cents, u.total_stock, u.sales_count,
              u.status, u.published_at, u.deleted_at, u.created_at, u.updated_at
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM upd) AS updated_rows,
       w.id, w.category_id, w.brand_id, w.title, w.subtitle, w.description,
       w.min_price_cents, w.max_price_cents, w.total_stock, w.sales_count,
       w.status, w.published_at, w.deleted_at, w.created_at, w.updated_at
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

-- name: RecalcProductAggregates :one
-- 重算 §3 的三个冗余字段。SKU 增 / 改价 / 软删之后都要跑一次。
--
-- 取值范围是**未软删**的 SKU，不分在售与停售。停售的规格仍出现在商品详情的
-- 规格矩阵里（契约明写），它的价格也还在给买家看，所以把它排除掉会让展示的
-- 价格区间和矩阵里的价格对不上。
--
-- 一个 SKU 都没有时三个数归 0，这与「新建的商品此刻是 0」一致；
-- 而「0 元商品出现在前台」这件事由上架闸门挡（没有 SKU 不能上架），
-- 不由这里挡 —— 这里只负责把冗余字段算对。
UPDATE products p
   SET min_price_cents = COALESCE(agg.min_price, 0),
       max_price_cents = COALESCE(agg.max_price, 0),
       total_stock     = COALESCE(agg.stock, 0)
  FROM (
        SELECT min(s.price_cents) AS min_price,
               max(s.price_cents) AS max_price,
               COALESCE(sum(i.available_qty), 0)::int AS stock
          FROM skus s
          LEFT JOIN inventories i ON i.sku_id = s.id
         WHERE s.product_id = sqlc.arg(product_id) AND s.deleted_at IS NULL
       ) agg
 WHERE p.id = sqlc.arg(product_id)
RETURNING p.min_price_cents, p.max_price_cents, p.total_stock;

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
