-- H5（Flutter Web）浏览器测试的夹具：CI 的单商家栈（db/seed/single.sql 灌过的 demo 店）上补测试要的数据。
-- 演示站不用它 —— 那边的同一份数据由 reset-demo.sh 经后台接口建。可以反复跑（幂等）。
--
-- 包邮券（e2e 可反复领）：flutter_app/integration_test/order_flow.dart 的包邮券用例要领它。
-- 每人限领 100：用例每跑一轮领一张。不设适用范围行 = 全场（数据模型 §7）。
-- merchant_id 显式写：以管理员身份跑，没有租户上下文。
INSERT INTO coupon_templates (merchant_id, name, coupon_type, threshold_cents, discount_cents,
                              discount_rate, max_discount_cents, valid_mode, valid_days,
                              total_count, per_user_limit, claimable)
SELECT m.id, '包邮券（e2e 可反复领）', 4, 0, 0, 0, 0, 2, 30, 0, 100, TRUE
  FROM merchants m
 WHERE m.code = 'demo'
   AND NOT EXISTS (SELECT 1 FROM coupon_templates c WHERE c.merchant_id = m.id AND c.name = '包邮券（e2e 可反复领）');
