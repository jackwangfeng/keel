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

-- ---------------------------------------------------------------------------
-- SKU 与库存
-- ---------------------------------------------------------------------------
--
-- 下单链路（M2 Task 4/5）要的是 SKU 和库存行，而不是商品本身 —— products 上的
-- total_stock 是给列表页看的汇总，真正被扣减的是 inventories.available_qty。
--
-- 价格刻意和 products 的 min/max 对齐：手冲咖啡壶 12900–15900 就真的有两个
-- 分别是 12900 与 15900 的 SKU。不对齐的话，「min_price_cents 是不是真的等于
-- 最便宜那个 SKU」这类断言在种子数据上永远无法证伪。
INSERT INTO skus (merchant_id, product_id, sku_code, spec_values, price_cents, status)
SELECT p.merchant_id, p.id, v.code, v.spec, v.cents, 1
  FROM products p
  JOIN merchants m ON m.id = p.merchant_id
  CROSS JOIN LATERAL (VALUES
        ('手冲咖啡壶',     'HCP-600',  '{"容量":"600ml"}'::jsonb, 12900::bigint),
        ('手冲咖啡壶',     'HCP-900',  '{"容量":"900ml"}'::jsonb, 15900::bigint),
        ('陶瓷马克杯',     'MUG-2',    '{"装量":"两只"}'::jsonb,   4900::bigint),
        ('挂耳咖啡 10 包', 'DRIP-10',  '{"烘焙":"中度"}'::jsonb,   6900::bigint),
        ('挂耳咖啡 10 包', 'DRIP-20',  '{"烘焙":"深度"}'::jsonb,   8900::bigint)
     ) AS v(title, code, spec, cents)
 WHERE m.code = 'demo'
   AND p.title = v.title
   AND NOT EXISTS (SELECT 1 FROM skus s
                    WHERE s.merchant_id = p.merchant_id AND s.sku_code = v.code);

-- 每个 SKU 一行库存。数量刻意不相等：全都是同一个数的话，「扣的是不是这一个
-- SKU」在断言里看不出来。
INSERT INTO inventories (sku_id, available_qty, warning_qty)
SELECT s.id, v.qty, 5
  FROM skus s
  JOIN merchants m ON m.id = s.merchant_id
  CROSS JOIN LATERAL (VALUES
        ('HCP-600', 20), ('HCP-900', 12), ('MUG-2', 50),
        ('DRIP-10', 30), ('DRIP-20', 18)
     ) AS v(code, qty)
 WHERE m.code = 'demo'
   AND s.sku_code = v.code
   AND NOT EXISTS (SELECT 1 FROM inventories i WHERE i.sku_id = s.id);

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
