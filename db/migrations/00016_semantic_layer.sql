-- 语义检索与 AI 层的底座（数据模型 §8）。M3 Task 1。
--
-- 四张表 + 关键词召回要用的两列，一次落地：
--   product_text_vectors / product_image_vectors / product_understanding /
--   product_clusters，以及 products 上的 search_text + search_vector。
--
-- 为什么向量表和 search_text 在同一份迁移里：它们是**同一件事的两半**。
-- 商品标题、副标题、类目变了，要重算的是「这个商品的全部派生数据」——
-- 文本向量与 bigram 串一起。分两次做会让「商品变更时哪些派生数据要重算」
-- 这件事散在两处，而散在两处的清单迟早只更新一处（理由与 db/tenancy.json
-- 那份合并清单是同一个：两头各自成立、接起来不成立，是最难在评审里看出来的脱节）。
--
-- ===========================================================================
-- 一、CREATE EXTENSION vector 要求镜像里真有它
-- ===========================================================================
--
-- 官方 postgres:16 里没有。实测（本机，postgres:16，跑到本文件）：
--
--     goose run: ERROR 00016_semantic_layer.sql: failed to run SQL migration:
--     failed to execute SQL query "CREATE EXTENSION IF NOT EXISTS vector;":
--     ERROR: extension "vector" is not available (SQLSTATE 0A000)
--
-- goose 这一层的报错其实**指名道姓**，不冤枉。会冤枉人的是上一层：
-- `docker compose up -d` 只等到容器**启动**就返回 0，migrate 容器随后非 0 退出、
-- app 因为 depends_on 根本不起 —— 读者看到的是「curl 连不上 8080」，
-- 而那句 SQLSTATE 0A000 埋在 `docker compose logs migrate` 里。
-- compose.yaml 顶部那段「判据永远是 smoke 的退出码，不是 up 的」说的就是这件事。
--
-- 所以 compose.yaml 与 .github/workflows/ci.yml 的 postgres 一并换成
-- pgvector/pgvector:pg16（本机实测 vector 0.8.6 / PostgreSQL 16.15）。
-- 冷库测试也要用同一个镜像，见 CONTRIBUTING。
--
-- ===========================================================================
-- 二、与文档 DDL 的一处偏离：merchant_id 有了 DEFAULT current_merchant()
-- ===========================================================================
--
-- §8 的四张表原本都写着 `merchant_id BIGINT NOT NULL REFERENCES merchants(id)`，
-- 没有默认值。照抄的话，Task 3 的入库语句里就必须出现 merchant_id 这个词，而
-- scripts/check_query_tenancy.py 不许 db/queries 里出现它（理由见那个脚本的
-- 文件头：应用层再过滤一遍租户，「RLS 到底有没有生效」就变得测不出来了）。
--
-- 这是 00010 / 00012 / 00013 / 00014 用过的同一条路子，第五次。
-- **数据模型 §8 的 DDL 已随本轮一起改**，两边保持一份真相。
--
-- 默认值同时是一道形状约束：Task 3 生成的 Go 函数签名里根本没有 merchant_id，
-- 「拿 A 店的上下文往 B 店的商品名下写一条向量」连编译都编不出来。
-- RLS 的 WITH CHECK 仍是第二道。
--
-- ===========================================================================
-- 三、post-filter 陷阱：建索引的人和用索引的人要看到同一段话
-- ===========================================================================
--
-- 语义检索层 §2.4 讲了这个陷阱：HNSW 是近似索引，规划器先取向量最近的 N 条
-- 再过滤（post-filter），过滤条件命中率低时最终返回远少于 N 条、甚至为空。
--
-- **在这个仓库里它比文档写的更棘手一层。** product_text_vectors 带 merchant_id、
-- 是 tenant 类，策略是 `merchant_id = current_merchant()` —— 那个过滤条件
-- **不是应用写在 SQL 里的 WHERE，是 RLS 注入的**。于是：
--
--   · 应用**看不见**它，也**改不掉**它；
--   · 「过量召回再过滤」（§2.4 方案 2）在应用层**根本实现不了**。这不是
--     「麻烦一点」，是机制上不成立：不带迭代扫描时，HNSW 索引扫描一共只会
--     吐出 ef_search 条候选（本镜像实测默认 40），吐完就结束。这 40 条里
--     如果一条都不属于当前租户，**把 LIMIT 从 100 放大到 500 一行也多不出来**
--     —— LIMIT 限的是「过滤后要几行」，限不住索引愿意吐几行。
--
--     **数据模型 §8 那句「一期的应对很朴素：召回时把 LIMIT 放大（例如取
--     top-300 再截到 100）」因此是错的**，它把 app-side 过滤的直觉搬到了
--     RLS-side 过滤上。本轮已在 §8 原处标注更正。
--
--   · 小商家几百个商品，而 HNSW 先在全库几万条向量里取最近的 ef_search 条：
--     **一条都不剩是正常结果，不是异常**。
--
-- 所以 hnsw.iterative_scan 不是优化项，是能用的前提。它是**会话级 GUC**，
-- 必须设在检索用的那条连接上（repository.WithTenant，和 SET LOCAL
-- app.merchant_id 同一个地方）。本镜像的 pgvector 0.8.6 支持它：
--
--     SET hnsw.iterative_scan = relaxed_order;   -- 默认 off
--     SET hnsw.max_scan_tuples = 20000;          -- 默认就是 20000
--
-- **具体取值由 M3 计划第一条要求的那个实验定（Task 4），不在这份迁移里拍板。**
-- 本迁移只负责让索引与约束的形状不挡路，并把上面这段话放在建索引的地方。
--
-- ### 为什么本轮不按 merchant_id 做分区索引（§2.4 方案 3）
--
-- 文档说「租户数不多时这是最干净的解法」。三条实测 / 事实说明它在这里不是：
--
--   1. **它改主键，而主键是闸门的一部分。** PostgreSQL 要求分区表上的唯一
--      约束包含全部分区键列。实测（PostgreSQL 16.15）：
--          CREATE TABLE part_probe (product_id BIGINT PRIMARY KEY,
--            merchant_id BIGINT NOT NULL, embedding vector(4) NOT NULL)
--          PARTITION BY LIST (merchant_id);
--          ERROR:  unique constraint on partitioned table must include all
--                  partitioning columns
--      主键得变成 (product_id, merchant_id)，那就和数据模型 §8「主键就是
--      product_id，一个商品一行」以及 db/tenancy.json 里那条
--      `product_text_vectors(product_id)` 豁免全部对不上 —— 一次 DDL 改动
--      牵动唯一真相源与豁免清单两处，代价远不止「加一行 PARTITION BY」。
--   2. **LIST 分区要在建店时跑 DDL，而 keel_app 没有建表权。** 每来一家新店
--      就要 CREATE TABLE ... PARTITION OF，这条 DDL 只有管理员角色跑得了，
--      也就是说「建店」这个业务动作要多一条管理员通道 —— 那是 00005 刚刚
--      收窄掉的那类东西。
--   3. **HASH 分区不解决这个问题。** 它把租户打散进 N 个桶，每个桶里仍然是
--      几十家租户混着，post-filter 的命中率只改善 N 倍，而 Keel 面向的是
--      单库多小店（README 第一段），租户数远大于任何合理的 N。
--
-- 结论：**本轮不分区**，迭代扫描是第一方案。真要分区，那是一次独立的、
-- 要同时改数据模型 §8 与 tenancy.json 的动作，不该夹在底座迁移里默默做掉。
--
-- ===========================================================================
-- 四、增量重算的触发点：判据的形状（M3 计划第四条，实现在 Task 3）
-- ===========================================================================
--
-- 计划提的判据是「product_text_vectors.updated_at 与 products.updated_at 的
-- 先后关系」。它**只能当一半用**，原因是 touch_updated_at 挂在 products 上，
-- 任何一列变了它都前进 —— 包括 sales_count、total_stock、min_price_cents 这些
-- 和文本毫无关系的列。拿它单独当判据，一次下单就会把全店商品判成「向量过期」，
-- 而重算一遍 embedding 的钱是真花出去的。
--
-- 另一半在数据模型 §8「增量重算的指纹：只认一处」：
-- product_understanding.input_hashes 按 processor 分别记输入指纹。
--
-- 所以判据的形状是两段，各自守住一个方向：
--
--   · **触发点（不许漏算）**：products.updated_at 前进 ⇒ 该商品必须被重新
--     *判定*一次。粗，但不漏 —— 它只依赖已经存在的 touch_updated_at 触发器。
--   · **判定（不许滥算）**：真要不要重算，只看
--     input_hashes ->> 'text_embedding' 是否等于当前文本的指纹。
--     search_text 是**同一批派生数据里的另一个 processor**，指纹另记一格
--     （input_hashes ->> 'search_text'）—— 它的输入只有 title/subtitle，
--     换一次类目不该逼着 bigram 串重写，而只在向量表放一个 content_hash
--     做不到这种粒度（§8 原话）。
--
-- 于是可机械检查的闸门是这一条，Task 3 直接接：
--
--     对每一个 products 行，若 products.updated_at > product_text_vectors.updated_at，
--     则 input_hashes ->> 'text_embedding' 必须等于「按当前 products 行算出的
--     文本指纹」。等于 ⇒ 这次变更不涉及文本，向量没过期；不等 ⇒ 向量过期了，
--     而它还没被重算，闸门红。
--
-- 这条判据的三个前提，internal/db/semantic_test.go 的
-- TestStalenessCriterionRawMaterial 已经用真实数据钉死了（它今天就跑）：
--   ① 改 title 会让 products.updated_at 前进；
--   ② 改 sales_count **也**会让它前进（这就是「只用先后关系会滥算」的证据）；
--   ③ 改 products 不会让 product_text_vectors.updated_at 跟着动
--      （没有级联触发器），所以这个先后关系是一个真的信号，不是恒等式。
-- 缺了 ③，整条判据是空谈；缺了 ②，Task 3 会以为单靠先后关系就够。

-- +goose Up

CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------------------
-- 文本向量（数据模型 §8）
-- ---------------------------------------------------------------------------
CREATE TABLE product_text_vectors (
    product_id    BIGINT       PRIMARY KEY,
    merchant_id   BIGINT       NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    content       TEXT         NOT NULL,       -- 实际送入模型的拼接文本，留作调试与精排输入
    embedding     vector(1024) NOT NULL,       -- BGE-M3 / Qwen3-Embedding
    model_name    TEXT         NOT NULL,
    model_version TEXT         NOT NULL,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    FOREIGN KEY (product_id, merchant_id)
        REFERENCES products(id, merchant_id) ON DELETE CASCADE
);

-- 参数见语义检索层 §2.3：m = 16 / ef_construction = 64，十万到百万量级的通用起点。
-- 在有离线评测集（§9.1）之前不要凭感觉调这两个值。
--
-- **没有 merchant_id 前缀，而且加不了** —— HNSW 是单列向量索引，不接受前置的
-- B-tree 列。这正是上面第三节那段话的物理根源：过滤只能发生在索引之后。
-- 顺带一句：TestUniqueConstraintsAreTenantScoped 只管**唯一**约束，
-- 普通索引不在它视野里，所以这两个索引不会因为缺前缀而红 —— 不红不代表没事，
-- 代价记在上面。
CREATE INDEX idx_ptv_hnsw ON product_text_vectors
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

ALTER TABLE product_text_vectors ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_text_vectors FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON product_text_vectors USING (merchant_id = current_merchant());

-- 00007 那个 DO 循环只在它自己那一次迁移里跑过，之后新建的表要自己挂 ——
-- TestUpdatedAtIsMaintainedByTrigger 会盯着。
CREATE OR REPLACE TRIGGER touch_product_text_vectors_updated_at
    BEFORE UPDATE ON product_text_vectors FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- tenant 类的默认 GRANT 面（db/tenancy.json），TestAppRoleGrantSurface 逐表比对。
-- 00005 把 ALTER DEFAULT PRIVILEGES 收成了「新表只自动拿到 SELECT」，
-- 写权限必须由建表的这份迁移显式申明 —— 少给一项就是一次运行期 42501。
GRANT SELECT, INSERT, UPDATE, DELETE ON product_text_vectors TO keel_app;

-- ---------------------------------------------------------------------------
-- 图像向量（主图，数据模型 §8）
-- ---------------------------------------------------------------------------
--
-- 一期只嵌入主图，所以主键就是 product_id。多图检索（同一商品多张图各出一个
-- 向量）是后续扩展，届时主键改为 (product_id, image_url)。
-- 维度 768 与文本的 1024 不同，这正是两张表拆开的原因（§8：pgvector 的维度
-- 写在列类型上，768 维的向量插不进 vector(1024) 列，是插入直接报错）。
CREATE TABLE product_image_vectors (
    product_id    BIGINT       PRIMARY KEY,
    merchant_id   BIGINT       NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    image_url     TEXT         NOT NULL,       -- 当前已嵌入的主图，换图即重算
    embedding     vector(768)  NOT NULL,       -- clip-vit-l
    model_name    TEXT         NOT NULL,
    model_version TEXT         NOT NULL,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    FOREIGN KEY (product_id, merchant_id)
        REFERENCES products(id, merchant_id) ON DELETE CASCADE
);

CREATE INDEX idx_piv_hnsw ON product_image_vectors
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

ALTER TABLE product_image_vectors ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_image_vectors FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON product_image_vectors USING (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_product_image_vectors_updated_at
    BEFORE UPDATE ON product_image_vectors FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON product_image_vectors TO keel_app;

-- ---------------------------------------------------------------------------
-- 商品理解的状态与非向量结果（数据模型 §8）
-- ---------------------------------------------------------------------------
--
-- input_hashes 是「这个 processor 的输入变没变」的**唯一**真相源（§8
-- 「增量重算的指纹：只认一处」），也就是上面第四节那条判据的后半段。
-- 向量表刻意不带 content_hash：同一件事两处写，漂移的表现形式是
-- 「向量悄悄过期了但系统认为它是新的」，在检索结果里几乎看不出来。
CREATE TABLE product_understanding (
    product_id       BIGINT      PRIMARY KEY,
    merchant_id      BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    status           SMALLINT    NOT NULL DEFAULT 0,  -- 0待处理 1部分完成 2完成 3失败
    -- 各 processor 的输入指纹，按能力分别记录：
    -- {"text_embedding":"ab12..","image_embedding":"cd34..","attribute_extract":"ef56.."}
    input_hashes     JSONB       NOT NULL DEFAULT '{}',
    -- 非向量类结果：标准化属性、卖点、类目预测、质量分、合规结论
    results          JSONB       NOT NULL DEFAULT '{}',
    pipeline_version TEXT        NOT NULL,
    last_error       TEXT,                            -- 最近一次失败原因，供后台排查
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (product_id, merchant_id)
        REFERENCES products(id, merchant_id) ON DELETE CASCADE
);

-- 后台「待处理 / 部分完成 / 失败」列表与重跑扫描
CREATE INDEX idx_pu_unfinished
    ON product_understanding(merchant_id, status, updated_at)
    WHERE status IN (0, 1, 3);

ALTER TABLE product_understanding ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_understanding FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON product_understanding USING (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_product_understanding_updated_at
    BEFORE UPDATE ON product_understanding FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON product_understanding TO keel_app;

-- ---------------------------------------------------------------------------
-- 同款簇（数据模型 §8）
-- ---------------------------------------------------------------------------
--
-- cluster_id 带 merchant_id 前缀：同款簇不跨租户，「A 店的商品与 B 店的商品
-- 是同款」在多店模型里没有消费方。索引前缀让这一点在物理布局上也成立。
--
-- method 不只是审计字段，它是写入权限：人工的确认 / 拆分（2 / 3）优先级最高，
-- 自动聚类写入时要带 `WHERE method = 1` 条件更新 —— 与库存、优惠券用的是
-- 同一套条件原子更新手法。
CREATE TABLE product_clusters (
    product_id  BIGINT      PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    cluster_id  BIGINT      NOT NULL,
    confidence  REAL        NOT NULL,
    method      SMALLINT    NOT NULL,   -- 1自动 2人工确认 3人工拆分
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_cluster_conf CHECK (confidence >= 0 AND confidence <= 1),
    FOREIGN KEY (product_id, merchant_id)
        REFERENCES products(id, merchant_id) ON DELETE CASCADE
);

CREATE INDEX idx_pc_cluster ON product_clusters(merchant_id, cluster_id);
-- 人工判定不得被自动逻辑覆盖，扫描自动簇时按此索引取
CREATE INDEX idx_pc_auto
    ON product_clusters(merchant_id, cluster_id) WHERE method = 1;

ALTER TABLE product_clusters ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_clusters FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON product_clusters USING (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_product_clusters_updated_at
    BEFORE UPDATE ON product_clusters FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON product_clusters TO keel_app;

-- ---------------------------------------------------------------------------
-- 关键词召回的两列（数据模型 §8「关键词召回」，方案论证在语义检索层 §3）
-- ---------------------------------------------------------------------------
--
-- **search_vector 由 search_text 生成，不是由 title 生成**，这一条是整个
-- 关键词召回路成立与否的分界线：to_tsvector('simple', title) 对中文等于不分词，
-- 「羊毛衫」整个被当成一个 token，用户搜「毛衫」一条也召不回。
-- 分词发生在**应用层**：search_text 存的是二元切分串（「羊毛衫」→「羊毛 毛衫」），
-- simple 配置按空格切就够，不需要任何第三方分词扩展。
--
-- 把生成表达式改成 coalesce(title,'') 不会报任何错，也不会让任何一条
-- 「索引建上了没有」的断言变红 —— 它只是让中文搜索悄悄失效。
-- internal/db/semantic_test.go 的 TestSearchVectorIsGeneratedFromSearchText
-- 用**互不相交的 title 与 search_text** 把这个方向钉死。
--
-- 代价（§8 原话）：search_text 是应用层维护的派生列，标题改了忘记重写就会
-- 索引过期。这条写入必须和商品更新在同一个事务里，判据的形状见上面第四节。
ALTER TABLE products ADD COLUMN search_text TEXT;
ALTER TABLE products ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (to_tsvector('simple', coalesce(search_text, ''))) STORED;
CREATE INDEX idx_products_fts ON products USING gin(search_vector);

-- products 的 GRANT 面与 RLS 都是 00001/00002/00005 定好的，加两列不改变它。
-- search_vector 是生成列，PostgreSQL 本身就不允许直接写它。

-- +goose Down

DROP INDEX IF EXISTS idx_products_fts;
ALTER TABLE products DROP COLUMN IF EXISTS search_vector;
ALTER TABLE products DROP COLUMN IF EXISTS search_text;

REVOKE ALL ON product_clusters FROM keel_app;
DROP POLICY tenant ON product_clusters;
DROP TABLE product_clusters;

REVOKE ALL ON product_understanding FROM keel_app;
DROP POLICY tenant ON product_understanding;
DROP TABLE product_understanding;

REVOKE ALL ON product_image_vectors FROM keel_app;
DROP POLICY tenant ON product_image_vectors;
DROP TABLE product_image_vectors;

REVOKE ALL ON product_text_vectors FROM keel_app;
DROP POLICY tenant ON product_text_vectors;
DROP TABLE product_text_vectors;

-- 扩展刻意**不**在这里 DROP。它是库级对象，别的东西（将来的物化视图、
-- 另一个 schema）可能已经在用 vector 类型；回滚一份迁移不该把整个库的
-- 类型系统拆掉。留着一个没人用的扩展的代价是零。
