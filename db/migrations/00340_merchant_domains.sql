-- 自有域名的登记路径：把 domain 从 shop_settings 搬到一张专门的新表，
-- 让应用进程能写它，而不必给 shop_settings 开任何写权限。
--
-- =============================================================================
-- 为什么这条接口要动表结构
-- =============================================================================
--
-- 契约给 PATCH /admin/merchants/{id} 加了 domain（平台运营登记商家自有域名）。
-- 而库里唯一的自有域名真相躺在 shop_settings.domain 上，keel_app 对它**只有 SELECT**：
--
--   · 00005 把 merchants / shop_settings 的三权收掉，实测的第一条越权就是
--     「租户 1 的上下文里改租户 2 的 domain（域名劫持）」；
--   · 00021 开店只还回 merchants 的 INSERT，文件头一节专门写「为什么**不**连
--     shop_settings 一起给」——它守着的正是 00005 三条里最贵的那一条；
--   · 00059 要改的只是时区与自动确认天数，宁可拆一张新表也没碰它。
--
-- 所以「加个 handler 就能登记域名」是不成立的：那条 UPDATE 会在运行期 42501，
-- 而 TestAppRoleGrantSurface 今天全绿——它只比对权限面，不知道应用代码想干什么。
-- 这条接口要么改表结构，要么不存在。
--
-- =============================================================================
-- 四条路
-- =============================================================================
--
-- ### ① GRANT UPDATE ON shop_settings TO keel_app —— 否决
--
-- 00005 点名要防的那一步，而且一次给全表：同一条连接能改别家的 domain（域名劫持）、
-- 改别家的 extra（那里放着支付回调的验签密钥，改了就能给别家伪造一笔已支付，见
-- repository.ChannelNotifySecret）。shop_settings 是 tenant-root、**没有 RLS**
-- （解析要在 SET LOCAL 之前读它），所以这条 GRANT 后面没有任何第二道防线。
--
-- ### ② 列级 GRANT（只给 domain 的 UPDATE / INSERT）—— 否决
--
-- 挡住了 extra，仍然挡不住「租户 1 的上下文里改租户 2 的 domain」：没有 RLS，
-- 谓词 WHERE merchant_id = ? 是应用层写的，而本仓库的规矩是租户隔离不靠应用层记得
-- （00059 文件头对同一件事的否决，理由原样适用）。
--
-- ### ③ 给 merchant_revisions 加一列 domain —— 否决
--
-- 它最省：那一类的闸门（读放开、INSERT 钉在 platform_scope()）与「当前状态 =
-- 最新一行修订」那份共用 SQL 全部现成，一行新机制都不用写。
--
-- 但它把**唯一性**从数据库手里拿走了。域名是全局物理资源：两家店绑同一个域名
-- 就是真的互相劫持，所以 00001 把 shop_settings.domain 写成全表 UNIQUE，
-- 而 tenancy.json 的 unique_global_ok 专门为它留了一条豁免。日志表里「哪家店当前
-- 用着这个域名」是一个派生值（每家店最新一行），普通唯一索引表达不了它，
-- 于是只能改成「先查一遍没人用、再插」——那条保证就变成「每一个写的人都要记得
-- 抢同一把 advisory lock」。①②④三条否决用的都是同一句判据：要紧的规矩不能只靠
-- 应用代码记得。这里也没有更好的替代，所以放弃这条最省的路。
--
-- 顺带第二个代价：解析器的 byDomain 会从「一次 UNIQUE 索引等值命中」变成
-- 「按商家逐个取最新一行再比」——那是每个未命中缓存的请求都要走一遍的路径。
--
-- ### ④ 一张专管域名的当前态表（本迁移采用）
--
--   · keel_app 在 merchant_domains 上只有 SELECT + INSERT + DELETE，**没有 UPDATE**：
--     换绑 = 同一个事务里先删后插。这张表只有 merchant_id / domain 两列有业务含义，
--     extra（支付密钥）、logo_url、currency 一权都没多。
--   · 写入被 RLS 钉在平台作用域，与 directory-log 同一手法：读侧 USING (true)
--     （解析期要读，理由与 tenant-root 一字不差），INSERT 的 WITH CHECK 与 DELETE 的
--     USING 都是 platform_scope()。两条被拒的样子**不一样**，而两边都拦得住：
--     租户作用域里插一行是 42501（WITH CHECK 不过），删别人那一行是**安静地 0 行**
--     （USING 不过 → 那一行对它不存在）。后者没有报错，所以它必须由
--     TestTenantScopeCannotReviseTheMerchantDirectory 去库里看那一行还在不在，
--     而不是只看有没有出错。
--   · domain 上保留**全表 UNIQUE**：那条约束是这条接口的安全核心，不是实现细节。
--     撞了它就是 23505，由 repository 翻成 409 merchant-domain-taken。
--   · 一家店一行（merchant_id 是主键），**没有这一行就是没有自有域名** ——
--     与「开店不写 shop_settings」那个既有事实同一形状，读侧一律 LEFT JOIN。
--
-- 代价说清楚：这张表是当前态，不是日志，所以**域名换绑的历史不在这里**
-- （changed_by / created_at 只说「现在这个域名是谁在什么时候登记的」）。
-- 名字与状态的修改历史仍然在 merchant_revisions，两处不是一回事，别指望一处补齐另一处。
--
-- ### ⑤ SECURITY DEFINER 函数 —— 否决
--
-- 00021 文件头③与 00024 的否决清单，理由原样适用：函数体在库里是一段能绕过全部
-- RLS 的代码，而没有任何闸门在看它（check_query_tenancy.py 的文件头自己写着挡不住这类写法）。
--
-- =============================================================================
-- 搬，不是抄一份
-- =============================================================================
--
-- shop_settings.domain 在同一个迁移里删掉。留着就是两份真相：接口写的是新表，
-- 而任何一个还读旧列的地方会静默按旧值跑，不报错。00059 为两列做过同样的决定，
-- 那里的话原样适用。已有的值原样搬过来（changed_by 留 NULL：那一行不是任何人
-- 登记的，是这次迁移把域名挪了过来）。
--
-- 闸门两侧都要跟着改，缺一侧就是一条静默的豁免：
--   · 设计文档 §2 的 DDL（TestEveryTableInTheDatabaseIsDocumented + check_tenancy.py）；
--   · db/tenancy.json 新增 platform-current 类、登记 merchant_domains，
--     并把 unique_global_ok 那条豁免从 shop_settings(domain) 改名到 merchant_domains(domain)；
--   · migrate_test.go 逐字钉死三条策略（与 directory-log 同一段代码路径）。

-- +goose Up

CREATE TABLE merchant_domains (
    merchant_id BIGINT      PRIMARY KEY REFERENCES merchants(id),
    domain      TEXT        NOT NULL UNIQUE,
    -- 登记这条的平台上操作员（staff.id）。不挂外键：staff 挂着 RLS，而这一列是审计，
    -- 不参与任何授权判断；理由与 merchant_revisions.changed_by 相同。
    -- NULL = 这一行是本次迁移从 shop_settings 搬过来的，当时没有这个人。
    changed_by  BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 只兜「存进去的必须是已归一化之后的文本」这一条下界：解析器比的是
    -- normalizeHost(Host)，那是小写、无端口、无结尾点的。带大写的登记不报错、不生效。
    -- 这里**不**再抄一遍域名形状正则：写入侧的形状校验在 tenant.ValidDomain，
    -- 与 KEEL_BASE_DOMAIN 共用同一个表达式，两处各写一份迟早会对不上。
    CONSTRAINT chk_merchant_domain_shape
        CHECK (btrim(domain) <> '' AND domain = lower(domain))
);

ALTER TABLE merchant_domains ENABLE ROW LEVEL SECURITY;
ALTER TABLE merchant_domains FORCE ROW LEVEL SECURITY;

-- 三条策略，名字与谓词由 db/tenancy.json 的 platform-current 类钉死
-- （internal/db/migrate_test.go 逐字比对）。
CREATE POLICY platform_current_read ON merchant_domains FOR SELECT USING (true);
CREATE POLICY platform_current_insert ON merchant_domains FOR INSERT WITH CHECK (platform_scope());
CREATE POLICY platform_current_delete ON merchant_domains FOR DELETE USING (platform_scope());

-- 00005 之后新表的默认权限只有 SELECT。换绑 = 删一行再插一行，所以两条都要给；
-- UPDATE 不给：这张表可写的只有 domain 一列，而它等于「插一条新的 + 删一条旧的」，
-- 多给一权换不到任何东西。
GRANT INSERT, DELETE ON merchant_domains TO keel_app;

-- 已有的登记原样搬过来。
INSERT INTO merchant_domains (merchant_id, domain)
SELECT merchant_id, domain FROM shop_settings WHERE domain IS NOT NULL;

ALTER TABLE shop_settings DROP COLUMN domain;

-- +goose Down

ALTER TABLE shop_settings ADD COLUMN domain TEXT UNIQUE;

UPDATE shop_settings s
   SET domain = d.domain
  FROM merchant_domains d
 WHERE d.merchant_id = s.merchant_id;

DROP TABLE merchant_domains;
