-- 商家的改名与停用 / 启用：追加式的 merchant_revisions，而不是 UPDATE merchants。
--
-- =============================================================================
-- 为什么不给 keel_app 加 UPDATE
-- =============================================================================
--
-- 00005 收窄 merchants 的写权限时实测过三条越权，其中两条正是本迁移要做的事：
--
--     租户 1 的上下文里把租户 2 停用（全站 DoS）→ UPDATE merchants
--     完全没有租户上下文时改 merchants          → UPDATE merchants
--
-- merchants 是 tenant-root：它刻意**没有** RLS（租户解析要在 SET LOCAL 之前读它），
-- 防护全压在 GRANT 面上。给 keel_app 一个 UPDATE，就是让**任何一个**租户上下文里的
-- 任何一条 bug 都能改商家目录——停掉别家、改别家的名字。那条 GRANT 挡不住任何东西，
-- 因为 merchants 上没有第二道防线。
--
-- 00021 开店拿到的是 INSERT，论证的核心是「INSERT 按定义碰不到任何一行已有数据」。
-- 改名与停用恰恰要碰已有数据，所以那条论证在这里**不成立**，不能照抄成 UPDATE。
--
-- =============================================================================
-- 采用的形状：一张只追加的修订表，写入被 RLS 钉在平台作用域上
-- =============================================================================
--
--   · keel_app 在 merchant_revisions 上只有 SELECT + INSERT：改不了历史，删不掉历史，
--     在 merchants 上的 GRANT 面一个字没动（仍是 SELECT + INSERT）。
--   · 当前的名字与状态 = 这家店**最新一行**修订；一行都没有时取 merchants 自己那一列。
--     每一行是完整快照（name + status 都写），不是增量——读的时候只看最后一行，
--     不必把历史折叠一遍。
--   · **写入由 RLS 挡在平台作用域之外**：INSERT 策略的 WITH CHECK 是
--     platform_scope()（00017）。租户作用域里的事务插不进来——这正是 UPDATE 方案
--     给不了的那道第二防线：它让「租户 1 的上下文里停用租户 2」在**数据库层**
--     就是 42501，而不是只靠应用代码不写这条 SQL。
--   · 读侧放开（USING (true)）：租户解析要在确定租户之前读它，理由与 merchants
--     本身是 tenant-root 一字不差。它不泄露任何东西——merchants 本来就人人可读，
--     修订里没有比 merchants 更多的信息（只多一个 staff_id，是平台操作员的 id）。
--
-- 顺带得到一份审计：谁在什么时候把哪家店停了。平台运维被问到「这家店为什么
-- 打不开了」时，这是唯一的答案来源。
--
-- ### 否决过的路
--
--   · **UPDATE merchants + 给 merchants 挂一条只管 UPDATE 的平台作用域策略**：
--     安全性与本方案等价，但它仍然是「给 keel_app 加 UPDATE」，而且把 merchants 从
--     tenant-root（无 RLS）改成了一张挂策略的表——解析层每一次读都要多过一层
--     策略求值，tenant-root 这个类别的定义也跟着破。
--   · **SECURITY DEFINER 函数**：00021 文件头③否决过，理由原样适用——函数体在库里
--     就是一段能绕过全部 RLS 的代码，而没有任何闸门在看它。
--   · **改 merchants.status 的同时保留历史**：两个真相源，读的人得知道该信哪一个。
--
-- ### 代价
--
-- 读「当前状态」的四处（tenant.Resolver 的解析与启动自检、repository.ActiveMerchants、
-- 商家目录的读接口）都要 LEFT JOIN LATERAL 取最新一行，而不是直接读 m.status。
-- 这四处写成同一个片段（tenant.effectiveMerchant），漏改一处的症状是
-- 「停用了但买家还打得开」——TestDisabledMerchantIs404ForBuyersButSwitchable 盯着。

-- +goose Up

CREATE TABLE merchant_revisions (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL REFERENCES merchants(id),
    name        TEXT        NOT NULL CHECK (btrim(name) <> ''),
    -- 只收 1 正常 / 2 停用。3 待审核没有写入路径：审核流程本轮不存在，
    -- 给它一条写入路径只会造出一个谁也推不动的状态。
    status      SMALLINT    NOT NULL CHECK (status IN (1, 2)),
    -- 谁改的。平台级操作员（staff.merchant_id IS NULL）。不挂外键：staff 挂着
    -- RLS，而这一列是审计，不参与任何授权判断；操作员被软删之后这一行也该留着。
    changed_by  BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 「这家店最新一行」是唯一的读法。
CREATE INDEX idx_merchant_revisions_latest ON merchant_revisions (merchant_id, id DESC);

ALTER TABLE merchant_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE merchant_revisions FORCE ROW LEVEL SECURITY;

-- 两条策略，名字与谓词由 db/tenancy.json 的 directory-log 类钉死
-- （internal/db/migrate_test.go 逐字比对）。
CREATE POLICY directory_read ON merchant_revisions FOR SELECT USING (true);
CREATE POLICY platform_write ON merchant_revisions FOR INSERT WITH CHECK (platform_scope());

-- 00005 之后新表的默认权限只有 SELECT；INSERT 在这里显式给。
-- 不给 UPDATE / DELETE：修订是只追加的。
GRANT INSERT ON merchant_revisions TO keel_app;

-- +goose Down
DROP TABLE merchant_revisions;
