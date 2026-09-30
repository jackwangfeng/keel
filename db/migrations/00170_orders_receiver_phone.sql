-- 订单冗余一列 receiver_phone（收货人手机号），给后台「按手机号找单」走索引用（2026-09-30 架构审查）。
--
-- ### 为什么 00035 那条表达式索引一直没用上
--
-- idx_orders_receiver_phone 建在 (merchant_id, (receiver_snapshot->>'phone')) 上，查询也照着写
-- 了 o.receiver_snapshot->>'phone' = $1。但 orders 挂着 RLS，而 jsonb 的 ->> 算子
-- （jsonb_object_field_text）**不是 leakproof**：规划器不许把它排到租户谓词前面去算，
-- 否则一个会报错的算子就能拿别家的行来试探。于是它只能当 Filter —— 计划是先用那条索引的
-- merchant_id 前缀取出全店订单，再逐行解 JSON 比手机号。实测每店 4 万单：
--   Index Cond: (merchant_id = current_merchant())
--   Filter: ((receiver_snapshot ->> 'phone') = '138…')   Rows Removed by Filter: 39998
-- 与「没有这条索引」一个量级，而且随订单量线性变慢。
--
-- text 的 = （texteq）是 leakproof 的。把手机号放进一列普通 text，谓词就能进 Index Cond。
--
-- ### 为什么是「普通列 + 触发器」而不是 GENERATED ALWAYS AS (...) STORED
--
-- 生成列最省心，但 ADD COLUMN ... GENERATED STORED 要**重写整张表**，全程持
-- ACCESS EXCLUSIVE 锁 —— orders 是最大的表之一，那是一次停机。普通可空列的 ADD COLUMN
-- 只改目录，毫秒级；存量由 00171 分批回填（不在 DDL 事务里），增量由这里的触发器维护。
-- 语义与生成列相同：列值永远等于 receiver_snapshot->>'phone'，应用层谁也不写它。
--
-- 触发器挂 INSERT 与 UPDATE OF receiver_snapshot：快照在下单时写一次，今天没有改收货地址的
-- 接口；将来有了，UPDATE 那一半保证它跟着走。

-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE orders ADD COLUMN receiver_phone TEXT;

COMMENT ON COLUMN orders.receiver_phone IS
    '收货人手机号，= receiver_snapshot->>''phone''，由触发器维护（00170），应用不写。'
    '存在的理由：->> 非 leakproof，RLS 下走不了表达式索引；text 的 = 是 leakproof。';

-- +goose StatementBegin
CREATE FUNCTION orders_sync_receiver_phone() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.receiver_phone := NEW.receiver_snapshot->>'phone';
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER sync_orders_receiver_phone
    BEFORE INSERT OR UPDATE OF receiver_snapshot ON orders
    FOR EACH ROW EXECUTE FUNCTION orders_sync_receiver_phone();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER sync_orders_receiver_phone ON orders;
DROP FUNCTION orders_sync_receiver_phone();
ALTER TABLE orders DROP COLUMN receiver_phone;
