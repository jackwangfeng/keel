-- 订单上快照券名：orders.coupon_name（数据模型 §5，契约 Order.coupon_name）。
--
-- =============================================================================
-- 为什么要快照，而不是详情页现 JOIN 券模板
-- =============================================================================
--
-- 00026 给订单落了 user_coupon_id，订单详情于是只能回一个 id 和一笔优惠金额 ——
-- 客户端要显示「已用：满 100 减 20」，只能拿 id 再去查券。而券实例**不快照规则**
-- （契约 UserCoupon 的描述原话），名字在 coupon_templates 上，商家随时能改。
-- 现 JOIN 的结果是：模板改名之后，三个月前那一单的详情页显示的是今天的名字 ——
-- 与「商品改名不影响历史订单」（order_items.title_snapshot）、「门店改名不影响
-- 历史订单」（orders.store_snapshot）是同一个 bug 的第三个实例。
--
-- 模板本身删不掉（user_coupons 对它有复合外键），所以「删了之后显示不出来」
-- 今天不会发生；但那是一条随 schema 变的前提，快照不依赖它。
--
-- =============================================================================
-- 写入：与 store_snapshot 同一条语句、同一个快照
-- =============================================================================
--
-- CreateOrderDraft 那条 INSERT ... SELECT 里用一个标量子查询取
-- user_coupons → coupon_templates.name（db/queries/orders.sql）。
-- 不让应用先查一次券名再传进来，理由与 store_snapshot 一字不差：外键
-- （user_coupon_id）与快照必须来自同一条语句 —— 两步之间模板可以改名，
-- 于是 user_coupon_id 指着那张券、快照写着另一个名字，两者都「看起来正常」。
--
-- =============================================================================
-- 约束：券名与券同进同出
-- =============================================================================
--
-- CHECK ((user_coupon_id IS NULL) = (coupon_name IS NULL))。两个方向各挡一件事：
--   · 有券没名字 → 快照那一步漏了（子查询没匹配上、或者将来有人另写一条插入
--     路径忘了带它），详情页会显示一张没有名字的券；
--   · 有名字没券 → 名字无从解释。
-- 子查询匹配不上时（user_coupon_id 指向一张不存在或不属于本租户的券）这条 CHECK
-- 与 orders_user_coupon_fkey 会一起拒绝 —— 那本来就是一次该失败的下单。
--
-- =============================================================================
-- 存量回填：用迁移时刻的模板名
-- =============================================================================
--
-- 存量订单下单那一刻的券名**已经丢了**（此前没有任何地方记过它），能拿到的
-- 最好近似是模板此刻的名字。它在两种情况下与真相不同：
--   · 下单之后、迁移之前模板改过名 —— 回填的是新名字；
--   · 没有第三种：模板删不掉（见上），所以不存在回填不出来的行。
-- 券功能是 0.1.0 当天才上线的（00026），这个窗口实际上只有几个小时。
-- 把它写在这里而不是假装回填是精确的：将来有人对账时发现某一单的券名与当时的
-- 活动名不一致，这段话就是答案。
--
-- 回填以管理员身份跑（goose 用 keel 连接），不受 RLS 约束，一条 UPDATE 覆盖全部租户；
-- JOIN 带上 merchant_id 相等，与两条复合外键的形状一致。
--
-- 列可空、无默认值：没用券的订单就是 NULL，契约里整个不出现。

-- +goose Up

ALTER TABLE orders ADD COLUMN coupon_name TEXT;

UPDATE orders o
   SET coupon_name = ct.name
  FROM user_coupons uc
  JOIN coupon_templates ct ON ct.id = uc.template_id AND ct.merchant_id = uc.merchant_id
 WHERE uc.id = o.user_coupon_id
   AND uc.merchant_id = o.merchant_id
   AND o.user_coupon_id IS NOT NULL;

ALTER TABLE orders ADD CONSTRAINT chk_coupon_name_with_coupon
    CHECK ((user_coupon_id IS NULL) = (coupon_name IS NULL));

-- +goose Down

ALTER TABLE orders DROP CONSTRAINT chk_coupon_name_with_coupon;
ALTER TABLE orders DROP COLUMN coupon_name;
