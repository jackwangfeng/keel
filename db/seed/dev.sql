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
INSERT INTO products (merchant_id, category_id, title, min_price_cents,
                      max_price_cents, total_stock, sales_count, status, published_at)
SELECT c.merchant_id, c.id, m.code || ' 的商品 ' || g, 1990, 4990, 100, 0, 1, now()
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
INSERT INTO products (merchant_id, category_id, title, min_price_cents,
                      max_price_cents, total_stock, sales_count, status,
                      published_at, deleted_at)
SELECT c.merchant_id, c.id, v.title, 1990, 4990, 100, 0, v.status, now(), v.deleted_at
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
-- SKU 与库存
-- ---------------------------------------------------------------------------
--
-- 每件商品一个 SKU（含草稿与软删的那两件 —— 商品下架不等于 SKU 消失，
-- 而「下架商品的 SKU 还能不能被下单」正是需要能被证伪的那类问题）。
--
-- sku_code 由商品标题派生，保证重复执行时 NOT EXISTS 认得出来。
INSERT INTO skus (merchant_id, product_id, sku_code, price_cents, status)
SELECT p.merchant_id, p.id, 'SKU-' || p.id, p.min_price_cents, 1
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
INSERT INTO inventories (sku_id, available_qty, warning_qty)
SELECT s.id, 10 + (s.id % 7) * 5, 3
  FROM skus s
  JOIN merchants m ON m.id = s.merchant_id
 WHERE m.code IN ('shop-a', 'shop-b')
   AND s.sku_code <> 'SKU-NOSTOCKROW'
   AND NOT EXISTS (SELECT 1 FROM inventories i WHERE i.sku_id = s.id);

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
         'A 店的已注销买家', 1::smallint, now())
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

-- ---------------------------------------------------------------------------
-- 一个**没有库存行**的 SKU（shop-a）
-- ---------------------------------------------------------------------------
--
-- 它是 repository.ErrSKUNotInTenant 在 HTTP 层唯一可达的靶子。
--
-- 那个 sentinel 覆盖两件事：「这一行属于别的商家」与「它根本没有库存行」。
-- 前者在下单接口上**不可达** —— order_items 的复合外键
-- (sku_id, merchant_id) → skus(id, merchant_id) 让跨租户的订单行根本写不进去，
-- 而定价那一步更早就把别家的 sku_id 当成「不可售」拒了。
--
-- 没有这一行数据，「跨租户 SKU 不会被翻译成库存不足」这条断言就只能对着一条
-- 不可达的分支空转 —— 这个仓库前几轮反复出现的正是这种「断言存在但和被测代码
-- 没有因果关系」。有了它，POST /orders 能真的走到扣减那一步、真的拿到那个
-- sentinel，于是「它没有被翻译成 409 库存不足」才是一句被证实过的话。
--
-- 挂在一件在架商品上（所以定价查得到它、试算会成功），但 inventories 里没有
-- 对应行（所以扣减时那一行在本租户不可见）。
INSERT INTO skus (merchant_id, product_id, sku_code, spec_values, price_cents, status)
SELECT p.merchant_id, p.id, 'SKU-NOSTOCKROW', '{"备注":"刻意没有库存行"}'::jsonb, 1990, 1
  FROM products p
  JOIN merchants m ON m.id = p.merchant_id
 WHERE m.code = 'shop-a'
   AND p.title = 'shop-a 的商品 1'
   AND NOT EXISTS (SELECT 1 FROM skus s
                    WHERE s.merchant_id = p.merchant_id AND s.sku_code = 'SKU-NOSTOCKROW');
