-- compose 默认形态的种子：**一家店**。
--
-- ### 为什么不是 db/seed/dev.sql
--
-- dev.sql 是测试夹具，它刻意播 6 家商家（含停用、软删、没登记域名的反例），
-- 好让跨租户隔离与租户解析的测试有区分力。把它加载进 compose 的默认形态，
-- 服务会**拒绝启动**：默认形态配了 KEEL_DEFAULT_MERCHANT（单商家部署），
-- 而 tenant.Preflight 在「配了默认商家 + 库里有多家活跃商家」时报错退出 ——
-- 单商家模式忽略 Host，那份配置会把 6 家店的流量全送到同一家去。
--
-- 两条路只能选一条：要么 compose 改走 KEEL_BASE_DOMAIN 多商家模式，
-- 要么默认形态的种子只播一家。选后者，因为 README 承诺的是
-- 「小商家 `docker compose up` 起来就是租户数为 1 的部署，感知不到多租户」——
-- 多商家模式下 `curl localhost:8080` 拿不到任何商品（Host 是 localhost，
-- 谁也匹配不上），要带 `-H 'Host: shop-a.example.com'` 才行，
-- 而那是没有一个小商家会做的动作。冒烟脚本跑的必须是他们真的会跑的那条命令。
--
-- 跨租户测试的种子基础一点没少：那些测试直接从 Go 里加载 dev.sql
-- （internal/handler、internal/tenant、internal/app 三处的 TestMain），
-- 从不经过 compose。多商家形态也仍然是一条命令 —— 见 compose.multi.yaml，
-- 它加载的就是 dev.sql。
--
-- ### 为什么是 demo 而不是 shop-a
--
-- 与 dev.sql 里的 6 家一个都不重名，于是两份种子之间没有任何需要「保持一致」的
-- 行 —— 没有共享的行，就没有会悄悄跑偏的行。code 取 demo 也和契约里的示例商家一致；
-- 它不在平台保留名单里（www/api/admin 那些才是）。
--
-- 幂等性同 dev.sql：每条 INSERT 带 WHERE NOT EXISTS，不用 ON CONFLICT ——
-- 后者只在真有唯一约束被撞到时才沉默跳过，而 categories/products 上撞不到，
-- 重复加载会一遍遍累积重复行。compose 每次 `up` 都会重跑一次这个文件。
--
-- ### 这份文件没有任何自动化覆盖（拆两份种子的真实代价）
--
-- dev.sql 有 `make test-db` 守着：表结构一改，加载它的那几个 TestMain 当场失败。
-- 这份没有。改个列名、加个 NOT NULL，dev.sql 那边会响，这边要等到有人跑
-- `docker compose up` 才响 —— 而那可能是发布前的最后一刻，也可能是一个新人
-- 第一次 clone 这个仓库的时候。
--
-- 补它的办法不是再写一个 Go 测试（那会把 compose 的种子也变成夹具），
-- 而是让 CI 真的跑一次 `docker compose up -d --build && ./scripts/smoke.sh`。
-- Task 8 的 CI 必须接上这条，否则这里就是一笔欠账。
-- 注意 `up -d` 在应用随后崩溃时**仍然退出 0**，所以 CI 里单跑 up 等于没跑，
-- 必须跟上 smoke。
--
-- 用管理员角色加载（compose 里的 seed 服务是 `psql postgres://keel:keel@...`）：
-- categories/products 带 RLS，keel_app 在没有租户上下文的连接上一行都插不进去。

INSERT INTO merchants (code, name, status)
SELECT 'demo', '示例小店', 1
 WHERE NOT EXISTS (SELECT 1 FROM merchants m WHERE m.code = 'demo');

-- 支付渠道的回调验签密钥（M2 任务 7）。
--
-- 没有它，`docker compose up` 起来的这套演示**下得了单但付不了款** ——
-- 支付回调是未认证入口，验签过不了就是 401，而「这家店没配密钥」正是被拒的
-- 成因之一（那是刻意的：没配密钥就放行会让每一家还没接支付的店都成为伪造入口）。
-- 而 M2 的产出标志是「能下单能支付」，所以这一行属于这份演示种子。
--
-- **这把密钥是公开的、写死的，和下面那个买家口令一样**：它是演示夹具，不是凭据。
-- 任何真实部署都必须换掉它 —— 拿着它的人可以伪造一条「已支付」的回调。
INSERT INTO shop_settings (merchant_id, extra)
SELECT m.id, jsonb_build_object('payment_channels', jsonb_build_object(
           'wechat', jsonb_build_object('notify_secret', 'demo-wechat-notify-secret'),
           'alipay', jsonb_build_object('notify_secret', 'demo-alipay-notify-secret')))
  FROM merchants m
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM shop_settings s WHERE s.merchant_id = m.id);

-- ---------------------------------------------------------------------------
-- 大区与门店
-- ---------------------------------------------------------------------------
--
-- 00020 之后 inventories 的 store_id 是 NOT NULL，没有门店就一行库存也插不
-- 进去，而那条迁移的回填只覆盖迁移那一刻库里已有的商家 —— demo 是之后才建的。
--
-- 单店部署只要一家默认店：is_default = TRUE 让它成为「全国兜底」的接单目标，
-- 默认店靠「全国兜底」接单，不靠围栏，所以不画围栏是正常形态。
-- **刻意不画围栏、不给坐标**：这套演示里没有任何一条路径按距离或围栏选店，
-- 编一组经纬度进去只会让人以为 `docker compose up` 起来的这家店真的开在那儿。
--
-- 幂等守卫同本文件其余部分：uk_regions_code / uk_stores_code 是带
-- `WHERE deleted_at IS NULL` 的部分唯一索引，NOT EXISTS 不依赖它们也照样幂等。
--
-- merchant_id 显式写：这个文件由管理员角色加载（见文件头），连接上没有
-- app.merchant_id，列默认值 current_merchant() 会直接 RAISE。
INSERT INTO regions (merchant_id, code, name)
SELECT m.id, 'default', '默认大区'
  FROM merchants m
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM regions r
                    WHERE r.merchant_id = m.id AND r.code = 'default');

INSERT INTO stores (merchant_id, region_id, code, name, is_default)
SELECT r.merchant_id, r.id, 'default', '示例小店（默认门店）', TRUE
  FROM regions r
  JOIN merchants m ON m.id = r.merchant_id
 WHERE m.code = 'demo'
   AND r.code = 'default'
   AND NOT EXISTS (SELECT 1 FROM stores st
                    WHERE st.merchant_id = r.merchant_id AND st.code = 'default');

-- ---------------------------------------------------------------------------
-- 类目
-- ---------------------------------------------------------------------------
--
-- 五个类目对应五个**语义簇**。这不是为了好看：检索演示要看得出「语义相近」
-- 与「字面相近」不是一回事，而一个只有咖啡器具的库里，搜什么都在同一个簇内。
--
-- 「默认分类」这个名字留着没改：它是第一版种子里咖啡器具那三件的归属，
-- 改名要动的是数据而不是结构，而改它换不来任何东西。
INSERT INTO categories (merchant_id, name, path, status)
SELECT m.id, v.name, '', 1
  FROM merchants m
  CROSS JOIN (VALUES ('默认分类'), ('女装'), ('家居'), ('数码配件'), ('食品饮料')) AS v(name)
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM categories c
                    WHERE c.merchant_id = m.id AND c.name = v.name);

-- path 要**含自己的 id**（形如 /12/），不能用名字。
--
-- 第一版这里写的是 '/' || v.name || '/'，也就是 /女装/。后台建类目、挪子树、判环
-- 全部按 path 前缀工作（admin_categories.sql 的 CreateCategoryRow、repository 的
-- MoveCategory），而买家侧按类目筛商品也按前缀取子树（products.sql 文件头）。
-- 用 id 的全部意义是**让前缀唯一**：两个同名顶层类目的 path 都是 /女装/ 时，
-- 挪其中一棵子树会连另一棵一起改写，按类目筛也会把两棵并在一起。
--
-- 为什么不在上面那条 INSERT 里直接写 id：id 是 GENERATED ALWAYS AS IDENTITY，
-- 插之前不知道；而写成数据修改 CTE 在 PostgreSQL 里是静默无效的（同一条语句的
-- 各个 CTE 共享快照，UPDATE 看不到刚插的行，理由同 CreateCategoryRow）。
--
-- 写成可以反复跑的：种子每次 compose up 都会跑，挂着一个由旧种子初始化的库时，
-- 这一条把名字格式的 path 连同它们的子孙一起改成 id 格式。按商家对齐，
-- 因为子孙判定是字符串前缀，不带商家的话两家店的同名类目会互相改写。
UPDATE categories c
   SET path = '/' || r.id || '/' || substr(c.path, length(r.path) + 1)
  FROM categories r
 WHERE r.parent_id IS NULL
   AND r.path !~ '^/[0-9]+/$'
   AND c.merchant_id = r.merchant_id
   AND (c.id = r.id
        -- 按前缀带上子孙只在旧 path 真有内容时做。空串或单个 / 当前缀，
        -- LIKE 会匹配这家商家的**全部**类目，一条种子改写整张表。
        OR (length(r.path) > 1 AND c.path LIKE r.path || '%'));


-- ---------------------------------------------------------------------------
-- 商品
-- ---------------------------------------------------------------------------
--
-- 二十三件在架商品。status = 1 且 published_at 非空，否则列表接口看不见它们
-- （ListProducts 的谓词是 `deleted_at IS NULL AND status = 1`）。
--
-- ### 为什么从 5 件加到 23 件
--
-- 演示栈上搜「咖啡」，裙子也出来了。查下来排序完全正确（挂耳 0.5408 /
-- 手冲壶 0.5396 / 马克杯 0.3883 / 吊带裙 0.2946 / 连衣裙 0.2797，
-- 两档之间有 0.09 的断崖），问题只是**库里一共 5 件，size 再小也是全部**。
--
-- 召回层**刻意没有相似度阈值**，这个决定不要去改：定阈值要有语义检索层
-- §9.1 的离线评测集，在那之前拍一个数就是盲调，而且换模型时它会失效
-- 却不报错（完整论证在 internal/service/search.go 第四节与
-- scripts/smoke_search.py 的文件头，都有测试守着）。
-- 正确的解法是让库里有足够的商品，size 自然把不相关的挤掉。
--
-- ### 这批数据自己要守的几条性质
--
--   · **每个簇里都有一对「字面不像、意思相近」的商品。** 它们是检索能力的
--     证据：关键词那一路一个二元组都不共享，只有向量那一路捞得回来。
--     三对：雪纺碎花连衣裙 / 真丝吊带长裙、香薰蜡烛 / 室内扩香藤条、
--     入耳式蓝牙耳机 / 头戴降噪耳罩。副标题也刻意错开，不让它们从那儿撞上。
--   · **价格散开，不都是整百。** 00019 之后商品的价格区间是从 skus 现算的，
--     所以这批价格直接决定列表上显示的区间对不对 —— 全都一样的话，
--     把 min() 写成 max() 不会红。
--   · **库存有高有低，并且留一件 0 库存的**（针织开衫外套）。
--     in_stock 的判据（「任意一个在售 SKU 水位 > 0」）因此可被证伪。
--     它刻意**不是**最后插入的那一件：scripts/smoke.sh 取 /products 的
--     items[0]（按 published_at 倒序，也就是最后插入的那件）去下单，
--     挑不出有货 SKU 时会失败。
--
-- **total_stock 不在这条 INSERT 里**（走列默认值 0）。00019 删掉
-- RecalcProductAggregates 之后，全仓库没有任何一处写那一列 —— 后台读到的
-- 总库存是从 inventories 现算的。种子往它里面填一个数，只会造出一个
-- 与真实水位对不上的假象。
INSERT INTO products (merchant_id, category_id, title, subtitle,
                      sales_count, status, published_at)
SELECT c.merchant_id, c.id, v.title, v.subtitle, 0, 1, now()
  FROM merchants m
  JOIN categories c ON c.merchant_id = m.id
  CROSS JOIN (VALUES
                -- 咖啡器具：手冲壶 / 法压壶 / 磨豆机 字面互不相干，意思同簇
                ('默认分类',   '手冲咖啡壶',      '600ml 玻璃'),
                ('默认分类',   '陶瓷马克杯',      '两只装'),
                ('默认分类',   '挂耳咖啡 10 包',  '中度烘焙'),
                ('默认分类',   '意式浓缩咖啡机',  '家用半自动'),
                ('默认分类',   '法压壶',          '350ml 耐热玻璃'),
                ('默认分类',   '手摇磨豆机',      '不锈钢磨芯'),
                ('默认分类',   '冷萃咖啡液 6 支', '无糖'),
                -- 女装：第一对「字面不像、意思相近」
                ('女装',       '雪纺碎花连衣裙',  '夏季新款 显瘦'),
                ('女装',       '真丝吊带长裙',    '法式复古'),
                ('女装',       '羊毛混纺大衣',    '通勤直筒'),
                ('女装',       '高腰阔腿牛仔裤',  '显腿长'),
                ('女装',       '针织开衫外套',    '慵懒风'),
                -- 家居：第二对（香薰蜡烛 / 室内扩香藤条）
                ('家居',       '亚麻四件套',      '1.8 米床 裸睡级'),
                ('家居',       '香薰蜡烛',        '雪松木质调'),
                ('家居',       '室内扩香藤条',    '无火 持久'),
                ('家居',       '羊毛地毯',        '北欧几何'),
                -- 数码配件：第三对（入耳式蓝牙耳机 / 头戴降噪耳罩）
                ('数码配件',   '入耳式蓝牙耳机',  '真无线 长续航'),
                ('数码配件',   '头戴降噪耳罩',    '包耳设计 舒适'),
                ('数码配件',   '机械键盘 87 键',  '茶轴 热插拔'),
                ('数码配件',   '快充充电宝',      '20000mAh 65W'),
                -- 食品饮料
                ('食品饮料',   '黑巧克力礼盒',    '72% 可可'),
                ('食品饮料',   '冻干草莓脆',      '无添加蔗糖'),
                ('食品饮料',   '明前龙井茶',      '100g 罐装')
             ) AS v(category, title, subtitle)
 WHERE m.code = 'demo'
   AND c.name = v.category
   AND NOT EXISTS (SELECT 1 FROM products p
                    WHERE p.merchant_id = c.merchant_id AND p.title = v.title);

-- ---------------------------------------------------------------------------
-- SKU 与库存
-- ---------------------------------------------------------------------------
--
-- 下单链路（M2 Task 4/5）要的是 SKU 和库存行，而不是商品本身 ——
-- 被扣减的是 inventories.available_qty。
--
-- 三件商品各有两个**不同价**的 SKU（手冲咖啡壶 12900/15900、
-- 挂耳咖啡 6900/8900、亚麻四件套 34900/42800）。00019 之后价格区间是从这里
-- 现算出来的（products 上那两列已经删了），所以「min_price_cents 是不是真的
-- 等于最便宜那个 SKU」这条断言的靶子就是这几行 —— 一件商品只有一个 SKU、
-- 或者两个 SKU 同价的话，把 min() 写成 max() 也不会红。
INSERT INTO skus (merchant_id, product_id, sku_code, spec_values, price_cents, status)
SELECT p.merchant_id, p.id, v.code, v.spec, v.cents, 1
  FROM products p
  JOIN merchants m ON m.id = p.merchant_id
  CROSS JOIN LATERAL (VALUES
        ('手冲咖啡壶',      'HCP-600',  '{"容量":"600ml"}'::jsonb,   12900::bigint),
        ('手冲咖啡壶',      'HCP-900',  '{"容量":"900ml"}'::jsonb,   15900::bigint),
        ('陶瓷马克杯',      'MUG-2',    '{"装量":"两只"}'::jsonb,     4900::bigint),
        ('挂耳咖啡 10 包',  'DRIP-10',  '{"烘焙":"中度"}'::jsonb,     6900::bigint),
        ('挂耳咖啡 10 包',  'DRIP-20',  '{"烘焙":"深度"}'::jsonb,     8900::bigint),
        ('意式浓缩咖啡机',  'ESP-01',   '{"颜色":"哑黑"}'::jsonb,   189900::bigint),
        ('法压壶',          'FP-350',   '{"容量":"350ml"}'::jsonb,    7680::bigint),
        ('手摇磨豆机',      'GRD-01',   '{"磨芯":"不锈钢"}'::jsonb,  23800::bigint),
        ('冷萃咖啡液 6 支', 'CB-6',     '{"口味":"无糖"}'::jsonb,     5580::bigint),
        ('雪纺碎花连衣裙',  'DRESS-M',  '{"尺码":"M"}'::jsonb,       19900::bigint),
        ('真丝吊带长裙',    'SKIRT-S',  '{"尺码":"S"}'::jsonb,       45900::bigint),
        ('羊毛混纺大衣',    'COAT-M',   '{"尺码":"M"}'::jsonb,       89900::bigint),
        ('高腰阔腿牛仔裤',  'JEAN-28',  '{"尺码":"28"}'::jsonb,      25900::bigint),
        ('针织开衫外套',    'CARD-F',   '{"尺码":"均码"}'::jsonb,    16800::bigint),
        ('亚麻四件套',      'BED-15',   '{"尺寸":"1.5 米"}'::jsonb,  34900::bigint),
        ('亚麻四件套',      'BED-18',   '{"尺寸":"1.8 米"}'::jsonb,  42800::bigint),
        ('香薰蜡烛',        'CAND-CD',  '{"香型":"雪松"}'::jsonb,     9900::bigint),
        ('室内扩香藤条',    'DIFF-01',  '{"容量":"180ml"}'::jsonb,    6880::bigint),
        ('羊毛地毯',        'RUG-16',   '{"尺寸":"1.6x2.3"}'::jsonb, 128000::bigint),
        ('入耳式蓝牙耳机',  'TWS-01',   '{"颜色":"星白"}'::jsonb,    39900::bigint),
        ('头戴降噪耳罩',    'HP-40',    '{"颜色":"雾灰"}'::jsonb,    59900::bigint),
        ('机械键盘 87 键',  'KB-87',    '{"轴体":"茶轴"}'::jsonb,    43800::bigint),
        ('快充充电宝',      'PB-20K',   '{"容量":"20000mAh"}'::jsonb, 21900::bigint),
        ('黑巧克力礼盒',    'CHOC-72',  '{"可可":"72%"}'::jsonb,      8850::bigint),
        ('冻干草莓脆',      'FD-STR',   '{"规格":"3 袋"}'::jsonb,     3980::bigint),
        ('明前龙井茶',      'TEA-LJ',   '{"规格":"100g"}'::jsonb,    26800::bigint)
     ) AS v(title, code, spec, cents)
 WHERE m.code = 'demo'
   AND p.title = v.title
   AND NOT EXISTS (SELECT 1 FROM skus s
                    WHERE s.merchant_id = p.merchant_id AND s.sku_code = v.code);

-- 每个 SKU 一行库存。数量刻意不相等：全都是同一个数的话，「扣的是不是这一个
-- SKU」在断言里看不出来。
--
-- **CARD-F 是 0**，它是 in_stock 那条判据唯一的反例：没有一件 0 库存的商品，
-- 把 `EXISTS (... AND i.available_qty > 0)` 里那个 `> 0` 删掉也不会红。
--
-- 全部挂在上面那家默认门店上（00020：主键是 (sku_id, store_id)）。用 JOIN 取
-- 门店而不是标量子查询：门店那一行缺失时这条语句应当**一行都不插**，
-- 让「没有库存」当场暴露，而不是插一个 NULL 去撞 NOT NULL —— 后者的报错
-- 指向约束名，指不回真因。
INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty, warning_qty)
SELECT s.id, st.id, s.merchant_id, v.qty, 5
  FROM skus s
  JOIN merchants m ON m.id = s.merchant_id
  JOIN stores st ON st.merchant_id = s.merchant_id
                AND st.is_default AND st.deleted_at IS NULL
  CROSS JOIN LATERAL (VALUES
        ('HCP-600', 20), ('HCP-900', 12), ('MUG-2', 50),
        ('DRIP-10', 30), ('DRIP-20', 18), ('ESP-01', 6),
        ('FP-350', 22), ('GRD-01', 14), ('CB-6', 40),
        ('DRESS-M', 25), ('SKIRT-S', 11), ('COAT-M', 8),
        ('JEAN-28', 16), ('CARD-F', 0),
        ('BED-15', 13), ('BED-18', 9), ('CAND-CD', 33),
        ('DIFF-01', 27), ('RUG-16', 4),
        ('TWS-01', 21), ('HP-40', 7), ('KB-87', 13), ('PB-20K', 19),
        ('CHOC-72', 26), ('FD-STR', 44), ('TEA-LJ', 10)
     ) AS v(code, qty)
 WHERE m.code = 'demo'
   AND s.sku_code = v.code
   AND NOT EXISTS (SELECT 1 FROM inventories i
                    WHERE i.sku_id = s.id AND i.store_id = st.id);

-- ---------------------------------------------------------------------------
-- 可登录的买家
-- ---------------------------------------------------------------------------
--
-- **口令是固定的，写在这里：手机号 13800000000，密码 keel-demo-2026。**
-- 冒烟脚本（scripts/smoke.sh）与 M2 Task 4/5 的端到端都要用它登录。
-- 这是**开发种子**里的固定口令，和「生产环境有个默认密码」是两件事：
-- 这个文件只由 compose 的 seed 服务加载，而那一栈本来就是本地演示用的。
--
-- password_hash 是 argon2id 的 PHC 字符串（internal/auth/password.go 生成）。
-- 它**不是**在这里现算的 —— SQL 里没有 argon2，pgcrypto 也只到 bcrypt。
-- 于是这一行是一份预先算好的常量，重算它要跑那个包里的 HashPassword。
-- 盐是随机的，所以同一个口令在 dev.sql 里那几行长得完全不一样，那是对的。
--
-- nickname 不留空：契约里 User.nickname 是必填的，空串会让前端显示一个
-- 没有名字的人 —— 而那不是种子想验证的东西。
INSERT INTO users (merchant_id, phone, password_hash, nickname, status)
SELECT m.id, '13800000000',
       '$argon2id$v=19$m=19456,t=2,p=1$J4kXTFFYK0Ts2p5Co+JeQg$RKPijLxGoS00y5FOKXGh260eD1CC5AT6IlGEo0vC7qY',
       '示例买家', 1
  FROM merchants m
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM users u
                    WHERE u.merchant_id = m.id AND u.phone = '13800000000');

-- ---------------------------------------------------------------------------
-- 收货地址
-- ---------------------------------------------------------------------------
--
-- 契约里 OrderCreateRequest.address_id 是**必填**，没有地址就一单也下不了。
-- 冒烟脚本与 M2 Task 4/5 的端到端都要用这一条：先 /auth/login 拿令牌，
-- 再 GET 不到地址列表（地址簿接口还没做），所以它的 id 只能从这里来 ——
-- 端到端脚本按 receiver_name 反查。
--
-- 幂等守卫用 (user_id, receiver_name)，理由同 dev.sql：这张表上没有能撞到的
-- 唯一约束，ON CONFLICT 在这里什么也不做。
INSERT INTO user_addresses (merchant_id, user_id, receiver_name, phone,
                            province, city, district, street, detail,
                            region_code, is_default)
SELECT u.merchant_id, u.id, '示例收件人', '13800000000',
       '浙江省', '杭州市', '西湖区', '文三路', '1 号楼 101', '330106', TRUE
  FROM users u
  JOIN merchants m ON m.id = u.merchant_id
 WHERE m.code = 'demo'
   AND u.phone = '13800000000'
   AND NOT EXISTS (SELECT 1 FROM user_addresses a
                    WHERE a.user_id = u.id AND a.receiver_name = '示例收件人');

-- ---------------------------------------------------------------------------
-- 检索用的派生数据（M3 Task 4）
-- ---------------------------------------------------------------------------
--
-- **混合检索需要几对「字面不像、意思相近」的商品**，否则
-- `docker compose up` 起来的这套演示里，双路召回与单路召回的结果一模一样，
-- 而 M3 的产出标志是「自然语言搜索可用」—— 看不出区别的演示等于没有演示。
-- 上面那批商品里埋了三对，逐对写在那段注释里。
--
-- 以「雪纺碎花连衣裙 / 真丝吊带长裙」为例：搜「连衣裙」时，前者由关键词那一路
-- 命中（search_text 里有「连衣」「衣裙」两个二元组），后者**一个二元组都不共享**
-- （它切出来的是「真丝 丝吊 吊带 带长 长裙」），只能由向量那一路捞回来。
-- 这一对是 scripts/smoke.sh 检索那一段的靶子。

-- ### search_text：预先算好的 bigram 串
--
-- **为什么种子要自己写这一列，而不是等索引任务去写。**
--
-- 派生数据入库任务（service/index.go）只在配了 KEEL_EMBED_ENDPOINT 时才启动，
-- 而 README 承诺的那条 `docker compose up` 里**没有推理引擎**（引擎是 infero，
-- GPU-only，跑在 compose 之外的宿主机进程里，见 scripts/infero-up.sh）。
-- 没有 GPU 的机器更是永远处在这个状态。
-- 于是默认那一栈里 search_text 永远是 NULL，关键词那一路一条也召不回 ——
-- 而语义检索层 §8 明说「任何一环故障，搜索都必须仍能返回结果」。
-- 没有这几行，那条降级链在默认演示里是**看不到**的：搜什么都是空的。
--
-- 代价是这一列变成了「SQL 里的一份 bigram 切分结果」，而 internal/search 的
-- 包注释里写着「两侧必须切得一模一样」。所以它不许靠人眼维护：
-- internal/search/seed_bigram_test.go 会读这个文件、把下面每一行的
-- (title, subtitle) 重新喂给 search.ProductText.SearchText()，逐字比对。
-- 改了切分算法而忘了改这里 → 那条测试红。
--
-- 下面这张表的边界由这两行注释标出来，测试按它们定位。不要改这两行的文字。
-- @keel:bigram-fixture:begin
UPDATE products p
   SET search_text = v.search_text
  FROM (VALUES
        ('手冲咖啡壶',      '600ml 玻璃',      '手冲 冲咖 咖啡 啡壶 600ml 玻璃'),
        ('陶瓷马克杯',      '两只装',          '陶瓷 瓷马 马克 克杯 两只 只装'),
        ('挂耳咖啡 10 包',  '中度烘焙',        '挂耳 耳咖 咖啡 10 包 中度 度烘 烘焙'),
        ('意式浓缩咖啡机',  '家用半自动',      '意式 式浓 浓缩 缩咖 咖啡 啡机 家用 用半 半自 自动'),
        ('法压壶',          '350ml 耐热玻璃',  '法压 压壶 350ml 耐热 热玻 玻璃'),
        ('手摇磨豆机',      '不锈钢磨芯',      '手摇 摇磨 磨豆 豆机 不锈 锈钢 钢磨 磨芯'),
        ('冷萃咖啡液 6 支', '无糖',            '冷萃 萃咖 咖啡 啡液 6 支 无糖'),
        ('雪纺碎花连衣裙',  '夏季新款 显瘦',   '雪纺 纺碎 碎花 花连 连衣 衣裙 夏季 季新 新款 显瘦'),
        ('真丝吊带长裙',    '法式复古',        '真丝 丝吊 吊带 带长 长裙 法式 式复 复古'),
        ('羊毛混纺大衣',    '通勤直筒',        '羊毛 毛混 混纺 纺大 大衣 通勤 勤直 直筒'),
        ('高腰阔腿牛仔裤',  '显腿长',          '高腰 腰阔 阔腿 腿牛 牛仔 仔裤 显腿 腿长'),
        ('针织开衫外套',    '慵懒风',          '针织 织开 开衫 衫外 外套 慵懒 懒风'),
        ('亚麻四件套',      '1.8 米床 裸睡级', '亚麻 麻四 四件 件套 1 8 米床 裸睡 睡级'),
        ('香薰蜡烛',        '雪松木质调',      '香薰 薰蜡 蜡烛 雪松 松木 木质 质调'),
        ('室内扩香藤条',    '无火 持久',       '室内 内扩 扩香 香藤 藤条 无火 持久'),
        ('羊毛地毯',        '北欧几何',        '羊毛 毛地 地毯 北欧 欧几 几何'),
        ('入耳式蓝牙耳机',  '真无线 长续航',   '入耳 耳式 式蓝 蓝牙 牙耳 耳机 真无 无线 长续 续航'),
        ('头戴降噪耳罩',    '包耳设计 舒适',   '头戴 戴降 降噪 噪耳 耳罩 包耳 耳设 设计 舒适'),
        ('机械键盘 87 键',  '茶轴 热插拔',     '机械 械键 键盘 87 键 茶轴 热插 插拔'),
        ('快充充电宝',      '20000mAh 65W',    '快充 充充 充电 电宝 20000mah 65w'),
        ('黑巧克力礼盒',    '72% 可可',        '黑巧 巧克 克力 力礼 礼盒 72 可可'),
        ('冻干草莓脆',      '无添加蔗糖',      '冻干 干草 草莓 莓脆 无添 添加 加蔗 蔗糖'),
        ('明前龙井茶',      '100g 罐装',       '明前 前龙 龙井 井茶 100g 罐装')
       ) AS v(title, subtitle, search_text),
       merchants m
 WHERE m.code = 'demo'
   AND p.merchant_id = m.id
   AND p.title = v.title
   AND p.search_text IS DISTINCT FROM v.search_text;
-- @keel:bigram-fixture:end

-- ---------------------------------------------------------------------------
-- 优惠券（领券中心里可以直接领的两张）
-- ---------------------------------------------------------------------------
--
-- 两张而不是一张：结算页的「选券」要有得选才看得出它在做什么 —— 同一单
-- 两张都能用时，本单可用券按能减的金额降序排，买家看得到哪张更划算。
--
--   · 满 50 减 10：演示商品大多几十元，一单一两件就过门槛；
--   · 全场 9 折最高减 30：折扣券的取整与封顶在大单上看得见。
--
-- 都是「领取后 30 天有效」而不是绝对时间：种子是长期跑着的演示栈每次启动都会
-- 灌的，写死一个截止日期，过了那天演示栈上的券就全部失效，而且没人会注意到。
-- 不设适用范围行 = 全场、所有门店（某一类没有「包含」规则就等于这一类不限，
-- 见数据模型 §7）。
--
-- merchant_id 显式写：种子以管理员身份跑，没有租户上下文，列默认值
-- current_merchant() 在这里会抛 42501。
INSERT INTO coupon_templates (merchant_id, name, coupon_type, threshold_cents, discount_cents,
                              discount_rate, max_discount_cents, valid_mode, valid_days,
                              total_count, per_user_limit, claimable)
SELECT m.id, v.name, v.coupon_type, v.threshold_cents, v.discount_cents,
       v.discount_rate, v.max_discount_cents, 2, 30, 0, 1, TRUE
  FROM merchants m
  CROSS JOIN (VALUES
      ('满 50 减 10',        1::smallint, 5000::bigint, 1000::bigint, 0::smallint,   0::bigint),
      ('全场 9 折最高减 30', 2::smallint,    0::bigint,    0::bigint, 900::smallint, 3000::bigint)
  ) AS v(name, coupon_type, threshold_cents, discount_cents, discount_rate, max_discount_cents)
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM coupon_templates c
                    WHERE c.merchant_id = m.id AND c.name = v.name);

-- ---------------------------------------------------------------------------
-- 运费模板（00055）：全店默认一个，让演示栈的结算页有运费可看
-- ---------------------------------------------------------------------------
--
-- 口径是国内小店最常见的那种：
--
--   · 全国（默认规则）：首件 8 元，每续 1 件 2 元，满 99 元包邮；
--   · 偏远地区（新疆、西藏、青海、内蒙古、宁夏）：首件 15 元，每续 1 件 5 元，**不包邮**；
--   · 港澳台不配送（下单时逐行报 422 region-not-deliverable）。
--
-- 演示买家的收货地址在杭州（区划码 330106），走默认规则：一单一两件几十元的商品
-- 会看到 8～10 元运费，凑到 99 元就包邮 —— 结算页上「满额包邮按券后金额判」看得见。
--
-- 按件计费而不是按重量：种子里的商品都没填重量（skus.weight_gram 为 0），
-- 按重量的话每单都只收首重，演示不出续件。
--
-- 幂等：模板按 (merchant_id, name) 守，规则按「这个模板还没有规则」守 ——
-- 规则表上的唯一约束只管「至多一条默认规则」，挡不住重复灌指定地区那一条。
-- merchant_id 显式写，理由同上面几段（管理员角色加载，没有租户上下文）。
INSERT INTO freight_templates (merchant_id, name, charge_mode, is_default, undeliverable_region_codes)
SELECT m.id, '全国运费（满 99 包邮）', 1, TRUE, ARRAY['710000', '810000', '820000']
  FROM merchants m
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM freight_templates t
                    WHERE t.merchant_id = m.id AND t.name = '全国运费（满 99 包邮）')
   -- 商家自己在后台另设了默认模板时不抢（uk_freight_templates_default 也会拒绝）。
   AND NOT EXISTS (SELECT 1 FROM freight_templates t
                    WHERE t.merchant_id = m.id AND t.is_default AND t.deleted_at IS NULL);

INSERT INTO freight_template_rules (merchant_id, template_id, sort_order, region_codes,
                                    first_unit, first_fee_cents, additional_unit, additional_fee_cents,
                                    free_threshold_cents, free_quantity)
SELECT t.merchant_id, t.id, v.sort_order, v.region_codes, 1, v.first_fee, 1, v.additional_fee,
       v.free_threshold, 0
  FROM freight_templates t
  JOIN merchants m ON m.id = t.merchant_id
  CROSS JOIN (VALUES
      (0, ARRAY['650000', '540000', '630000', '150000', '640000']::TEXT[], 1500::bigint, 500::bigint, 0::bigint),
      (1, ARRAY[]::TEXT[],                                                   800::bigint, 200::bigint, 9900::bigint)
  ) AS v(sort_order, region_codes, first_fee, additional_fee, free_threshold)
 WHERE m.code = 'demo'
   AND t.name = '全国运费（满 99 包邮）'
   AND NOT EXISTS (SELECT 1 FROM freight_template_rules r WHERE r.template_id = t.id);
