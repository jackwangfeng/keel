-- +goose NO TRANSACTION
--
-- 回填 orders.receiver_phone，再建它的索引、删掉那条用不上的表达式索引（00170 的后半）。
--
-- 这一份不进事务（NO TRANSACTION），理由是 CONTRIBUTING.md「迁移怎么写」的两条：
--
--   · **回填不进 DDL 事务**：一条 UPDATE 改全表，就是整张 orders 的行锁攥在一个事务里，
--     外加一次性产生整表的死元组。这里按主键区间每 5000 行一批、每批 COMMIT
--     （DO 块在事务外执行时允许 COMMIT，PostgreSQL 11 起）。
--   · **大表建索引用 CONCURRENTLY**：普通 CREATE INDEX 持 SHARE 锁，建多久就挡多久的写；
--     CONCURRENTLY 不挡写，代价是它不能在事务块里跑 —— 这正是 NO TRANSACTION 的用处。
--
-- 回填时把 session_replication_role 切成 replica：否则每一行都会触发 touch_updated_at，
-- 全部订单的 updated_at 被改成迁移那一刻（idx_orders_refunding 等按 updated_at 排序的地方
-- 顺序全乱）。它只作用于这一个会话，迁移角色是超级用户，设得了；DO 块结束前复原。
--
-- 这里不 SET lock_timeout：CONCURRENTLY 只拿 SHARE UPDATE EXCLUSIVE，不与业务读写冲突，
-- 没有「排在长查询后面挡住所有写」那个问题；而 NO TRANSACTION 下 goose 逐条经 *sql.DB 执行，
-- 会话级 SET 落在哪条连接上并无保证，写了反而误导。
--
-- 全部语句可重跑：中途失败后再跑一次，回填从头按区间补（已填的行 IS DISTINCT FROM 为假，
-- 不会再写），索引 IF NOT EXISTS。唯一要人工处理的是 CONCURRENTLY 失败留下的 INVALID
-- 索引：先 DROP INDEX CONCURRENTLY idx_orders_receiver_phone_col 再重跑。

-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE
    lo    bigint;
    hi    bigint;
    step  constant bigint := 5000;
BEGIN
    SELECT min(id), max(id) INTO lo, hi FROM orders;
    IF lo IS NULL THEN
        RETURN;
    END IF;
    PERFORM set_config('session_replication_role', 'replica', false);
    WHILE lo <= hi LOOP
        UPDATE orders
           SET receiver_phone = receiver_snapshot->>'phone'
         WHERE id >= lo AND id < lo + step
           AND receiver_phone IS DISTINCT FROM (receiver_snapshot->>'phone');
        COMMIT;
        lo := lo + step;
    END LOOP;
    PERFORM set_config('session_replication_role', 'origin', false);
END $$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_receiver_phone_col
    ON orders (merchant_id, receiver_phone);

-- 旧的表达式索引在 RLS 下只被当作 merchant_id 前缀用过（见 00170 文件头），
-- 别的 merchant_id 打头的索引一样做得到；留着它只剩写放大。
DROP INDEX CONCURRENTLY IF EXISTS idx_orders_receiver_phone;


-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_receiver_phone
    ON orders (merchant_id, (receiver_snapshot->>'phone'));
DROP INDEX CONCURRENTLY IF EXISTS idx_orders_receiver_phone_col;
