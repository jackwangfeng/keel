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
-- 用管理员角色加载（compose 里的 seed 服务是 `psql postgres://keel:keel@...`）：
-- categories/products 带 RLS，keel_app 在没有租户上下文的连接上一行都插不进去。

INSERT INTO merchants (code, name, status)
SELECT 'demo', '示例小店', 1
 WHERE NOT EXISTS (SELECT 1 FROM merchants m WHERE m.code = 'demo');

INSERT INTO categories (merchant_id, name, path, status)
SELECT m.id, '默认分类', '/', 1
  FROM merchants m
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM categories c
                    WHERE c.merchant_id = m.id AND c.name = '默认分类');

-- 三件在架商品。status = 1 且 published_at 非空，否则列表接口看不见它们
-- （ListProducts 的谓词是 `deleted_at IS NULL AND status = 1`）。
INSERT INTO products (merchant_id, category_id, title, subtitle, min_price_cents,
                      max_price_cents, total_stock, sales_count, status, published_at)
SELECT c.merchant_id, c.id, v.title, v.subtitle, v.min_cents, v.max_cents, 100, 0, 1, now()
  FROM merchants m
  JOIN categories c ON c.merchant_id = m.id AND c.name = '默认分类'
  CROSS JOIN (VALUES
                ('手冲咖啡壶',   '600ml 玻璃',   12900::bigint, 15900::bigint),
                ('陶瓷马克杯',   '两只装',        4900::bigint,  4900::bigint),
                ('挂耳咖啡 10 包', '中度烘焙',     6900::bigint,  8900::bigint)
             ) AS v(title, subtitle, min_cents, max_cents)
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM products p
                    WHERE p.merchant_id = c.merchant_id AND p.title = v.title);
