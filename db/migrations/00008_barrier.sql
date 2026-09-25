-- 子事务屏障表。
--
-- **它为什么在这里，而不是「随 dtmrs 一起建出来」。**
--
-- 仓库里此前三处都写着 barrier 是 dtmrs 的 migrate() 产物：db/tenancy.json 的
-- 条目、架构 §6、以及 00006 那句「barrier 不在这里建……归 M2 Task 3」。
-- 那句话是错的，M2 Task 3 接上 dtmrs 之后实测出来的事实是：
--
--   · dtmrs 的 migrate() 建的是 trans_global / trans_branch_op / auth_token
--     三张——**协调器自己的状态**。把 C ABI 的 dtmrs_start 指向一个空库跑一遍，
--     长出来的就是这三张，没有 barrier。
--   · barrier 由 dtmrs-barrier 那个 crate 的 BranchBarrier::migrate 建，而
--     dtmrs-ffi 根本不依赖它（Cargo.toml 的依赖表里没有，导出的 22 个符号里
--     也没有）。经 cgo 嵌入的宿主拿不到那个函数。
--   · 这不是 dtmrs 的缺口，是结构上的必然：屏障的 decide() 接收**调用方的事务**，
--     屏障记录必须与业务变更同事务提交。任何非 Rust 宿主都得自己实现那三十行，
--     连同它的表。
--
-- 所以没有任何东西会替我们建它。它归本项目的迁移。
--
-- 表结构照抄 dtmrs-barrier 的 DDL（v0.11.0，crates/dtmrs-barrier/src/lib.rs），
-- **一列都不要加**：Go 侧的屏障实现与 Rust 侧逐行对应，两边对同一张表操作。
-- 比 DTM 的表少一个自增 id 列，那是上游刻意去掉的（三种方言的自增写法不通用，
-- 而算法只依赖那个唯一约束）。
--
-- 类别是 cross-tenant-infra（db/tenancy.json）：主键是
-- (gid, branch_id, op, barrier_id)——全局事务维度，没有 merchant_id 可挂，
-- 也不该有租户策略。租户隔离由 gid 承担：gid 的形状是
-- order-{merchant_id}-{order_no}（架构 §5），屏障行天然按租户分开，
-- 而分支写业务表时仍然走 RLS。

-- +goose Up
CREATE TABLE barrier (
    trans_type  TEXT   NOT NULL,
    gid         TEXT   NOT NULL,
    branch_id   TEXT   NOT NULL,
    op          TEXT   NOT NULL,
    barrier_id  TEXT   NOT NULL,
    reason      TEXT   NOT NULL,
    create_time BIGINT NOT NULL,
    PRIMARY KEY (gid, branch_id, op, barrier_id)
);

COMMENT ON TABLE barrier IS
    '子事务屏障（数据模型 §6）。形状照抄 dtmrs-barrier v0.11.0，别加列。';

-- GRANT 面：**只有 INSERT**。
--
-- 00005 之后新表的默认权限是 SELECT，写权限由建表的这份迁移显式申明。
-- 这里反过来，连默认那一项 SELECT 也要收掉——按实际调用面，keel_app 在这张表上
-- 只发一条语句：
--
--     INSERT INTO barrier (...) VALUES (...) ON CONFLICT DO NOTHING
--
-- 整个屏障算法就靠它的 rows_affected 判「空回滚」与「重复请求」；不读、不改、不删。
--
-- 实测两条（PostgreSQL 16，keel_app 只有 INSERT）：
--   · 不带冲突目标的 `ON CONFLICT DO NOTHING` 跑得通——首次 `INSERT 0 1`，
--     重复 `INSERT 0 0`，正是算法要的两个值；
--   · 写成 `ON CONFLICT (gid, branch_id, op, barrier_id) DO NOTHING` 会报
--     `permission denied for table barrier`——**带冲突目标要额外的 SELECT 权**。
--
-- 所以这条 GRANT 面同时是一道形状约束：屏障的 SQL 不许写冲突目标。
-- 哪天有人「顺手把冲突列写清楚」，红的会是运行期的 42501，而这段注释是答案。
--
-- 少给的代价也说清楚：将来若要加一个清理过期屏障行的任务（这张表只增不减，
-- 见下），那个任务需要 DELETE——而它该走管理员角色或一份显式加 GRANT 的迁移，
-- 不该靠这里预先多给一项。
REVOKE ALL ON barrier FROM keel_app;
GRANT INSERT ON barrier TO keel_app;

-- +goose Down
REVOKE ALL ON barrier FROM keel_app;
DROP TABLE barrier;
