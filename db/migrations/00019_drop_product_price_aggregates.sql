-- 删掉 products 上那两列冗余价格（数据模型 §3）。M4 Task 3。
--
-- ===========================================================================
-- 一、为什么现在删，而不是继续维护它们
-- ===========================================================================
--
-- `min_price_cents` / `max_price_cents` 从 00001 起就在表上，注释写着
-- 「由 SKU 变更时同步更新」。M4 Task 2 真的给它们配了一个同步器
-- （db/queries/admin_products.sql 的 RecalcProductAggregates），
-- 本轮把同步器和列一起删掉，改成读的时候现算。三条理由：
--
--   ① **它们很快就不再是商品的属性。** 多门店 + 大区的设计已经定稿，
--      那之后价格有三层（SKU 基准价 → 大区价 → 门店价），
--      「这件商品多少钱」取决于这次请求解析到哪一家门店。
--      一个挂在 products 行上的标量答不出一个依赖于解析结果的问题 ——
--      到那时这两列不是「要改」，是「没有正确取值」。
--
--   ② **留着它们就是第三个 reserved_qty。** 同一张表上的 total_stock 已经是
--      db/queries/search.sql 文件头点名的那个东西：「一句永远不会被纠正的谎」。
--      一个冗余列只要有一条写路径忘了同步，它就开始撒谎，而撒谎不报错、
--      不变慢、不留痕迹 —— 症状是前台列表价和详情页价对不上，
--      而那看起来像缓存问题。
--
--   ③ **价格过滤今天本来就没有索引可走。** 唯一覆盖 products 的列表索引是
--      idx_products_listing ON products(merchant_id, category_id, status,
--      published_at DESC) WHERE deleted_at IS NULL —— 这两列不在其中，
--      也不在任何别的索引里。也就是说它们从来只是「已经取到这一行之后少一次
--      join」，不是「让规划器少扫一批行」。改成现算改变的是每行的常数，
--      不改变执行计划的类别。
--
-- ===========================================================================
-- 二、现算的形状：一处公式，四条读路径
-- ===========================================================================
--
-- 每条读路径挂一个 LEFT JOIN LATERAL：
--
--     (SELECT min(s.price_cents), max(s.price_cents)
--        FROM skus s WHERE s.product_id = p.id AND s.deleted_at IS NULL)
--
-- 取值范围是**未软删**的 SKU，不分在售与停售 —— 与被删掉的
-- RecalcProductAggregates 逐字一致。停售的规格仍出现在商品详情的规格矩阵里
-- （契约明写），它的价格还在给买家看，把它排除掉会让展示的价格区间和矩阵里
-- 的价格对不上。
--
-- 一个 SKU 都没有时 COALESCE 到 0，同样与旧列的取值一致（它的 DEFAULT 是 0）。
-- 这一条在 db/queries/search.sql 的两条召回里尤其要紧：旧的过滤条件写的是
-- 「p.max_price_cents >= $min」，而 max_price_cents 在没有 SKU 时是 0，
-- 于是那件商品被筛掉。换成裸的 max(price) 会得到 NULL，`NULL >= $min` 也是
-- 「不通过」，看上去一样；但反方向的「p.min_price_cents <= $max」在旧列下是
-- `0 <= $max` 即**通过**，换成 NULL 就变成不通过了。所以两处都留着 COALESCE，
-- 让现算与旧列在全部取值上逐点相同，而不是「大部分情况下一样」。
--
-- ===========================================================================
-- 三、Down 段能还原列，还不回值
-- ===========================================================================
--
-- Down 把两列加回来，取值是**按当时的 skus 现算的结果**，不是 DEFAULT 0 ——
-- 回滚到旧世界意味着旧的读路径（读列）要立刻能给出正确答案，而一列全 0
-- 会让整站商品在回滚的那一刻变成免费。
--
-- 回滚之后仍然缺一样东西：维护它们的那个同步器。那是代码，不是 schema，
-- 迁移还不回来 —— 回滚 schema 的同时必须把代码也回滚到 00019 之前。
-- 这句话写在这里，是因为「迁移能 down」很容易被读成「回滚是安全的」。

-- +goose Up

ALTER TABLE products DROP COLUMN min_price_cents;
ALTER TABLE products DROP COLUMN max_price_cents;

-- +goose Down

ALTER TABLE products ADD COLUMN min_price_cents BIGINT NOT NULL DEFAULT 0;
ALTER TABLE products ADD COLUMN max_price_cents BIGINT NOT NULL DEFAULT 0;

UPDATE products p
   SET min_price_cents = COALESCE(agg.min_price, 0),
       max_price_cents = COALESCE(agg.max_price, 0)
  FROM (SELECT s.product_id,
               min(s.price_cents) AS min_price,
               max(s.price_cents) AS max_price
          FROM skus s
         WHERE s.deleted_at IS NULL
         GROUP BY s.product_id) agg
 WHERE p.id = agg.product_id;
