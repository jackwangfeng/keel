-- 收货地址（数据模型 §9）。
--
-- ### 为什么它是 Task 4/5 的隐藏前置
--
-- 契约的 `OrderCreateRequest` 里 `address_id` 是 **required**，而这张表一直
-- 不存在。和当初的 `users` 是同一类情况：不建就没法开工 —— 下单第一步要做的
-- 「地址有效性校验」（架构 §5 第 ④ 步）没有可查的东西，`orders.receiver_snapshot`
-- 也没有可拷贝的源。
--
-- ### 为什么 orders 里没有 address_id
--
-- §9 写明了：地址与订单的关系是**快照，不是外键**。下单时把这一行拍扁成
-- JSONB 写进 `orders.receiver_snapshot`，之后用户改地址、删地址都不影响历史
-- 订单。所以本迁移**不**给 orders 加任何指向本表的列 —— 契约里的 address_id
-- 只活在请求体里，落库的是快照。
--
-- ### db/tenancy.json 不需要为它加条目
--
-- 它是最普通的业务表：自带 merchant_id、策略是直接的列比较。那正是 tenant 类
-- 的定义，而 tenant 是**默认类别**。加一条「其实什么都没豁免」的条目只会让那份
-- 清单从「每一条都需要解释」退化成一张表名目录（理由同 00009 的文件头）。
--
-- 唯一要登记的那一条早就在册：`user_addresses(user_id)` 已经写在
-- unique_global_ok 里（那份清单是**前瞻**的，表还没建就先登记好），
-- 理由是「首列 user_id 已蕴含租户」。

-- +goose Up

-- merchant_id 这里**不**给 DEFAULT current_merchant()，和 user_tokens（00010）
-- 待遇不同，这不是笔误：本轮没有任何应用代码往这张表写行（地址簿的增删改
-- `/addresses` 不在 Task 4/5 的范围里），写它的只有种子，而种子走管理员角色、
-- 本来就没有租户上下文。等 `/addresses` 落地时再补这个默认值，届时它才有
-- 要挡的东西。
CREATE TABLE user_addresses (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id   BIGINT      NOT NULL REFERENCES merchants(id),
    user_id       BIGINT      NOT NULL,
    receiver_name TEXT        NOT NULL,
    phone         TEXT        NOT NULL,
    province      TEXT        NOT NULL,
    city          TEXT        NOT NULL,
    district      TEXT        NOT NULL,
    street        TEXT        NOT NULL DEFAULT '',
    detail        TEXT        NOT NULL,          -- 门牌号等
    region_code   TEXT,                          -- 行政区划码，用于运费与配送范围计算
    postal_code   TEXT,
    tag           SMALLINT    NOT NULL DEFAULT 0, -- 0无 1家 2公司 3学校
    is_default    BOOLEAN     NOT NULL DEFAULT FALSE,
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (user_id, merchant_id) REFERENCES users(id, merchant_id)
);

-- 「每个用户至多一个默认地址」这条规则**就是**这个部分唯一索引，不是代码里
-- 「设置默认前先把其他的置 false」再祈祷没人并发点两下（§9）。
-- 注意是「至多一个」不是「恰好一个」：新用户零个地址是合法状态。
CREATE UNIQUE INDEX uk_user_addresses_default ON user_addresses(user_id)
    WHERE is_default AND deleted_at IS NULL;
CREATE INDEX idx_user_addresses_user ON user_addresses(merchant_id, user_id)
    WHERE deleted_at IS NULL;

-- 行级安全。ENABLE 之外必须再加 FORCE（00002 与数据模型 §2「坑一」）。
-- 策略一律叫 tenant（db/tenancy.json 的 policy_name）。
ALTER TABLE user_addresses ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_addresses FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON user_addresses USING (merchant_id = current_merchant());

-- 00007 那个 DO 循环只在它自己那一次迁移里跑过，之后新建的表要自己挂 ——
-- TestUpdatedAtIsMaintainedByTrigger 会盯着。
CREATE OR REPLACE TRIGGER touch_user_addresses_updated_at
    BEFORE UPDATE ON user_addresses FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- tenant 类的默认 GRANT 面（db/tenancy.json），TestAppRoleGrantSurface 逐表比对。
-- INSERT/UPDATE/DELETE 今天没有调用点（地址簿接口还没做），仍然给：
-- GRANT 面按**类别**申明，不按今天的调用面裁剪（理由同 00009 里 users 那段）。
GRANT SELECT, INSERT, UPDATE, DELETE ON user_addresses TO keel_app;

-- +goose Down
REVOKE ALL ON user_addresses FROM keel_app;
DROP POLICY tenant ON user_addresses;
DROP TABLE user_addresses;
