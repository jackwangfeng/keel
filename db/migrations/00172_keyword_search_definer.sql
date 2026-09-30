-- 关键词召回走 GIN：一个只会返回商品 id 的 SECURITY DEFINER 函数（2026-09-30 架构审查）。
--
-- ### 问题
--
-- SearchProductsByKeyword 的 p.search_vector @@ to_tsquery(...) 用的算子 ts_match_vq
-- **不是 leakproof**。products 挂着 RLS，规划器因此不许把它排到租户谓词前面，GIN 索引
-- idx_products_fts（00016）进不了计划：只能先按 merchant_id 取出全店商品，再逐行匹配。
-- 实测每店 4 万商品：RLS 下 6.7 ms（全店扫、Rows Removed by Filter: 39980），
-- 同一条查询去掉 RLS 走 GIN 0.04 ms；而且前者随商品数线性变慢。
--
-- ### 两条路，选了 (b)
--
-- (a) ALTER FUNCTION pg_catalog.ts_match_vq(tsvector, tsquery) LEAKPROOF。一行，但：
--     · 改的是系统目录里的内建函数，要真超级用户。托管 Postgres（云厂商 RDS 一类）的
--       「管理员」不是超级用户，这份迁移在那里直接失败；
--     · pg_dump 不导出、pg_upgrade 不保留系统函数的属性变更。换一次库或升一次大版本它就
--       **安静地没了**：不报错，只是检索又退回全店扫 —— 最难发现的那种回退；
--     · leakproof 是对「任何输入都不会以依赖数据的方式报错或留痕」的担保。PostgreSQL 自己
--       没给 ts_match_vq 这个担保，我们替它签字，担的是全库所有用到 @@ 的地方。
--
-- (b) 一个 SECURITY DEFINER 函数，函数体里显式 merchant_id = current_merchant() 后再 @@，
--     在没有 RLS 的上下文里 GIN 照常可用。选它，并把它能做的事压到最小：
--
--     · **只返回 id。** 调用方（db/queries/search.sql）拿这些 id 回 products 表 JOIN，
--       而那次 JOIN 仍在 keel_app 的 RLS 之下。所以就算函数体写错、吐出别家的 id，
--       外层也一行都取不到 —— 这个函数只决定「本店哪些商品命中」，不决定「看得见谁」。
--       租户隔离的最后一道仍是 RLS，跨商家隔离测试照常在守。
--     · **owner 是一个专用的 NOLOGIN 角色 keel_search_definer**，不是迁移角色（超级用户）。
--       它带 BYPASSRLS（不带就和 keel_app 一样被 FORCE RLS 管着，白做），但只被授予
--       products 的三列（id, merchant_id, search_vector）的 SELECT，别的表一张都读不了。
--       00021 文件头③否决 SECURITY DEFINER 的理由正是「owner 是超级用户，函数体就是一段
--       能绕过全部 RLS 的代码」；这里 owner 能碰到的全部数据就是这三列。
--       NOLOGIN + 不授予任何人成员资格：没有人能以它的身份连上来或 SET ROLE 过去
--       （internal/repository 的 TestKeywordDefinerRoleIsNarrow 钉住）。
--     · search_path 固定为 pg_catalog, pg_temp，表与函数全部写全名：调用方改不了它解析到
--       哪个 products、哪个 current_merchant()。
--     · current_merchant() 读的是调用方事务里的 app.merchant_id；没设就 42501 报错，
--       与直接查表一样 fail-closed。平台作用域（app.platform_scope = on）下同样报错。
--
--     与 agent_ro 那组视图（00131）是同一个做法：显式 merchant_id = current_merchant()
--     的过滤写在库里的对象上，不写在 db/queries 里（scripts/check_query_tenancy.py 只扫后者，
--     它要挡的是「应用层重复过滤把 RLS 测试喂饱」，这里外层 JOIN 仍然只靠 RLS）。
--
-- 它要超级用户（或本身带 BYPASSRLS 的 CREATEROLE 角色）才建得出 BYPASSRLS 的角色，
-- 与 (a) 一样是一次性的特权要求；差别是它进 pg_dumpall 的角色导出、随库迁移，
-- 不会在升级后悄悄消失。
--
-- 性能上的代价：GIN 在全平台的 search_vector 上，命中的是**所有商家**里含这些二元组的
-- 商品，再按 merchant_id 过滤。常见词在商家很多时会多扫一些堆页；那是延迟，不是泄露
-- （返回值只含本店 id）。真到了那一天，换 btree_gin 的 (merchant_id, search_vector) 复合 GIN。

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'keel_search_definer') THEN
        CREATE ROLE keel_search_definer NOLOGIN NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
    END IF;
END $$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO keel_search_definer;
GRANT SELECT (id, merchant_id, search_vector) ON products TO keel_search_definer;

-- ROWS 200：检索的召回上限就是这个量级（service 层 RowLimit），给规划器一个接近的估计，
-- 让外层选「按 id 逐个回表」而不是去扫 products。
-- +goose StatementBegin
CREATE FUNCTION keyword_hit_products(q tsquery) RETURNS SETOF bigint
    LANGUAGE sql STABLE STRICT SECURITY DEFINER PARALLEL SAFE ROWS 200
    SET search_path = pg_catalog, pg_temp
AS $$
    SELECT p.id
      FROM public.products p
     WHERE p.merchant_id = public.current_merchant()
       AND p.search_vector @@ q
$$;
-- +goose StatementEnd

ALTER FUNCTION keyword_hit_products(tsquery) OWNER TO keel_search_definer;
REVOKE ALL ON FUNCTION keyword_hit_products(tsquery) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION keyword_hit_products(tsquery) TO keel_app;

COMMENT ON FUNCTION keyword_hit_products(tsquery) IS
    '本店 search_vector 命中 q 的商品 id（00172）。SECURITY DEFINER 只为让 GIN 在 RLS 下可用；'
    '调用方回 products JOIN 时仍受 RLS 管。owner 只读得到 products 的三列。';

-- +goose Down
DROP FUNCTION keyword_hit_products(tsquery);
REVOKE SELECT (id, merchant_id, search_vector) ON products FROM keel_search_definer;
REVOKE USAGE ON SCHEMA public FROM keel_search_definer;
-- 角色是集群级的，同一集群上别的库（测试库、库存库）可能还在用它的授权，这里不 DROP ROLE。
-- 与 00131 的 keel_agent_ro 同一惯例。
