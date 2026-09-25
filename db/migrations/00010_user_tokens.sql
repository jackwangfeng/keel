-- 买家的刷新令牌（会话）。
--
-- ### 这张表**不在**数据模型设计文档里，这是一处需要解释的偏离
--
-- §9 建的是 users / user_identities / user_addresses，§14 给后台建了 staff_tokens，
-- 而买家侧一张会话表都没有。契约那边却写着：
--
--     POST /auth/logout —— 「服务端吊销当前 refresh_token；access_token 到期自然失效」
--
-- 「吊销」是一个**必须有服务端状态**的动作。没有这张表，logout 只能返回一个
-- 什么也没做的 204 —— 客户端以为退出了，那串 refresh_token 仍然能换出新的
-- access_token，直到三十天后自然过期。共用设备上退出登录是买家会做、
-- 也认为有用的动作，做成一个善意的谎言比没有这个接口更糟。
--
-- 所以两份「唯一真相源」在这里对不上：数据模型少了买家会话表，而契约要求能吊销。
-- 本轮的处理是**按契约实现，并把这条偏离单独放在这一份迁移里**，不掺进 00009：
-- 谁若认为该反过来（改契约、把 logout 降级成客户端丢弃令牌），
-- 回滚这一份迁移就够了，§9 那两张表不受影响。
--
-- 建议的收口动作（不在本任务的范围里，已在报告里列为 defer）：
-- 把这张表补进数据模型 §9，理由就是上面这段。
--
-- ### 为什么 access_token 不进这张表
--
-- §9 已经写过一句相关的话：「access_token 这类短期票据根本不该进数据库」。
-- 这里照做：access_token 是自带签名的无状态令牌，每次请求校验它不查库
-- （见 internal/auth/token.go 里那段对「签名覆盖租户」与「服务端存储」两条路
-- 各自代价的说明）。进库的只有生命周期以月计、且必须可吊销的 refresh_token。
--
-- ### 只存 sha256，不存明文
--
-- 理由同 §14 的 staff_tokens：一份能直接用来登录的凭据，在库里以明文躺着，
-- 等价于把所有人的账号写进备份、写进慢查询日志、写进任何一次 pg_dump。
-- 校验是按 hash 的点查，明文只在签发的那一瞬间存在于响应体里。

-- +goose Up

-- merchant_id 的 DEFAULT current_merchant() 不是省事，是**不给调用方留口子**。
--
-- 插入一行会话时，租户只能来自事务里那句 set_config('app.merchant_id')，
-- 也就是 repository.WithTenant 从请求上下文拿到的那个值。调用方**没有那个参数
-- 可以传错**，也就没有「拿 A 店的上下文往 B 店名下插一行会话」这种写法 ——
-- 它连编译都编不出来，因为生成的 Go 函数签名里根本没有 merchant_id。
--
-- 同时它让 db/queries/users.sql 里那条 INSERT 不必写出 merchant_id 这个词，
-- 于是 scripts/check_query_tenancy.py 那条「应用层不得重复过滤租户」的规矩
-- 不需要为它开一个 ALLOW 豁免 —— 豁免会连带把这张表上**别的**查询里的
-- merchant_id 一起放行，而那正是它要挡的东西。
--
-- RLS 的 WITH CHECK 仍然是第二道：DEFAULT 只在没写这一列时才生效，
-- 真有人写了一个别家的 merchant_id，策略会当场拒绝。
CREATE TABLE user_tokens (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    user_id     BIGINT      NOT NULL,
    token_hash  BYTEA       NOT NULL,   -- sha256(refresh_token 明文)
    expire_at   TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,            -- 非空即已吊销（logout / 轮换后的旧值）
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (user_id, merchant_id) REFERENCES users(id, merchant_id)
);

-- 首列是 merchant_id，和 §14 的 staff_tokens(token_hash) 待遇**不同**，
-- 这不是笔误：那张表的校验是「拿着一串 token 去全库点查」，租户前缀无从谈起
-- （平台级 staff 的 merchant_id 本来就是 NULL）。买家这边相反 —— 租户在校验
-- 之前就已经由 Host 定下来了，把它收进索引首列于是既合规矩三，
-- 又让「A 店的 refresh_token 在 B 店查不到」变成索引层面的事实。
--
-- 不过要说清楚：那个事实**不是**本项目防跨店用令牌的那一道。真正那一道在
-- internal/auth 里，是签名覆盖的租户字段，在任何查库之前就把请求拒掉 ——
-- 理由见 internal/auth/middleware.go：靠「查不到」来拒绝，症状会伪装成
-- 「登录失效」，而万一 id 撞上，拒绝就变成了读到另一个人的数据。
CREATE UNIQUE INDEX uk_user_tokens_hash ON user_tokens(merchant_id, token_hash);

-- 按用户找活着的会话（将来的「登出所有设备」与过期清理都走它）。
CREATE INDEX idx_user_tokens_live ON user_tokens(merchant_id, user_id, expire_at)
    WHERE revoked_at IS NULL;

ALTER TABLE user_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_tokens FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON user_tokens USING (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_user_tokens_updated_at
    BEFORE UPDATE ON user_tokens FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- tenant 类的默认 GRANT 面（db/tenancy.json），TestAppRoleGrantSurface 逐表比对。
-- DELETE 今天没有调用点 —— 吊销是 UPDATE revoked_at，过期行的清理属于将来的
-- 定时任务。理由同 00009 里 users 那段：GRANT 面按类别申明，不按今天的调用面裁剪。
GRANT SELECT, INSERT, UPDATE, DELETE ON user_tokens TO keel_app;

-- +goose Down
REVOKE ALL ON user_tokens FROM keel_app;
DROP POLICY tenant ON user_tokens;
DROP TABLE user_tokens;
