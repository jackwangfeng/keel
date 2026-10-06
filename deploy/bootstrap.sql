-- 生产部署的引导脚本：**只建商家，不播任何演示数据**。
--
-- 为什么需要它：app 启动时 tenant.Preflight 会校验 KEEL_DEFAULT_MERCHANT 指向的商家
-- 存在且可服务，对不上直接拒绝启动。而演示栈的 db/seed/single.sql 会连带播商品、
-- 优惠券、订单，甚至一个固定口令的演示买家（13800000000 / keel-demo-2026）——
-- 生产部署要的是空白库加一家自己的店。
--
-- 首个管理员不用这里建：应用启动时 EnsureBootstrapAdmin 会往 stdout 打一个一次性
-- token（单次使用、过期作废），用它换第一个平台管理员。刻意不把它做成初始化步骤，
-- 因为任何「默认口令 + 首次登录强制改密」都比「一次性 token」更容易被自动化脚本吃掉。
--
-- 幂等：重复执行既不会重复插入，也不会改动已有商家。改店名请直接 UPDATE。
--
-- 由 compose.prod.yaml 的 seed 服务调用，参数用 psql -v 传入：
--   -v merchant_code=... -v merchant_name=...

\set ON_ERROR_STOP on

INSERT INTO merchants (code, name, status)
SELECT :'merchant_code', :'merchant_name', 1
 WHERE NOT EXISTS (SELECT 1 FROM merchants m WHERE m.code = :'merchant_code');

\echo 'bootstrap: 商家 [' :merchant_code '] 已就绪，接下来看 app 打的 bootstrap token'
