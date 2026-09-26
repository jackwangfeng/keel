-- 地址簿的写路径落地（/addresses，契约 User tag）。
--
-- ### 只补一个默认值，这是 00011 欠下的那一笔
--
-- 00011 建 user_addresses 时刻意没给 merchant_id 挂 DEFAULT current_merchant()，
-- 理由原话是「本轮没有任何应用代码往这张表写行……等 /addresses 落地时再补这个
-- 默认值，届时它才有要挡的东西」。现在它落地了。
--
-- 为什么一定要默认值、而不是让 repository 把租户当参数传进 INSERT：
-- 那会让生成出来的 Go 函数签名里多一个 merchant_id 参数 —— 一个调用方可以
-- 传错的参数。默认值取的是本事务 set_config 进来的那个租户（与 user_tokens、
-- orders 同一个做法），调用方手里根本没有那个参数。传错的后果本来会被 RLS 的
-- WITH CHECK 挡住（USING 兼作 WITH CHECK），但「挡住」表现为一次 42501，
-- 而「不存在那个参数」连那一次都不会发生。
--
-- ### 「每用户至多一个默认地址」仍然只有那个部分唯一索引一个执行者
--
-- 本迁移不改它。服务端在切换默认时先清旧、再置新（同一事务），并且事务开头
-- 先锁住这个买家的 users 行 —— 那是为了让两个并发的「设为默认」排队，
-- 而不是让第二个撞上 uk_user_addresses_default 报 500。索引仍是最后一道：
-- 服务端的串行化写错了，数据库照样拒绝第二个默认地址。

-- +goose Up
ALTER TABLE user_addresses ALTER COLUMN merchant_id SET DEFAULT current_merchant();

-- +goose Down
ALTER TABLE user_addresses ALTER COLUMN merchant_id DROP DEFAULT;
