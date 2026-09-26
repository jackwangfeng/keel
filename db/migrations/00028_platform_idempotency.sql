-- 幂等键有了「平台级」的落点：merchant_id 可空，策略换成 staff 那一种（数据模型 §12）。
--
-- =============================================================================
-- 它挡在哪两条接口前面
-- =============================================================================
--
-- POST /admin/merchants（开店）与 POST /admin/staff（加员工）。契约给两条都声明了
-- 必填的 Idempotency-Key，而 0.1.0 发布时它们没有实现（CHANGELOG 0.1.0 Known gaps）。
-- 缺的不是代码，是一个**落点**：
--
--     idempotency_keys.merchant_id 是 NOT NULL DEFAULT current_merchant()，
--     而这两条接口跑在平台作用域里（app.platform_scope = 'on'，app.merchant_id 没设）
--     —— current_merchant() 在那里是 RAISE，抢占插入那一句当场报错。
--
-- 加员工那一条更别扭：它的作用域取决于调用者（平台管理员加平台操作员 / 商家管理员
-- 加本店员工），只接上商家那一半会让同一条接口有两种语义。所以两半一起接。
--
-- =============================================================================
-- 两条路，选了第一条
-- =============================================================================
--
-- ### ① merchant_id 可空 + 策略换成 staff 那一条（本迁移采用）
--
--     merchant_id  BIGINT NOT NULL DEFAULT current_merchant()
--              →   BIGINT          DEFAULT staff_scope_merchant()
--     POLICY tenant USING (merchant_id = current_merchant())
--              →   USING / WITH CHECK (merchant_id IS NOT DISTINCT FROM staff_scope_merchant())
--
-- 这正是 00017 给 staff 用的那个形状，而那个形状的安全性论证已经做过一次、
-- 有行为测试钉着（internal/db/staff_rls_test.go 那张四象限表）：
--
--   · **租户作用域里它逐行等价于旧策略。** enterTenantScope 显式把
--     app.platform_scope 关掉，staff_scope_merchant() 于是就是 current_merchant()
--     —— 买家下单、后台那 5 条 POST 走的每一条语句，判定结果一个都不变；
--     忘了设租户照旧是 RAISE（42501），不会退化成「看见 NULL 那一抽屉」。
--   · **商家员工碰不到平台记录。** 租户作用域里 `NULL IS NOT DISTINCT FROM 7`
--     为假：读不到 merchant_id 为 NULL 的行，WITH CHECK 也不让他插出一行 NULL ——
--     不然他能替某个平台管理员预埋一份存档，对方下一次用那把钥匙重试时
--     拿到的就是一份伪造的「重放」。
--   · **平台会话碰不到任何一家店的记录。** 平台作用域里 staff_scope_merchant()
--     是 NULL，`7 IS NOT DISTINCT FROM NULL` 为假：读不到、也插不出带
--     merchant_id 的行。「平台管理员误用某家店的存档」按构造不可能。
--
-- CHANGELOG 里说的「提权风险」指的是**另一种**写法：
-- `USING (merchant_id = current_merchant() OR merchant_id IS NULL)`。它读侧看起来
-- 差不多，而 USING 同时充当 WITH CHECK，任何租户上下文都能写出 NULL 行 ——
-- staff 表上那就是「商家管理员给自己造一个平台管理员」，这里是「商家员工往平台
-- 键空间里写存档」。本迁移不用那种写法；db/tenancy.json 的 column-scope 类逐字
-- 比对谓词（policy_qual），改成 OR 写法当场红。
--
-- 额外一道：`CHECK (merchant_id IS NOT NULL OR subject_kind = 2)`。平台作用域里
-- 只有后台操作员，买家按定义属于某一家店 —— 一行 merchant_id 为 NULL 的买家
-- 记录只可能是 bug，让它在数据库上当场 23514。
--
-- ### ② 另起一张 platform_idempotency_keys（无 merchant_id，策略 USING (platform_scope())）
--
-- 物理隔离看起来更「干净」，但输在与 00023 一模一样的地方：**两份实现**。
-- 两条抢占 SQL、两条读、两条存档、两套 Go 方法、两份 GRANT 面、将来两个过期清理
-- 任务 —— 分叉的那天没有任何东西变红。它还要在 db/tenancy.json 里新开一个类别、
-- 在 Go 与 Python 两侧的闸门里各加一个分支，而那个类别的形状（「只在平台作用域
-- 可见」）恰好就是 column-scope 类在 NULL 那一抽屉上已经做到的事。
--
-- 它唯一的好处是不动买家那条热路径上的策略。而上面第一条已经说明：租户作用域里
-- 新旧策略逐行等价，改动的只是谓词的写法；代价是每行多一次 platform_scope() 的
-- current_setting 读取（STABLE 函数，同一条语句内可以折叠），在一张按主键点查的
-- 表上可以忽略。
--
-- =============================================================================
-- 落地上的细节
-- =============================================================================
--
-- 1. **默认值换成 staff_scope_merchant()**，与 staff 表同一个函数。查询里依旧
--    一个 merchant_id 都没有（scripts/check_query_tenancy.py），生成的 Go 函数签名
--    也不变 —— 作用域只能来自事务里那句 set_config，不能由参数传错。
-- 2. **idx_idem_expire 不动**。平台那几行 merchant_id 为 NULL，B-tree 照样收录。
--    将来的过期清理任务按租户入队、走 RLS，看不见平台那一抽屉 —— 那一抽屉要自己
--    在平台作用域里清一次。今天两边都没有清理任务（00012 的文件头：DELETE 权
--    按类别给，今天没有调用点），所以这不是一次退化，是登记一笔将来要一起做的事。
-- 3. **主键不动**：(scope, subject_kind, subject_id, idem_key)。平台员工与商家员工
--    都是 subject_kind = 2、id 都来自 staff 那一张表，所以两个作用域的员工不会
--    撞在同一个主体上；而跨作用域的读写由上面的策略挡住，不靠主键。
-- 4. db/tenancy.json：idempotency_keys 从默认的 tenant 类登记成 tenant-nullable，
--    带 policy_qual —— 与 staff 同一条逐字断言。

-- +goose Up

ALTER TABLE idempotency_keys ALTER COLUMN merchant_id DROP NOT NULL;
ALTER TABLE idempotency_keys ALTER COLUMN merchant_id SET DEFAULT staff_scope_merchant();

ALTER TABLE idempotency_keys
    ADD CONSTRAINT chk_idem_platform_is_staff
    CHECK (merchant_id IS NOT NULL OR subject_kind = 2);

DROP POLICY tenant ON idempotency_keys;
CREATE POLICY tenant ON idempotency_keys
  USING      (merchant_id IS NOT DISTINCT FROM staff_scope_merchant())
  WITH CHECK (merchant_id IS NOT DISTINCT FROM staff_scope_merchant());

-- +goose Down

-- 平台那几行回滚之后没有落点（NOT NULL 会拒绝它们），先删掉。它们是随 expire_at
-- 过期即删的基础设施行，代价是「那几把钥匙的重放拿不到存档」，而回滚本来就意味着
-- 平台那两条接口的幂等代码也回去了。与 00023 的 Down 同一条理由。
DELETE FROM idempotency_keys WHERE merchant_id IS NULL;

DROP POLICY tenant ON idempotency_keys;
CREATE POLICY tenant ON idempotency_keys USING (merchant_id = current_merchant());

ALTER TABLE idempotency_keys DROP CONSTRAINT chk_idem_platform_is_staff;
ALTER TABLE idempotency_keys ALTER COLUMN merchant_id SET DEFAULT current_merchant();
ALTER TABLE idempotency_keys ALTER COLUMN merchant_id SET NOT NULL;
