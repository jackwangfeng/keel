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
