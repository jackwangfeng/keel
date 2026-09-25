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
