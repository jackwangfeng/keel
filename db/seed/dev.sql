-- 开发与测试用的种子数据。
--
-- 刻意放在 db/migrations 之外：迁移描述的是 schema，任何环境都要跑；
-- 种子是测试夹具，只该出现在开发库和 CI 库里。混进迁移的测试数据，
-- 迟早会随着一次例行 `goose up` 落到生产库上。
--
-- 用管理员角色加载（compose 里是 `psql postgres://keel:keel@...`）：
-- categories/products 之后会带 RLS，keel_app 在没有租户上下文的连接上
-- 一行都插不进去。
--
-- ### 幂等性
--
-- 每条 INSERT 都带 `WHERE NOT EXISTS` 守卫，而不是 `ON CONFLICT DO NOTHING`。
-- 两者看着等价，实则不是：ON CONFLICT 只在「真有一个唯一约束被撞到」时才
-- 沉默跳过，没有对应约束的表上它什么也不做 —— 重复加载会一遍遍累积重复行。
-- 而测试每次都会加载这个文件，于是「每个租户 N 件商品」这类断言的结果会随
-- 加载次数漂移：第一次绿，第二次开始红，或者更糟 —— 断言写成 >= 时永远绿。
-- NOT EXISTS 不依赖任何约束，表上有没有唯一键都幂等。

-- 商家。
--   shop-a        正常，单商家部署里的默认商家
--   shop-b        正常，多商家部署里按子域名解析
--   shop-c        正常，绑定了自定义域名（域名与 code 无关）
--   shop-closed   status = 2，停用；解析必须把它当作不存在
--   shop-deleted  软删（deleted_at 非空）但 status 仍是 1；解析同样必须拒绝
--   shop-nodomain 正常，但没登记域名；多商家部署下它只能靠子域名访问，
--                 这正是 Preflight「没配 KEEL_BASE_DOMAIN 就有店打不开」那条检查的靶子
INSERT INTO merchants (code, name, status, deleted_at)
SELECT v.code, v.name, v.status, v.deleted_at
  FROM (VALUES
          ('shop-a',        '示例店 A',     1::smallint, NULL::timestamptz),
          ('shop-b',        '示例店 B',     1::smallint, NULL::timestamptz),
          ('shop-c',        '示例店 C',     1::smallint, NULL::timestamptz),
          ('shop-closed',   '已停用的店',   2::smallint, NULL::timestamptz),
          ('shop-deleted',  '已删除的店',   1::smallint, now()),
          ('shop-nodomain', '没登记域名的店', 1::smallint, NULL::timestamptz)
       ) AS v(code, name, status, deleted_at)
 WHERE NOT EXISTS (SELECT 1 FROM merchants m WHERE m.code = v.code);

-- 子域名形态的店铺：域名恰好是 code + 平台基础域名，解析走 merchants.code 一支。
-- shop-nodomain 刻意不在这个列表里。
INSERT INTO shop_settings (merchant_id, domain)
SELECT m.id, m.code || '.example.com'
  FROM merchants m
 WHERE m.code IN ('shop-a', 'shop-b', 'shop-closed', 'shop-deleted')
   AND NOT EXISTS (SELECT 1 FROM shop_settings s WHERE s.merchant_id = m.id);

-- 自定义域名形态：域名落在平台基础域名之外，只能走 shop_settings.domain 一支。
-- 这条是给解析器的第二个匹配分支当靶子的 —— 如果那一支拿子域名（'custom'）
-- 去比完整域名，它永远匹配不上，而只有本行这种数据能让那个 bug 现形。
INSERT INTO shop_settings (merchant_id, domain)
SELECT m.id, 'custom.example.net'
  FROM merchants m
 WHERE m.code = 'shop-c'
   AND NOT EXISTS (SELECT 1 FROM shop_settings s WHERE s.merchant_id = m.id);

-- ### 支付渠道的回调验签密钥（M2 任务 7）
--
-- 支付回调是**未认证入口**：租户由 Host 定，而「这份报文是不是真的」由验签定。
-- 密钥是每租户每渠道一把，存在 shop_settings.extra 里（数据模型 §1 建的那一列）——
-- 不为它新建一张表的理由写在 internal/repository/payment.go 的
-- ChannelNotifySecret 上（一张文档里没有的表会让 check_tenancy 当场红）。
--
-- **这几把密钥写死在种子里，和买家口令一样**（口令的理由见下面 users 那段）：
-- 它们是测试夹具，不是凭据。真实部署里这一列要由建店流程写入。
--
-- shop-a 与 shop-b 都配上，**shop-c 刻意不配** —— 它是「没配密钥的店」那条
-- 断言的靶子：没配密钥必须是**拒绝**（401），不是「跳过验签」。
-- 那是这类代码最经典的洞，而没有一家没配密钥的店，那条断言就没有可达的路径。
--
-- **balance 这一把是刻意配上的，而契约里 webhook 根本没有 balance 这个渠道**
-- （余额支付不产生渠道回调）。它是「渠道白名单」那条断言的靶子：
-- 不配的话，「一条 balance 回调被拒了」既可能是白名单挡的、也可能只是因为
-- 没有 balance 密钥可验 —— 两件事分不开，而把 balance 加进白名单的那种改动
-- 会照样绿。配上之后，那条断言只由白名单守着。（变异验证 P6 就是这么发现的。）
--
-- jsonb_set 的第四个参数 true = 路径不存在时创建。用它而不是整个覆盖 extra：
-- 这份种子会被反复加载，整个覆盖会把别的测试往 extra 里放的东西冲掉。
UPDATE shop_settings s
   SET extra = jsonb_set(s.extra, ARRAY['payment_channels'],
                         jsonb_build_object(
                             'wechat', jsonb_build_object('notify_secret', 'seed-wechat-secret-' || m.code),
                             'alipay', jsonb_build_object('notify_secret', 'seed-alipay-secret-' || m.code),
                             'balance', jsonb_build_object('notify_secret', 'seed-balance-secret-' || m.code)),
                         true)
  FROM merchants m
 WHERE m.id = s.merchant_id
   AND m.code IN ('shop-a', 'shop-b')
   AND NOT (s.extra ? 'payment_channels');

-- ### 商品
--
-- shop-a 与 shop-b 各自有商品，**件数刻意不同**（3 与 2）。
--
-- 两点都是必须的。只给一家播商品的话，跨租户测试形同虚设：另一边拿到空列表，
-- 而空列表既可能是 RLS 把别家的数据挡住了，也可能只是那家店本来就没有商品。
-- 件数相同的话，「两边都返回 3 件」这个观察同样区分不开「各看各的 3 件」和
-- 「都看到了全部 3 件」——只有当 a 是 3、b 是 2，RLS 一旦失效两边就都变成 5，
-- 断言立刻红。
--
-- 用 WHERE NOT EXISTS 而不是 ON CONFLICT DO NOTHING：categories 与 products 上
-- 没有能撞到的唯一约束（UNIQUE (id, merchant_id) 里的 id 是自增的，永远撞不上），
-- 所以 ON CONFLICT 在这两张表上什么也不做，重复加载会一遍遍累积重复行 ——
-- 实测两次之后行数从 2 变成 4。而测试每跑一次就加载一次这个文件。
INSERT INTO categories (merchant_id, name, path, status)
SELECT m.id, '默认分类', '/', 1
  FROM merchants m
 WHERE m.code IN ('shop-a', 'shop-b')
   AND NOT EXISTS (SELECT 1 FROM categories c
                    WHERE c.merchant_id = m.id AND c.name = '默认分类');

-- generate_series 的上界按店取：件数不同才让跨租户断言有区分力（见上）。
-- 它引用了同一个 FROM 里的 m —— FROM 里的集合返回函数是隐式 LATERAL 的。
--
-- description 也播上：契约的 ProductDetail 里有它，而 products.description
-- 在种子里一直是 NULL —— 那会让「详情把 description 填出来了」这句话没有靶子
-- （字段是可选的，NULL 时它整个不出现，与「压根没实现」长得一模一样）。
INSERT INTO products (merchant_id, category_id, title, description,
                      total_stock, sales_count, status, published_at)
SELECT c.merchant_id, c.id, m.code || ' 的商品 ' || g,
       m.code || ' 的商品 ' || g || ' 的详细描述', 100, 0, 1, now()
  FROM merchants m
  JOIN categories c ON c.merchant_id = m.id AND c.name = '默认分类'
  CROSS JOIN generate_series(1, CASE m.code WHEN 'shop-a' THEN 3 ELSE 2 END) g
 WHERE m.code IN ('shop-a', 'shop-b')
   AND NOT EXISTS (SELECT 1 FROM products p
                    WHERE p.merchant_id = c.merchant_id
                      AND p.title = m.code || ' 的商品 ' || g);

-- 反例商品：shop-a 各一件草稿（status = 0）与一件软删（deleted_at 非空）。
--
-- 它们的作用是让 ListProducts / CountProducts 里那两个谓词**可被证伪**。
-- 没有它们的话，`WHERE deleted_at IS NULL AND status = 1` 这两行删掉之后
-- 查询结果一个字都不变，全部测试照样绿 —— 而 CountProducts 的注释里写着
-- 「条件必须与 ListProducts 逐字一致」，那句话原本没有任何东西在守。
--
-- 同理，status 字段在响应里带不带 omitempty 也要靠反例才看得出来：
-- 在架商品的 status 恒为 1，omitempty 永远不触发。
--
-- 件数刻意不进 shop-a 的那 3 件里：它们必须**不**出现在列表和 total 里，
-- 所以 wantA 仍然是 3，而库里 shop-a 实际有 5 行。
-- internal/handler 的 TestDraftAndDeletedProductsAreInvisible 拿这个差值做断言。
INSERT INTO products (merchant_id, category_id, title,
                      total_stock, sales_count, status,
                      published_at, deleted_at)
SELECT c.merchant_id, c.id, v.title, 100, 0, v.status, now(), v.deleted_at
  FROM merchants m
  JOIN categories c ON c.merchant_id = m.id AND c.name = '默认分类'
  CROSS JOIN (VALUES
                ('shop-a 的草稿商品', 0::smallint, NULL::timestamptz),
                ('shop-a 的已删商品', 1::smallint, now())
             ) AS v(title, status, deleted_at)
 WHERE m.code = 'shop-a'
   AND NOT EXISTS (SELECT 1 FROM products p
                    WHERE p.merchant_id = c.merchant_id AND p.title = v.title);

-- ---------------------------------------------------------------------------
-- 大区与门店
-- ---------------------------------------------------------------------------
--
-- 00020 之后 inventories 的主键是 (sku_id, store_id) 且 store_id NOT NULL：
-- 没有门店就一行库存也插不进去。那条迁移里的回填只覆盖**迁移那一刻库里已有
-- 的**商家，而这六家全是迁移之后才插的，所以门店得自己播。
--
-- 六家都播，包括停用的（shop-closed）和软删的（shop-deleted）：理由与迁移
-- 回填那一段同一条 —— 「跳过不营业的商家」会让它名下的行无处可落，而报错指向
-- 的是一条 NOT NULL 约束，不是这个判断。何况这份种子会重复加载，
-- 「哪几家有门店」与「哪几家有商品」不是同一批的话，下一个往 shop-c 加一件
-- 商品的人会撞上一条完全看不出真因的报错。
--
-- is_default = TRUE 且不画围栏：默认店靠「全国兜底」接单，不靠围栏（「非默认 且
-- 无围栏」这一种组合。这批夹具没有一条断言碰地理围栏，画一个假多边形只会让
-- 后来人以为那几个坐标是有意义的。location 同理留空。
--
-- 显式写 merchant_id 而不吃列默认值：这个文件由管理员角色加载
-- （见文件头），连接上没有 app.merchant_id，current_merchant() 会直接 RAISE。
INSERT INTO regions (merchant_id, code, name)
SELECT m.id, 'default', '默认大区'
  FROM merchants m
 WHERE m.code IN ('shop-a', 'shop-b', 'shop-c',
                  'shop-closed', 'shop-deleted', 'shop-nodomain')
   AND NOT EXISTS (SELECT 1 FROM regions r
                    WHERE r.merchant_id = m.id AND r.code = 'default');

INSERT INTO stores (merchant_id, region_id, code, name, is_default)
SELECT r.merchant_id, r.id, 'default', '默认门店', TRUE
  FROM regions r
  JOIN merchants m ON m.id = r.merchant_id
 WHERE m.code IN ('shop-a', 'shop-b', 'shop-c',
                  'shop-closed', 'shop-deleted', 'shop-nodomain')
   AND r.code = 'default'
   AND NOT EXISTS (SELECT 1 FROM stores st
                    WHERE st.merchant_id = r.merchant_id AND st.code = 'default');

-- ---------------------------------------------------------------------------
-- SKU 与库存
-- ---------------------------------------------------------------------------
--
-- 每件商品一个 SKU（含草稿与软删的那两件 —— 商品下架不等于 SKU 消失，
-- 而「下架商品的 SKU 还能不能被下单」正是需要能被证伪的那类问题）。
--
-- sku_code 由商品标题派生，保证重复执行时 NOT EXISTS 认得出来。
--
-- 价格是一个常量 1990。00019 之前它写的是 `p.min_price_cents`，
-- 而那一列现在没有了 —— 它本来就是从 SKU 算回去的，拿它当 SKU 的价格
-- 是一条循环定义（也正因为如此，「区间算得对不对」在这批数据上证伪不了）。
INSERT INTO skus (merchant_id, product_id, sku_code, price_cents, status)
SELECT p.merchant_id, p.id, 'SKU-' || p.id, 1990, 1
  FROM products p
  JOIN merchants m ON m.id = p.merchant_id
 WHERE m.code IN ('shop-a', 'shop-b')
   AND NOT EXISTS (SELECT 1 FROM skus s
                    WHERE s.merchant_id = p.merchant_id AND s.product_id = p.id);

-- 库存数量按 SKU id 取模错开，理由同 single.sql：全相等就分不出扣的是哪一个。
--
-- SKU-NOSTOCKROW 刻意排除在外 —— 它就是为了「有 SKU、没有库存行」这个状态而
-- 存在的（见文件末尾那段）。排除条件写在这里而不是靠「它在这条语句之后才建出来」
-- ：这个文件每次测试都会重新加载一遍，第二次加载时它已经在库里了，
-- 少了这个条件就会被顺手补上一行库存，而那条断言会从此空转。
--
-- 库存挂在上面那家默认门店上（00020：store_id NOT NULL，主键是
-- (sku_id, store_id)）。JOIN 而不是标量子查询：少了门店那一行时这条语句
-- 应当**一行都不插**并让后续断言红，而不是插进一个 NULL 去撞 NOT NULL ——
-- 后者的报错指向约束，指不回这里。
-- merchant_id 同样显式写：管理员连接上没有 app.merchant_id，列默认值
-- current_merchant() 会 RAISE。
INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty, warning_qty)
SELECT s.id, st.id, s.merchant_id, 10 + (s.id % 7) * 5, 3
  FROM skus s
  JOIN merchants m ON m.id = s.merchant_id
  JOIN stores st ON st.merchant_id = s.merchant_id
                AND st.is_default AND st.deleted_at IS NULL
 WHERE m.code IN ('shop-a', 'shop-b')
   AND s.sku_code <> 'SKU-NOSTOCKROW'
   AND NOT EXISTS (SELECT 1 FROM inventories i
                    WHERE i.sku_id = s.id AND i.store_id = st.id);

-- ---------------------------------------------------------------------------
-- 买家
-- ---------------------------------------------------------------------------
--
-- **口令固定：密码 keel-dev-2026**，手机号见下。internal/handler 与
-- internal/auth 的测试拿它登录，M2 Task 4/5 的下单链路也要用。
--
-- ### shop-a 与 shop-b 的买家共用同一个手机号，这是刻意的
--
-- 数据模型 §8/§9 定的是「买家属于商家」—— 同一个人在 A 店和 B 店是**两行
-- users**，uk_users_phone 的首列是 merchant_id 正是这条语义的落地。
-- 两边用同一个号码之后，下面这些断言才有区分力：
--
--   · 在 A 店按这个号码登录，拿到的必须是 A 店那一行（user_id 不同、昵称不同）；
--   · A 店签出的令牌拿去 B 店，必须被当作**鉴权失败**拒绝，
--     而不是「在 B 店查不到这个人」—— 这家店明明有这个号码；
--   · 唯一索引没收进租户内的话，第二家店的这一行根本插不进去，种子当场失败。
--
-- 号码不同的话，跨店那条测试的「被拒绝」既可能是租户校验起了作用，
-- 也可能只是因为 B 店压根没有这个人。那就什么也证明不了。
--
-- ### 三个反例
--
--   13800000002  status = 2（封禁）：401 与 403 是契约里两种不同的响应，
--                没有一个被封的账号，那两条分支里永远只有一条被走到。
--   13800000003  password_hash 为空（仅第三方登录的账号，§9 明写这一列可空）：
--                契约要求这种账号传 password 回 401，而不是 500。
--   13800000004  已软删（deleted_at 非空）：uk_users_phone 带 deleted_at IS NULL，
--                所以它和 13800000001 可以共存于 shop-a —— 号码复用那条规则
--                （§9）在种子里就有一个活着的例子，而登录必须查不到它。
--
-- ### shop-a 里还有第二个**能登录、能下单**的买家（13800000005）
--
-- 它是「订单只能看见自己的」那条断言唯一可达的靶子。
--
-- 租户隔离由 RLS 挡，而**同一家店里 A 买家能不能读到 B 买家的订单**是 RLS 管不到
-- 的那一层（策略里只有 current_merchant()，它认不出买家）。只有同一个 merchant_id
-- 下的两个真实买家各自下过一单，`GET /orders` 与 `GET /orders/{order_no}` 上那个
-- user_id 条件才是可证伪的：把它删掉，两条断言当场红。
--
-- 上面三个反例都当不了这个靶子 —— 封禁的、没有口令的、已软删的，
-- 一个都登录不进来，也就一单都下不了。跨店的那个 13800000001@shop-b 也不行：
-- 它被 RLS 挡着，删掉 user_id 条件它照样看不见 A 店的订单，于是那条断言
-- 会在被测逻辑已经失效的情况下保持绿色。
INSERT INTO users (merchant_id, phone, password_hash, nickname, status, deleted_at)
SELECT m.id, v.phone, v.hash, v.nickname, v.status, v.deleted_at
  FROM merchants m
  CROSS JOIN (VALUES
        ('shop-a', '13800000001',
         '$argon2id$v=19$m=19456,t=2,p=1$KDDF6U9F6TwfTQ1agq3d2Q$KEULoLsSDb6bROT+qoKhR+rDPMp5LrgPjfIpBRE3bGk',
         'A 店的买家', 1::smallint, NULL::timestamptz),
        ('shop-b', '13800000001',
         '$argon2id$v=19$m=19456,t=2,p=1$iTGAOt8186mE/De9Kp87pg$FB1XmLG0ayHpZZU1Z4YAb8XZH7HXh31FhRXL7mwAFbQ',
         'B 店的买家', 1::smallint, NULL::timestamptz),
        ('shop-a', '13800000002',
         '$argon2id$v=19$m=19456,t=2,p=1$/KYk3di70nEP/Ta9DxhHWw$5wpDREBvZhMmIFH9OhyS4xBleIryuNdRlB0qtnvtZXA',
         'A 店的封禁买家', 2::smallint, NULL::timestamptz),
        ('shop-a', '13800000003', NULL,
         'A 店的仅第三方登录买家', 1::smallint, NULL::timestamptz),
        ('shop-a', '13800000004', NULL,
         'A 店的已注销买家', 1::smallint, now()),
        ('shop-a', '13800000005',
         '$argon2id$v=19$m=19456,t=2,p=1$coA0R4QAIztkIeGLvk6xdw$K/7KBumFV9XK6XzvHRglarBYZToW3JPVQYTkjPMNeNE',
         'A 店的第二个买家', 1::smallint, NULL::timestamptz)
     ) AS v(code, phone, hash, nickname, status, deleted_at)
 WHERE m.code = v.code
   AND NOT EXISTS (SELECT 1 FROM users u
                    WHERE u.merchant_id = m.id AND u.phone = v.phone
                      AND u.nickname = v.nickname);

-- ---------------------------------------------------------------------------
-- 收货地址
-- ---------------------------------------------------------------------------
--
-- 契约里 OrderCreateRequest.address_id 是**必填**，所以没有地址就一单也下不了。
-- 两家店的可登录买家各一条默认地址（13800000001 在 A 店和 B 店是两行 users，
-- 见上面那段）。
--
-- **两家都要有**，而且 receiver_name 不同：只给一家播的话，「A 店的买家用了
-- B 店的 address_id 会怎样」这条断言就没有靶子 —— 失败也可能只是因为那一行
-- 根本不存在，而那正是要排除的另一种解释。
--
-- 幂等守卫用 (user_id, receiver_name)：这张表上没有能撞到的唯一约束
-- （uk_user_addresses_default 只管「至多一个默认」），ON CONFLICT 在这里什么
-- 也不做，重复加载会一遍遍累积重复行。
INSERT INTO user_addresses (merchant_id, user_id, receiver_name, phone,
                            province, city, district, street, detail,
                            region_code, is_default)
SELECT u.merchant_id, u.id, v.receiver, '13800000001',
       '浙江省', '杭州市', '西湖区', '文三路', v.detail, '330106', TRUE
  FROM users u
  JOIN merchants m ON m.id = u.merchant_id
  CROSS JOIN (VALUES
        ('shop-a', 'A 店收件人', '100 号 1 单元 101'),
        ('shop-b', 'B 店收件人', '200 号 2 单元 202')
     ) AS v(code, receiver, detail)
 WHERE m.code = v.code
   AND u.phone = '13800000001'
   AND u.deleted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM user_addresses a
                    WHERE a.user_id = u.id AND a.receiver_name = v.receiver);

-- shop-a 第二个买家（13800000005）的地址。
--
-- 单独一条 INSERT 而不是并进上面那个 CROSS JOIN：那一段是按 (code, receiver)
-- 给「每家店的 13800000001」播的，硬塞进去要给它加一个号码维度，而那会让
-- 上面那两行的含义变得要读两遍才明白。
--
-- 它必须有地址：契约里 address_id 是必填的，没有地址这个买家就一单也下不了，
-- 而「订单只能看见自己的」那条断言需要他真的有一单。
INSERT INTO user_addresses (merchant_id, user_id, receiver_name, phone,
                            province, city, district, street, detail,
                            region_code, is_default)
SELECT u.merchant_id, u.id, 'A 店第二个收件人', '13800000005',
       '浙江省', '杭州市', '滨江区', '江南大道', '300 号 3 单元 303', '330108', TRUE
  FROM users u
  JOIN merchants m ON m.id = u.merchant_id
 WHERE m.code = 'shop-a'
   AND u.phone = '13800000005'
   AND u.deleted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM user_addresses a
                    WHERE a.user_id = u.id AND a.receiver_name = 'A 店第二个收件人');

-- ---------------------------------------------------------------------------
-- 一个**没有库存行**的 SKU（shop-a）
-- ---------------------------------------------------------------------------
--
-- **它守的东西本轮（00020）换了，行本身还要留着。**
--
-- 从前它是 repository.ErrSKUNotInTenant 在 HTTP 层唯一可达的靶子：那个
-- sentinel 当时同时覆盖「这一行属于别的商家」与「它根本没有库存行」。
--
-- 00020 把扣减的失败从两种拆成四种，并把「这家店根本没有这一行」挪进了
-- ErrInsufficientStock（internal/repository/inventory.go 的文件头逐条写着）。
-- 理由是库存按门店分之后「缺行」从罕见变成常态 —— 新店、新品、缺货清零都会
-- 缺行 —— 而判成「本店不卖」会让一家刚开的店在录库存之前什么都不卖。
--
-- 于是这一行现在守的是**那条新语义**：缺行 ≡ 可售 0，报 409 缺货，
-- 不是 422「本店不卖」。执行者是 internal/handler 的
-- TestMissingInventoryRowMeansZeroStockNotUnsold。
-- 它同时还是 products.sql 那条 in_stock 判据的反例（「有 SKU、没有库存行」
-- 的商品不算有货）—— 那一条本轮没变。
--
-- 挂在一件在架商品上（所以定价查得到它、试算会成功），但 inventories 里没有
-- 对应行 —— 而下面那条库存 INSERT 明确把它排除在外。
INSERT INTO skus (merchant_id, product_id, sku_code, spec_values, price_cents, status)
SELECT p.merchant_id, p.id, 'SKU-NOSTOCKROW', '{"备注":"刻意没有库存行"}'::jsonb, 1990, 1
  FROM products p
  JOIN merchants m ON m.id = p.merchant_id
 WHERE m.code = 'shop-a'
   AND p.title = 'shop-a 的商品 1'
   AND NOT EXISTS (SELECT 1 FROM skus s
                    WHERE s.merchant_id = p.merchant_id AND s.sku_code = 'SKU-NOSTOCKROW');
