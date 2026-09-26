-- 门店、大区、两层可见性排除、三层价格覆盖，以及库存按门店分（数据模型 §4）。
--
-- 契约（docs/电商系统-OpenAPI.yaml 的 Store tag，23 条操作）与设计（数据模型 §4）
-- 上一轮已经定稿并入库，db/migrations 一个字都没改 —— 那 6 张表至今只存在于
-- 文档里。本文件把它们落到库里，并把三张老表改成按门店分。
--
-- ===========================================================================
-- 一、这条迁移为什么必须先换数据库镜像
-- ===========================================================================
--
-- 围栏是**多边形**，不是圆心加半径（数据模型 §4 论证过：运营在地图上画出来的
-- 是一串顶点，判定是「点在多边形内」，自己写射线法 UDF 等于重写 PostGIS 里被
-- 验证了二十年的那部分，而且没有空间索引，每次判定要全表扫门店）。
-- 所以下面第一句是 CREATE EXTENSION postgis，而它要求镜像里真有它。
--
-- pgvector/pgvector:pg16 里**没有** PostGIS，postgis/postgis:16-3.4 里
-- **没有** pgvector，而 00016 的第一句是 CREATE EXTENSION vector。两样都要，
-- 所以 docker/postgres/Dockerfile 自建了一个（FROM postgis/postgis:16-3.4 再从
-- PGDG 源装 postgresql-16-pgvector）。compose 与 CI 已随上一条提交换过去。
--
-- 那句 CREATE EXTENSION **仍然要写**，尽管 postgis 镜像的入口脚本在 initdb
-- 阶段就建好了它：入口脚本只在**数据目录为空**时跑，而挂着已有 pgdata 卷升级
-- 上来的库不会走那一步。「镜像里有」不等于「这个库里建过」。
--
-- 副作用要写明：postgis 会在 public 下建出一张 spatial_ref_sys 表和两张视图
-- （geometry_columns / geography_columns）。表那一张进了 db/tenancy.json 的
-- cross-tenant-infra，视图那两张由下面新加的视图闸门按「扩展自有对象」排除
-- （判据是 pg_depend.deptype = 'e'，不是一张名字清单）。
--
-- ===========================================================================
-- 二、GEOGRAPHY 而不是 GEOMETRY，这一条决定了「按距离排」是不是对的
-- ===========================================================================
--
-- GEOMETRY(POLYGON, 4326) 把经纬度当平面直角坐标算，ST_Distance 返回的是「度」。
-- 在北京那个纬度上一度经度约 85 km、一度纬度约 111 km —— **两个方向的尺子不一样
-- 长**。于是「按距离排」会在东西向与南北向上给出系统性偏斜的名次，
-- 而它看起来完全正常：数字是有的，顺序是稳定的，只是错的。
--
-- GEOGRAPHY 走球面计算，ST_Distance 与 <-> 都直接返回米。代价是球面运算更慢，
-- 以及少数函数不支持它 —— 本设计只用到 ST_Intersects / ST_Distance / <-> /
-- ST_IsValid，前三个都支持，ST_IsValid 在写入校验时对 GEOMETRY 调用。
--
-- SRID 4326 两处都写死。混进别的 SRID，ST_Intersects 会直接报错而不是静默算错，
-- 这是好事。
--
-- ===========================================================================
-- 三、回填：这是整条迁移里唯一一处不能靠「新数据走新路」绕过去的地方
-- ===========================================================================
--
-- inventories 的主键要从 sku_id 变成 (sku_id, store_id)，orders 要多两列
-- NOT NULL 的 store_id / region_id。库里已有的行必须有一家店可挂，
-- 否则这两个 ALTER 直接失败。
--
-- 所以下面给**每一个**已有商家建出一个默认大区 + 一家默认门店（code 都是
-- 'default'），再把库存、流水、订单挂上去。软删的商家也建 —— 它名下的历史订单
-- 一样要有归属，而「跳过软删商家」会让那批行无处可落，ALTER 当场失败，
-- 报错指向一条 NOT NULL 约束而不是这条判断。
--
-- 建出来的默认门店没有围栏（fence IS NULL）。这合法：
-- 因为默认店本来就靠「全国兜底」而不是靠围栏接单。
--
-- ===========================================================================
-- 四、Down 段还得原结构，还不原「哪家店」
-- ===========================================================================
--
-- inventories 的主键从 (sku_id, store_id) 退回 sku_id 时，同一个 SKU 在多家店
-- 的多行必须合成一行 —— 没有任何一种合法的合法：加起来是「把五家店的货当成
-- 一仓」，取默认店那一行是「其余四家的货凭空消失」。Down 取的是**默认门店
-- 那一行**，并在这里写明它有损，理由是回滚的语义是「回到只有一家店的世界」，
-- 而那个世界里的那一家店就是默认店。真要不丢数据，回滚前先把库存并到默认店。
--
-- orders 的三列直接 DROP，那是干净的（它们是本轮新加的）。

-- +goose Up

-- ---------------------------------------------------------------------------
-- 0. 扩展
-- ---------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS postgis;

-- ---------------------------------------------------------------------------
-- 1. regions（大区）。数据模型 §4。
--
-- **大区没有自己的几何。** 一次地理解析已经存在（坐标 → 命中哪些门店围栏 →
-- 门店），而门店属于某个大区，于是同一次解析同时给出两件事。再画一套大区边界
-- 就要回答「围栏与大区边界不一致时听谁的」，而那个问题没有好答案：按大区算价、
-- 按门店发货会出现「A 大区的价格、B 大区的货」。所以 stores.region_id 是大区
-- 唯一的入口。
--
-- 也没有 is_default：默认大区就是默认门店所在的那个大区，是一个可以推导出来的
-- 量，存一份等于给它一次和事实不一致的机会。
-- ---------------------------------------------------------------------------
CREATE TABLE regions (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    code        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    status      SMALLINT    NOT NULL DEFAULT 1,   -- 0 停用 1 启用
    deleted_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_regions_code ON regions(merchant_id, code)
    WHERE deleted_at IS NULL;
-- 供 stores / region_product_overrides / region_sku_prices / orders 做复合外键
CREATE UNIQUE INDEX uk_regions_id_merchant ON regions(id, merchant_id);

-- ---------------------------------------------------------------------------
-- 2. stores（门店与电子围栏）。数据模型 §4。
-- ---------------------------------------------------------------------------
CREATE TABLE stores (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id   BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    region_id     BIGINT      NOT NULL,            -- 所属大区，决定大区价与大区可见性
    code          TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    phone         TEXT        NOT NULL DEFAULT '',
    province      TEXT        NOT NULL DEFAULT '',
    city          TEXT        NOT NULL DEFAULT '',
    district      TEXT        NOT NULL DEFAULT '',
    address       TEXT        NOT NULL DEFAULT '',
    location      GEOGRAPHY(POINT, 4326),          -- 门店自身坐标，「按距离排」按它算
    fence         GEOGRAPHY(POLYGON, 4326),        -- 电子围栏，多边形
    is_default    BOOLEAN     NOT NULL DEFAULT FALSE,
    status        SMALLINT    NOT NULL DEFAULT 1,  -- 0 停业 1 营业
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- ### 这里**没有** CHECK (is_default OR fence IS NOT NULL)，这是想清楚之后删掉的
    --
    -- 本轮契约先行的那一版写着这条 CHECK，理由是「一家非默认店没有围栏就是
    -- 一家永远接不到单的店」—— 那个判断本身没错，错的是把它做成写入约束。
    -- 落地时对着真库跑了一遍，它让**两条契约明写的流程直接不可能**：
    --
    --   A) POST /admin/stores 按契约**不收围栏**（建店是表单、画围栏是地图，
    --      后台的两个界面）。于是这条端点建不出任何一家非默认门店 ——
    --      实测 23514，整个多门店功能从第一步就走不通。
    --
    --   B) PUT /admin/stores/{id}/default 的第一步是「清掉旧的那家默认店」。
    --      而上面那段回填给**每一个**已有商家造出的正是「默认店 + 无围栏」，
    --      于是这条 UPDATE 在每一个迁移过的库上都以 23514 失败 ——
    --      实测过，报错指着被清掉 is_default 的那一行。
    --
    -- 两条都不是边角情况，是主路径。所以这条规则从「写不进去」退成
    -- 「这一条路径上不许这么改」：**PUT .../fence 传 null 时，如果这家店不是
    -- 默认店，service 层拒掉（409 store-fence-required）**，
    -- 见 repository.SetStoreFence。
    --
    -- 诚实地说清楚退让了什么：这个状态**现在是可达的**（新建一家店、或者把
    -- 默认位让给别人），数据库不再挡它。契约本来就把它定义成一个正常的中间态
    -- （AdminStore.fence 为 null + is_default 为 false ⇒ 后台挂「未完成」提示），
    -- 所以可达不是新问题；从前那条 CHECK 只是在同时宣称「它不可达」
    -- 与「它是一个要显示的状态」，而两句话不能都对。
    FOREIGN KEY (region_id, merchant_id) REFERENCES regions(id, merchant_id)
);
-- 「至多一个默认店」这条规则**就是**这条部分唯一索引，不必在代码里写
-- 「设默认前先把其他的置 false」再祈祷没人并发点两下。切换默认店必须在同一个
-- 事务里先清旧再置新，顺序反了会自己撞自己。
-- 是「至多一个」而不是「恰好一个」：一个刚建出来、还没配默认店的商家是合法状态。
CREATE UNIQUE INDEX uk_stores_default ON stores(merchant_id)
    WHERE is_default AND deleted_at IS NULL;
CREATE UNIQUE INDEX uk_stores_code ON stores(merchant_id, code)
    WHERE deleted_at IS NULL;
-- 供 inventories / store_product_overrides / store_sku_prices / orders /
-- inventory_logs 做复合外键
CREATE UNIQUE INDEX uk_stores_id_merchant ON stores(id, merchant_id);
CREATE INDEX idx_stores_listing ON stores(merchant_id, status)
    WHERE deleted_at IS NULL;
-- 本文件唯一一条加不上 merchant_id 前缀的索引，与 GIN / HNSW 同源：
-- merchant_id 是 BIGINT，没有 GIST 操作符类，要放进同一个索引得先装 btree_gist。
-- 为一张几十行的表引入一个扩展不划算 —— GIST 先给候选，RLS 再过滤。
CREATE INDEX idx_stores_fence ON stores USING GIST (fence);

-- ---------------------------------------------------------------------------
-- 3. 每个已有商家一个默认大区 + 一家默认门店（回填的落点）
--
-- 用管理员角色跑，没有 app.merchant_id，current_merchant() 会 RAISE ——
-- 所以这两条 INSERT 显式写 merchant_id，不吃列默认值。
-- ---------------------------------------------------------------------------
INSERT INTO regions (merchant_id, code, name)
SELECT m.id, 'default', '默认大区' FROM merchants m;

INSERT INTO stores (merchant_id, region_id, code, name, is_default)
SELECT r.merchant_id, r.id, 'default', '默认门店', TRUE
  FROM regions r WHERE r.code = 'default';

-- ---------------------------------------------------------------------------
-- 4. 两张可见性排除表。数据模型 §4。
--
-- **排除表，不是清单表。缺一行的意思是「在售」，不是「不卖」。**
-- 产品定下的默认语义是「门店开店即营业，卖全部商品，手动下架不卖的」。
-- 包含表（记「这家店卖哪些」）的代价是 10 店 × 10,000 商品 = 十万行，
-- 而且上新一件商品之后它在所有门店默认不卖 —— 与「开店即营业」正面冲突。
--
-- chk_*_override_status CHECK (status = 0) 是那条产品规则在数据库里的执行者：
-- 有了它，这两张表在结构上表达不出「这家店只卖这几件」。想把默认改成
-- 「默认不卖」，必须先改 DDL，而那是一次迁移、一次评审；没有它，同一个改动
-- 只需要改一行注释和一个 WHERE，不会有任何东西变红。
--
-- 表名叫 overrides 而不是 exclusions，留的是将来加 sort_order（同样是 SPU
-- 粒度）的口子，届时这条 CHECK 放宽成 status IN (0, 1)。
--
-- updated_by 是单列外键（指向 staff），与 shipments(created_by) 同一条豁免：
-- staff.merchant_id 可空，复合外键在 NULL 下 MATCH SIMPLE 直接跳过检查，
-- 平台级操作员那一路会写不进去。清单在 db/tenancy.json 的 fk_single_column_ok。
-- 留这一列的收益很具体：运营最常问「这件商品在这家店为什么不卖了」，
-- 而一张只有主键两列的表回答不了它。
--
-- 重新上架就是删掉那一行，不加 deleted_at —— 给排除表再加软删标记等于让
-- 「不卖」有两种写法，而查询要同时认得两种。
-- ---------------------------------------------------------------------------
CREATE TABLE region_product_overrides (
    region_id   BIGINT      NOT NULL,
    product_id  BIGINT      NOT NULL,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    status      SMALLINT    NOT NULL,
    updated_by  BIGINT      REFERENCES staff(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (region_id, product_id),
    CONSTRAINT chk_region_override_status CHECK (status = 0),
    FOREIGN KEY (region_id, merchant_id)  REFERENCES regions(id, merchant_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id)
);
CREATE INDEX idx_region_overrides_product ON region_product_overrides(merchant_id, product_id);

CREATE TABLE store_product_overrides (
    store_id    BIGINT      NOT NULL,
    product_id  BIGINT      NOT NULL,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    status      SMALLINT    NOT NULL,
    updated_by  BIGINT      REFERENCES staff(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, product_id),
    CONSTRAINT chk_store_override_status CHECK (status = 0),
    FOREIGN KEY (store_id, merchant_id)   REFERENCES stores(id, merchant_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id)
);
CREATE INDEX idx_store_overrides_product ON store_product_overrides(merchant_id, product_id);

-- ---------------------------------------------------------------------------
-- 5. 两张价格覆盖表。数据模型 §4。
--
-- 同样是覆盖表：缺一行的意思是「用上一层的价」，不是「免费」或「不卖」。
-- 绝大多数 SKU 在绝大多数大区、门店上一行都没有 —— 它们用 skus.price_cents。
--
-- 没有 is_active / effective_from：定时调价、促销价是另一个产品（要活动、
-- 要时间窗、要与券的叠加顺序对齐），不该混进「这家店这个 SKU 卖多少钱」
-- 这个最底层的事实里。一期改价就是 upsert 一行、撤销就是删一行。
--
-- 可见性与价格**不能合并成一张表**：可见性是 SPU 粒度（运营说的是「这家店不卖
-- 这款」），价格是 SKU 粒度（同一款下不同规格的基准价本来就不同）。
-- 主键列都不一样，合并只有两条路 —— 要么价格降级成整款一个价，
-- 要么可见性升级成 SKU 粒度、运营逐个规格点下架。两条都是为省一张表改坏产品。
-- ---------------------------------------------------------------------------
CREATE TABLE region_sku_prices (
    region_id   BIGINT      NOT NULL,
    sku_id      BIGINT      NOT NULL,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    price_cents BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (region_id, sku_id),
    CONSTRAINT chk_region_price_nonneg CHECK (price_cents >= 0),
    FOREIGN KEY (region_id, merchant_id) REFERENCES regions(id, merchant_id),
    FOREIGN KEY (sku_id, merchant_id)    REFERENCES skus(id, merchant_id)
);

CREATE TABLE store_sku_prices (
    store_id    BIGINT      NOT NULL,
    sku_id      BIGINT      NOT NULL,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    price_cents BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, sku_id),
    CONSTRAINT chk_store_price_nonneg CHECK (price_cents >= 0),
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id),
    FOREIGN KEY (sku_id, merchant_id)   REFERENCES skus(id, merchant_id)
);

-- ---------------------------------------------------------------------------
-- 6. inventories 按门店分。数据模型 §4，§15 第 3 条在这里结清。
--
-- 三件事一起做：补 merchant_id、补 store_id、主键从 sku_id 变成
-- (sku_id, store_id)。**这张表因此从 parent-scoped 变成 tenant 类。**
--
-- 原豁免（不带 merchant_id）的两个理由本轮都失效了：
--
--   · 「租户归属由主键唯一决定，错挂之后没有第二个真相可以对不上」——
--     主键多了 store_id 之后有了：这一行可以说它属于 A 店的 SKU、
--     而那家店属于 B 商家。那正是 order_items 那种「两个真相对不上」的形状。
--   · 「热点表，多一列索引成本不划算」—— 这一列不是为索引加的，是为两条复合
--     外键加的，没有它复合外键拼不出来。而这两条外键合起来钉死的恰恰是新的
--     越权形状：把 A 店的库存挂到 B 商家的 SKU 上。
--
-- 策略随之从对父表的 EXISTS 子查询简化成直接的列比较。这是净收益：
-- 旧策略在每次扣减上外加两次 skus_pkey 查找（USING 与 WITH CHECK 各一），
-- 新策略是一次列比较，而下单扣减是全站最热的写路径。
--
-- 列序是 (sku_id, store_id) 不是反过来：热路径（详情页算 in_stock、下单扣减）
-- 都按 sku_id 进入；后台的「这家店的库存清单」是低频页面，给它
-- idx_inventories_store 就够了，而且那条索引还能带上 WHERE available_qty > 0。
-- ---------------------------------------------------------------------------
ALTER TABLE inventories ADD COLUMN merchant_id BIGINT;
ALTER TABLE inventories ADD COLUMN store_id    BIGINT;

UPDATE inventories i SET merchant_id = s.merchant_id
  FROM skus s WHERE s.id = i.sku_id;
UPDATE inventories i SET store_id = st.id
  FROM stores st
 WHERE st.merchant_id = i.merchant_id AND st.is_default;

ALTER TABLE inventories ALTER COLUMN merchant_id SET NOT NULL;
ALTER TABLE inventories ALTER COLUMN store_id    SET NOT NULL;
ALTER TABLE inventories ALTER COLUMN merchant_id SET DEFAULT current_merchant();
ALTER TABLE inventories ADD CONSTRAINT inventories_merchant_id_fkey
    FOREIGN KEY (merchant_id) REFERENCES merchants(id);

-- 旧主键是 sku_id 上的单列约束，同时也是那条单列外键的落点。
ALTER TABLE inventories DROP CONSTRAINT inventories_sku_id_fkey;
ALTER TABLE inventories DROP CONSTRAINT inventories_pkey;
ALTER TABLE inventories ADD PRIMARY KEY (sku_id, store_id);
ALTER TABLE inventories ADD CONSTRAINT inventories_sku_fkey
    FOREIGN KEY (sku_id, merchant_id)   REFERENCES skus(id, merchant_id);
ALTER TABLE inventories ADD CONSTRAINT inventories_store_fkey
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id);
CREATE INDEX idx_inventories_store ON inventories(merchant_id, store_id)
    WHERE available_qty > 0;

DROP POLICY tenant ON inventories;
CREATE POLICY tenant ON inventories
  USING      (merchant_id = current_merchant())
  WITH CHECK (merchant_id = current_merchant());

-- ---------------------------------------------------------------------------
-- 7. inventory_logs 补 store_id。数据模型 §4。
--
-- 不是可选的冗余：流水的唯一用途是对账，而对账口径从「这个商家这个 SKU 扣了
-- 多少」变成「这家店这个 SKU 扣了多少」。before_available / after_available
-- 现在记的是**某一家门店**的水位，不写下是哪一家，同一个 SKU 在五家店的流水
-- 会交织成一条谁也对不平的序列。
--
-- idx_inv_logs_sku_time 因此把 store_id 插在 sku_id 前面：盘点页是
-- 「这家店、这个 SKU、按时间倒序」，门店是更外层的筛选。
-- ---------------------------------------------------------------------------
ALTER TABLE inventory_logs ADD COLUMN store_id BIGINT;
UPDATE inventory_logs l SET store_id = st.id
  FROM stores st
 WHERE st.merchant_id = l.merchant_id AND st.is_default;
ALTER TABLE inventory_logs ALTER COLUMN store_id SET NOT NULL;
ALTER TABLE inventory_logs ADD CONSTRAINT inventory_logs_store_fkey
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id);
DROP INDEX idx_inv_logs_sku_time;
CREATE INDEX idx_inv_logs_sku_time
    ON inventory_logs(merchant_id, store_id, sku_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 8. orders 补 store_id / region_id / store_snapshot。数据模型 §5。
--
-- **两列都是 NOT NULL，理由在 §4 末尾**：SAGA 分支只拿到三个字符串
-- （gid / branchID / op），它读回订单行拿到一个 NULL 的 store_id 时无路可走 ——
-- 既不能猜默认店（那会把单扣到另一家店去），也不能失败（订单已经落库了）。
--
-- store_id 落在 orders 上就够，**不改 gid 的文法**：gid 是屏障幂等的键
-- （barrier 主键的第一列），改它的文法等于改那把钥匙的形状，而正在途中的事务
-- 会在部署切换的瞬间拿旧文法写、新文法读 —— 屏障失效的表现是业务被执行两遍，
-- 不是报错。分支本来就要按 order_no 读回订单行拿明细，store_id 跟着一起回来。
--
-- store_id / region_id 是外键，store_snapshot 是快照，**两样都要**：
-- 门店与大区是租户的运营主数据，第一需求就是按它聚合（「这家店这个月卖了多少」），
-- GROUP BY 一个 JSONB 里的字符串做不了；而门店会改名、会搬家、会换大区，
-- 三个月前那单的详情页要显示当时那个名字。快照里只放展示字段，不放 id ——
-- 放了就会有人去 GROUP BY 它。
--
-- region_id 冗余在 orders 上、不靠 stores.region_id 推：门店可以被调到另一个
-- 大区去，而这一单的价格是按**当时那个大区**算的。
-- ---------------------------------------------------------------------------
ALTER TABLE orders ADD COLUMN store_id       BIGINT;
ALTER TABLE orders ADD COLUMN region_id      BIGINT;
ALTER TABLE orders ADD COLUMN store_snapshot JSONB;

UPDATE orders o
   SET store_id = st.id,
       region_id = st.region_id,
       store_snapshot = jsonb_build_object(
           'store_name', st.name, 'region_name', r.name,
           'address', st.address, 'phone', st.phone)
  FROM stores st JOIN regions r ON r.id = st.region_id
 WHERE st.merchant_id = o.merchant_id AND st.is_default;

ALTER TABLE orders ALTER COLUMN store_id       SET NOT NULL;
ALTER TABLE orders ALTER COLUMN region_id      SET NOT NULL;
ALTER TABLE orders ALTER COLUMN store_snapshot SET NOT NULL;
ALTER TABLE orders ADD CONSTRAINT orders_store_fkey
    FOREIGN KEY (store_id, merchant_id)  REFERENCES stores(id, merchant_id);
ALTER TABLE orders ADD CONSTRAINT orders_region_fkey
    FOREIGN KEY (region_id, merchant_id) REFERENCES regions(id, merchant_id);
CREATE INDEX idx_orders_store ON orders(merchant_id, store_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 9. sku_prices_by_store —— 「就近生效」的唯一一份实现。数据模型 §4。
--
-- 这是全仓库唯一一处写 COALESCE(门店价, 大区价, 基准价) 的地方，而这一点是有
-- 执行者的：scripts/check_query_tenancy.py 新增一条，禁止 db/queries/*.sql 里
-- 出现 store_sku_prices / region_sku_prices 这两个表名（写价格的那两条 upsert
-- 按文件名豁免）。绕开视图就是在列表那边另写一套聚合，而那正是「列表显示的价
-- 与下单算出的价分叉」的来源 —— 那种 bug 只在特定门店 + 特定商品上出现。
--
-- **WITH (security_invoker = true) 不是可选项。少了它就是一个跨租户读取的洞。**
-- PostgreSQL 的视图默认以**视图属主**的权限求值，于是底层表的 RLS 按属主判定，
-- 而迁移跑出来的属主通常就是应用自己。实测（两个商家、各一个大区一家店、
-- 各自的 SKU，keel_app 同款的受限角色）：security_invoker 的视图给商家 1 返回
-- 4 行（对），默认视图返回 5 行 —— 多出来的那一行是商家 2 的店 × 商家 2 的 SKU。
-- 一行。不是报错，不是空集，是安静地多出一行别人的价格。
-- internal/db/migrate_test.go 本轮新增的视图闸门守住这一条。
--
-- **查这张视图必须带 store_id**：它的 FROM 是 stores × skus，不带门店条件就是
-- 一次笛卡儿积。规划器会把 store_id 推到 stores 上，之后两条 LEFT JOIN 都是
-- 主键点查。
--
-- price_source 不是调试字段：后台价格页要显示「这个价来自哪一层」，
-- order_items 要把它快照下来 —— 否则事后对账说不清「这个价是怎么来的」。
-- ---------------------------------------------------------------------------
-- 大区那一层也要一份「就近生效」，而上面那张视图给不了它。
--
-- 这是本文件相对数据模型 §4 唯一的一处**增加**（不是偏离：那一节写的 DDL
-- 一个字都没改），理由要写清楚：
--
-- 契约里大区是一个**真实的作用域** —— GET /admin/regions/{id}/products 要回
-- 「这个大区的可见性与生效价」，而它的 price_source 只会是 1 或 2。
-- 用 sku_prices_by_store 表达不了：那张视图的最内一层恰恰是门店价，
-- 而大区这一层必须**忽略**门店价。挑一家该大区下的门店去读也不行 ——
-- 那家店可能自己定了价，而且「挑哪一家」本身就没有答案。
--
-- 那为什么不在 db/queries 里写一句 COALESCE(rsp.price_cents, s.price_cents)：
-- 因为那正是 scripts/check_query_tenancy.py 本轮新加的那条闸门要挡的东西 ——
-- 一旦 db/queries 里可以直接读价格底表，「就近生效只有一份实现」这条纪律
-- 就只剩一句注释。放进视图之后，两层与三层的公式都在这一个文件里、
-- 挨着写、同受 security_invoker 闸门管，而 db/queries 一次都碰不到那两张底表。
--
-- 两条 COALESCE 的一致性由它们**挨着**保证，不由机械检查保证。这一点诚实说：
-- 三层那条是 COALESCE(门店, 大区, 基准)，两层这条是它去掉最内层，
-- 改一条不改另一条会让后台的大区价页与买家看到的价对不上。
-- 相邻的十行是这个仓库对这类耦合一贯的答案（见 00006 里 orders 的两条 CHECK）。
CREATE VIEW sku_prices_by_region WITH (security_invoker = true) AS
SELECT r.id         AS region_id,
       s.id         AS sku_id,
       s.product_id AS product_id,
       COALESCE(rsp.price_cents, s.price_cents) AS price_cents,
       CASE WHEN rsp.price_cents IS NOT NULL THEN 2   -- 大区价
            ELSE 1 END                                 -- 基准价
                    AS price_source
  FROM regions r
  JOIN skus   s   ON s.merchant_id = r.merchant_id
  LEFT JOIN region_sku_prices rsp ON rsp.region_id = r.id AND rsp.sku_id = s.id
 WHERE r.deleted_at IS NULL AND s.deleted_at IS NULL;

CREATE VIEW sku_prices_by_store WITH (security_invoker = true) AS
SELECT st.id        AS store_id,
       st.region_id AS region_id,
       s.id         AS sku_id,
       s.product_id AS product_id,
       COALESCE(ssp.price_cents, rsp.price_cents, s.price_cents) AS price_cents,
       CASE WHEN ssp.price_cents IS NOT NULL THEN 3   -- 门店价
            WHEN rsp.price_cents IS NOT NULL THEN 2   -- 大区价
            ELSE 1 END                                 -- 基准价
                    AS price_source
  FROM stores st
  JOIN skus   s   ON s.merchant_id = st.merchant_id
  LEFT JOIN store_sku_prices  ssp ON ssp.store_id  = st.id        AND ssp.sku_id = s.id
  LEFT JOIN region_sku_prices rsp ON rsp.region_id = st.region_id AND rsp.sku_id = s.id
 WHERE st.deleted_at IS NULL AND s.deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- 10. 行级安全。ENABLE 之外必须再加 FORCE（数据模型 §2「坑一」）。
-- 策略一律叫 tenant（db/tenancy.json 的 policy_name）。
--
-- 六张新表全部是 tenant 类：自带 merchant_id、策略是直接的列比较。
-- USING 与 WITH CHECK 两侧都写出来，与 00006 的 inventories 同一条理由：
-- 两者在这里等价，但显式写出来之后，将来任何一次「只放宽写侧」的改动
-- （读侧看不出任何异常）都是一次可见的删改。
-- ---------------------------------------------------------------------------
ALTER TABLE regions ENABLE ROW LEVEL SECURITY;
ALTER TABLE regions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON regions
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE stores ENABLE ROW LEVEL SECURITY;
ALTER TABLE stores FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON stores
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE region_product_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE region_product_overrides FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON region_product_overrides
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE store_product_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE store_product_overrides FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON store_product_overrides
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE region_sku_prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE region_sku_prices FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON region_sku_prices
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE store_sku_prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE store_sku_prices FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON store_sku_prices
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

-- ---------------------------------------------------------------------------
-- 11. updated_at 触发器。00007 的那个 DO 循环只跑过一次，新表要自己挂。
-- TestUpdatedAtIsMaintainedByTrigger 从系统目录枚举带这一列的表，少挂一张就红。
-- ---------------------------------------------------------------------------
CREATE OR REPLACE TRIGGER touch_regions_updated_at
    BEFORE UPDATE ON regions FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE OR REPLACE TRIGGER touch_stores_updated_at
    BEFORE UPDATE ON stores FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE OR REPLACE TRIGGER touch_region_product_overrides_updated_at
    BEFORE UPDATE ON region_product_overrides FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE OR REPLACE TRIGGER touch_store_product_overrides_updated_at
    BEFORE UPDATE ON store_product_overrides FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE OR REPLACE TRIGGER touch_region_sku_prices_updated_at
    BEFORE UPDATE ON region_sku_prices FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE OR REPLACE TRIGGER touch_store_sku_prices_updated_at
    BEFORE UPDATE ON store_sku_prices FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ---------------------------------------------------------------------------
-- 12. GRANT 面。00005 之后新表的默认权限只有 SELECT，写权限由建表的这份迁移
-- 显式申明。TestAppRoleGrantSurface 按 db/tenancy.json 的类别逐表比对，
-- 多给少给都会红。
--
-- 视图 sku_prices_by_store 只读：它是四条读路径的取价口，没有任何一条写路径
-- 该经过它（写价格走两张底表的 upsert）。默认权限本来就只给 SELECT，
-- 这里显式写一遍是为了让 GRANT 面在这一处能一眼读完。
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE
    ON regions, stores,
       region_product_overrides, store_product_overrides,
       region_sku_prices, store_sku_prices TO keel_app;
GRANT SELECT ON sku_prices_by_store, sku_prices_by_region TO keel_app;

-- +goose Down

REVOKE ALL ON sku_prices_by_store, sku_prices_by_region FROM keel_app;
REVOKE ALL ON regions, stores,
       region_product_overrides, store_product_overrides,
       region_sku_prices, store_sku_prices FROM keel_app;

DROP VIEW sku_prices_by_store;
DROP VIEW sku_prices_by_region;

DROP INDEX idx_orders_store;
ALTER TABLE orders DROP CONSTRAINT orders_region_fkey;
ALTER TABLE orders DROP CONSTRAINT orders_store_fkey;
ALTER TABLE orders DROP COLUMN store_snapshot;
ALTER TABLE orders DROP COLUMN region_id;
ALTER TABLE orders DROP COLUMN store_id;

DROP INDEX idx_inv_logs_sku_time;
CREATE INDEX idx_inv_logs_sku_time
    ON inventory_logs(merchant_id, sku_id, created_at DESC);
ALTER TABLE inventory_logs DROP CONSTRAINT inventory_logs_store_fkey;
ALTER TABLE inventory_logs DROP COLUMN store_id;

-- 库存回滚**有损**，这一段是那个损失发生的地方。
--
-- 主键退回 sku_id 时同一个 SKU 在多家店的多行必须合成一行，而没有一种合成是
-- 对的：加起来是「把五家店的货当成一仓」，取默认店那一行是「其余四家的货凭空
-- 消失」。取默认店那一行，因为回滚的语义是「回到只有一家店的世界」，
-- 而那个世界里的那一家店就是默认店。真要不丢数据，回滚前先把库存并到默认店。
DELETE FROM inventories i
 WHERE EXISTS (SELECT 1 FROM stores st
                WHERE st.id = i.store_id AND NOT st.is_default);
DROP POLICY tenant ON inventories;
DROP INDEX idx_inventories_store;
ALTER TABLE inventories DROP CONSTRAINT inventories_store_fkey;
ALTER TABLE inventories DROP CONSTRAINT inventories_sku_fkey;
ALTER TABLE inventories DROP CONSTRAINT inventories_pkey;
ALTER TABLE inventories ADD PRIMARY KEY (sku_id);
ALTER TABLE inventories ADD CONSTRAINT inventories_sku_id_fkey
    FOREIGN KEY (sku_id) REFERENCES skus(id);
ALTER TABLE inventories DROP CONSTRAINT inventories_merchant_id_fkey;
ALTER TABLE inventories DROP COLUMN store_id;
ALTER TABLE inventories DROP COLUMN merchant_id;
CREATE POLICY tenant ON inventories
  USING      (EXISTS (SELECT 1 FROM skus s
                       WHERE s.id = inventories.sku_id
                         AND s.merchant_id = current_merchant()))
  WITH CHECK (EXISTS (SELECT 1 FROM skus s
                       WHERE s.id = inventories.sku_id
                         AND s.merchant_id = current_merchant()));

DROP TABLE store_sku_prices;
DROP TABLE region_sku_prices;
DROP TABLE store_product_overrides;
DROP TABLE region_product_overrides;
DROP POLICY tenant ON stores;
DROP TABLE stores;
DROP POLICY tenant ON regions;
DROP TABLE regions;

-- postgis 不 DROP：spatial_ref_sys 里可能已经有别的东西在用，而扩展本身是
-- 幂等建出来的。Down 的职责是还原这条迁移自己建的东西，不是还原镜像。
