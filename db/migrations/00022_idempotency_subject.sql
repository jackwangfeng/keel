-- 幂等键换一个主体列：user_id → (subject_kind, subject_id)（数据模型 §12）。
--
-- =============================================================================
-- 这是一次 schema 决定，而它挡在「5 条后台 POST 接上 Idempotency-Key」前面
-- =============================================================================
--
-- 契约给 /admin/uploads、/admin/products、.../publication、.../skus、
-- /admin/categories 这 5 条都声明了必填的 `Idempotency-Key` 与 422
-- IdempotencyKeyReused，而服务端一直没有实现。不是忘了，是缺这一步：
--
--     idempotency_keys 的主键是 (scope, user_id, idem_key)，
--     而 user_id 那一列在后台这条路上要放的是 staff_id。
--
-- 「把 staff_id 当成 user_id 用」正是 internal/auth/staff_middleware.go 与
-- 数据模型 §14 反复点名的那件事：staff.id 与 users.id 来自同一种自增序列，
-- 撞上不是小概率，是日常。
--
-- =============================================================================
-- 两条路，选了第一条
-- =============================================================================
--
-- ### ① 给这张表换一个主体列（本迁移采用）
--
--     user_id BIGINT  →  subject_kind SMALLINT + subject_id BIGINT
--     PK (scope, user_id, idem_key) → PK (scope, subject_kind, subject_id, idem_key)
--
-- 一张表、一份实现、一个过期清理任务、一条 RLS 策略。买家与后台共用的是
-- 「抢占 → 三态处置 → 存档回放」这套机制，而那套机制里没有任何一处关心主体
-- 是谁 —— 它只要一个能唯一标识调用者的东西。
--
-- **subject_kind 不是冗余，尽管它今天可以由 scope 推出来。** 今天的 scope
-- 串确实是按接口起的（orders.create / admin.products.create），两个身份域
-- 的 scope 永远不会相等，所以光换个列名也够用。问题在于那是一条**没有执行者
-- 的约定**：它只活在「起 scope 名字的人知道这回事」里，而一旦有人复用了一个
-- scope 串（比如把某条接口拆成买家版与后台版却沿用同一个 scope），
-- staff_id = 7 与 user_id = 7 就会撞在同一行上 —— 后果是其中一个人拿到另一个
-- 人的存档响应，而它不报错。加一列之后，那件事按构造不可能发生。
-- 代价是 2 个字节，以及这份迁移。
--
-- 它顺带让这一行**自己说得清自己是谁的**：过期清理、审计、排查都不必先去
-- 查一张「scope 前缀属于哪个身份域」的对照表。
--
-- ### ② 另起一张 staff_idempotency_keys
--
-- **输在「两份实现」上，而这个仓库刚为同一件事付过账。** 两张形状相同的表
-- 意味着两条抢占 SQL、两个三态处置、两个 24 小时清理任务、两条 RLS 策略、
-- 两份 GRANT 面。它们分叉的那天不会有任何东西变红 —— repository/tenant.go
-- 的文件头为 set_config 写过一模一样的话（「两份实现意味着将来有人只改对
-- 其中一份」），db/tenancy.json 的 _readme 为两份豁免清单写过一模一样的话
-- （「两边各自成立、接起来不成立」）。
--
-- 它唯一的好处是不动买家那条路。而那个好处在这里很小：改动是机械的
-- （四条查询各多一个参数，两处调用点各多一个 repository.BuyerSubject(...)），
-- 而且**编译器会把每一处都点出来** —— 换的是函数签名，不是某个字段的含义。
-- 拿一次编译期就能穷尽的改动，去换一份会永久存在的分叉风险，不划算。
--
-- 另外它还要一张新表进 db/tenancy.json、进数据模型文档、进那一串目录驱动的
-- 闸门，而那张表与既有的那张逐列相同 —— TestEveryTableInTheDatabaseIsDocumented
-- 会通过，但读文档的人要在两节几乎一样的 DDL 之间分辨差别。
--
-- =============================================================================
-- 落地上的三处细节
-- =============================================================================
--
-- 1. **subject_kind 先带 DEFAULT 1 再把默认值摘掉。** 带默认值是为了给存量行
--    回填（这张表此刻的每一行都是买家的）；摘掉是因为一个「不传就当买家」的
--    默认值，正是这份迁移要消灭的那类东西 —— 后台那条路忘了传的症状会是
--    「它安静地跑在买家的键空间里」，而那就是本文件开头那个 bug 的复现。
--    摘掉之后 sqlc 生成的函数签名里 subject_kind 是必填参数。
--
-- 2. **CHECK 约束钉住取值**。1 买家 / 2 后台，与 Go 侧
--    repository.IdempotencySubjectUser / ...Staff 逐值一致。没有它的话，
--    一个拼错的 3 会安静地开出第三个键空间。
--
-- 3. **idx_idem_expire 不动**：它是 (merchant_id, expire_at)，清理任务按租户
--    入队走 RLS，需要 merchant_id 打头（§12）。主体列换了不影响它。
--
-- =============================================================================
-- db/tenancy.json 要跟着改两处（已改）
-- =============================================================================
--
--   · unique_global_ok 的键从 idempotency_keys(scope,user_id,idem_key)
--     换成 idempotency_keys(scope,subject_kind,subject_id,idem_key)。
--     理由不变：主体 id 已蕴含租户，scope 打头是为了让抢占插入是一次点查。
--   · fk_missing_ok 里的 idempotency_keys.user_id **删掉**。那条豁免的判据是
--     「列名形如 <x>_id、能解析到一张同名父表、却没有外键」，而 subject_id
--     解析不到任何一张 subjects 表 —— 留着它，两侧闸门都会报「该清理了」。
--     而且它记的那件事现在由列名自己说了：这一列指向的是哪张表取决于
--     subject_kind，所以它按定义就挂不了外键。

-- +goose Up

ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;

ALTER TABLE idempotency_keys RENAME COLUMN user_id TO subject_id;

-- 先带默认值回填存量（这张表此刻的每一行都是买家的），再把默认值摘掉。
ALTER TABLE idempotency_keys ADD COLUMN subject_kind SMALLINT NOT NULL DEFAULT 1;
ALTER TABLE idempotency_keys ALTER COLUMN subject_kind DROP DEFAULT;

ALTER TABLE idempotency_keys
    ADD CONSTRAINT chk_idem_subject CHECK (subject_kind IN (1, 2));

ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey
    PRIMARY KEY (scope, subject_kind, subject_id, idem_key);

-- +goose Down

ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys DROP CONSTRAINT chk_idem_subject;

-- 回滚会丢掉「这一行是谁的」这个信息。后台那些行回滚之后会变成 user_id，
-- 也就是本文件开头那个 bug —— 所以先把它们删掉，而不是留在原地。
-- 它们是随 expire_at 过期即删的基础设施行，删掉的代价是「那几把钥匙的重放
-- 拿不到存档」，而回滚本来就意味着后台那条路的代码也回去了。
DELETE FROM idempotency_keys WHERE subject_kind = 2;

ALTER TABLE idempotency_keys DROP COLUMN subject_kind;
ALTER TABLE idempotency_keys RENAME COLUMN subject_id TO user_id;
ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (scope, user_id, idem_key);
