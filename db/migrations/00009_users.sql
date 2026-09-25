-- 买家身份：users / user_identities，以及 orders.user_id 那条**刻意留红**的外键。
--
-- DDL 照抄数据模型设计 §9，那里是唯一真相源。§9 的那句话也照做了：
-- 「章节顺序不是迁移顺序 —— users 必须先于 orders、user_coupons、carts 建立」。
-- 这里晚于 orders 是因为 orders 先落地了（M2 Task 1），所以 users 建完的第一件事
-- 就是把那条欠下的外键补上，见文件末尾。
--
-- ### 为什么 db/tenancy.json 不需要为这两张表加条目
--
-- 它们是最普通的业务表：自带 merchant_id、策略是直接的列比较、唯一约束全部
-- 收在租户内。那正是清单里 tenant 类的定义，而 tenant 是**默认类别** ——
-- 「不在 tables 里登记的表一律按这一类查」。往清单里加一条「其实什么都没豁免」
-- 的条目，只会让那份清单从「每一条都需要解释」退化成一张表名目录，
-- 而它现在的价值恰恰在于短。
--
-- 两侧闸门都会真的查到它们：Go 侧从系统目录枚举（migrate_test.go 的五条），
-- Python 侧从设计文档枚举（check_tenancy.py）。漏挂策略、漏挂触发器、
-- GRANT 面多给少给，都会红在这两处。
--
-- ### 一条数据库兜不住的约束（§9 明写）
--
-- 「每个账号至少要有一种可登录的凭据」跨了 users 与 user_identities 两张表，
-- CHECK 做不到、触发器代价太大，只能由注册流程保证。本轮**没有注册流程**
-- （密码登录不注册，验证码登录还没有短信服务），所以这条今天还没有落点。

-- +goose Up

-- ---------------------------------------------------------------------------
-- users（数据模型 §9）
-- ---------------------------------------------------------------------------
--
-- password_hash 可空是刻意的：国内电商的主登录态是手机验证码和微信授权，
-- 密码是可选项。硬性 NOT NULL 只会逼出一堆存着随机串的假密码。
-- 存进这一列的是 PHC 字符串（$argon2id$v=19$m=...,t=...,p=...$salt$hash），
-- 参数随每一行一起存 —— 理由见 internal/auth/password.go。
CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id   BIGINT      NOT NULL REFERENCES merchants(id),
    phone         TEXT,                             -- 国内电商主登录态
    email         TEXT,
    password_hash TEXT,                             -- 可空：允许仅第三方登录
    nickname      TEXT        NOT NULL DEFAULT '',
    avatar_url    TEXT,
    gender        SMALLINT    NOT NULL DEFAULT 0,   -- 0未知 1男 2女
    status        SMALLINT    NOT NULL DEFAULT 1,   -- 1正常 2封禁 3已注销
    last_login_at TIMESTAMPTZ,
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 供 orders / user_coupons / user_identities / user_addresses / carts / uploads
    -- 做复合外键：买家属于某个商家，订单与地址不得挂到别家的买家身上
    UNIQUE (id, merchant_id)
);

-- 唯一索引带 deleted_at IS NULL（§9）：号码会被运营商回收再放号，注销用户
-- 占着一个手机号，新主人就永远注册不了。带上软删条件后号码可以复用，
-- 而历史订单仍然挂在旧的 user_id 上 —— 交易数据的归属永不改写。
--
-- 首列是 merchant_id，这不只是规矩三：**同一个手机号在 A 店和 B 店是两行
-- users**（§8/§9 的「买家属于商家」）。收在租户内是这条语义的字面落地，
-- 而不是一个索引选择性的取舍。db/seed/dev.sql 里 shop-a 与 shop-b 的买家
-- 就用同一个号码，那份种子是这条断言的靶子。
CREATE UNIQUE INDEX uk_users_phone ON users(merchant_id, phone)
    WHERE phone IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX uk_users_email ON users(merchant_id, email)
    WHERE email IS NOT NULL AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- user_identities（数据模型 §9）
--
-- 微信 openid 按 appid 隔离：同一个人在小程序、公众号、App 里拿到三个不同的
-- openid，只有 unionid 能把他们认成一个人。一个用户对多条身份是本来就存在的
-- 事实，拿列去表达迟早要加 openid_mp / openid_app / openid_web。
--
-- 本轮**没有任何代码读写这张表**：第三方登录（/auth/wechat/login）不在 Task 1.5
-- 的范围里。它现在就建出来，是因为 users 的外键落点、清单闸门与 §9 的 DDL
-- 是一整块；分两次建的话，第二次那份迁移要在一张已经有数据的表上补索引。
-- ---------------------------------------------------------------------------
CREATE TABLE user_identities (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL REFERENCES merchants(id),
    user_id     BIGINT      NOT NULL,
    provider    SMALLINT    NOT NULL,   -- 1微信小程序 2微信公众号 3微信开放平台 4支付宝 5Apple
    external_id TEXT        NOT NULL,   -- openid：同一个人在不同 appid 下不同
    union_id    TEXT,                   -- 微信 unionid：跨应用识别同一个人
    credential  JSONB       NOT NULL DEFAULT '{}',  -- session_key 等，密文存放
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (user_id, merchant_id) REFERENCES users(id, merchant_id)
);
CREATE UNIQUE INDEX uk_user_identities
    ON user_identities(merchant_id, provider, external_id);
CREATE INDEX idx_user_identities_user ON user_identities(merchant_id, user_id);
CREATE INDEX idx_user_identities_union
    ON user_identities(merchant_id, union_id) WHERE union_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 行级安全。ENABLE 之外必须再加 FORCE（00002 与数据模型 §2「坑一」）。
-- 策略一律叫 tenant（db/tenancy.json 的 policy_name）。
-- ---------------------------------------------------------------------------
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON users USING (merchant_id = current_merchant());

ALTER TABLE user_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_identities FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON user_identities USING (merchant_id = current_merchant());

-- ---------------------------------------------------------------------------
-- updated_at 触发器。00007 那个 DO 循环只在它自己那一次迁移里跑过，
-- 之后新建的表要自己挂 —— TestUpdatedAtIsMaintainedByTrigger 会盯着。
-- ---------------------------------------------------------------------------
CREATE OR REPLACE TRIGGER touch_users_updated_at
    BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE OR REPLACE TRIGGER touch_user_identities_updated_at
    BEFORE UPDATE ON user_identities FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ---------------------------------------------------------------------------
-- GRANT 面。00005 之后新表的默认权限只有 SELECT，写权限由建表的迁移显式申明。
-- tenant 类的默认是四权（db/tenancy.json），TestAppRoleGrantSurface 逐表比对。
--
-- users 的 DELETE 说一句：买家注销走的是 deleted_at 软删（§9 的手机号复用
-- 依赖这一列），所以 DELETE 今天没有调用点。它仍然给，是因为 GRANT 面按
-- **类别**申明而不是按今天的调用面裁剪 —— 逐表裁剪会让清单的类别失去意义，
-- 而 barrier 那种按实际调用面收窄的表在清单里是显式登记过的例外。
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON users, user_identities TO keel_app;

-- ---------------------------------------------------------------------------
-- 补上 orders.user_id 的复合外键 —— 00006 刻意留下的那条红线。
--
-- 00006 的文件头 ① 写明：users 属于 §9，当时没有落点，所以这条外键建不出来，
-- 而且**刻意没有**登记进 db/tenancy.json 的 fk_missing_ok —— 登记等于把提醒
-- 关掉。于是 users 一被建出来，TestForeignKeysAreNotSilentlyMissing 当场红
-- （它按「列名 <x>_id + 库里存在同名父表」解析），这份迁移不补上它就过不去。
--
-- 为什么必须是复合的而不是 REFERENCES users(id)：单列外键只保证「这个 user
-- 存在」，不保证「这个 user 是本店的」。A 店的订单挂上 B 店的买家之后，
-- 数据库不拒、RLS 也不拒（orders 的策略只看 orders.merchant_id），
-- 而那一单会同时出现在 A 的后台和 B 的「我的订单」里 —— 手机号、收货地址、
-- 金额全都跟着泄露。复合外键让这一行根本写不进去。
--
-- 约束名显式写出来：默认名是 orders_user_id_merchant_id_fkey，
-- 而 42501/23503 报错里出现的就是这个名字，它该指向这段话。
ALTER TABLE orders
    ADD CONSTRAINT fk_orders_user
    FOREIGN KEY (user_id, merchant_id) REFERENCES users(id, merchant_id);

-- +goose Down
ALTER TABLE orders DROP CONSTRAINT fk_orders_user;

REVOKE ALL ON users, user_identities FROM keel_app;

DROP POLICY tenant ON user_identities;
DROP POLICY tenant ON users;

DROP TABLE user_identities;
DROP TABLE users;
