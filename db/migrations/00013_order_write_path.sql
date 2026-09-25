-- 下单写路径的两处 schema 前置。Task 4/5 需要它们，00006 建订单域时还看不出来
-- （那一轮只有读路径）。**不改 00006 一个字**，改动全部落在这份新迁移里。
--
-- ===========================================================================
-- 一、orders / order_items / inventory_logs 的 merchant_id 补上
--     DEFAULT current_merchant()
-- ===========================================================================
--
-- 理由和 00010 给 user_tokens 用的那条一模一样，但这里是被一条**机械闸门**逼
-- 出来的，不是风格选择：scripts/check_query_tenancy.py 不许 db/queries/*.sql
-- 里出现 merchant_id 这个词。那条规矩的理由写在它自己的文件头 —— 应用层再过滤
-- 一遍租户，「RLS 到底有没有生效」就变得测不出来了：跨租户读不到数据既可能是
-- RLS 拦住的，也可能只是那个 WHERE 拦住的，而 RLS 失效不报错、不变慢、不留痕迹。
--
-- 于是这三张表上的每一条 INSERT 都面临同一个选择：要么在 SQL 里写出
-- merchant_id（把整条下单写路径塞进那个脚本的 ALLOW 豁免里，等于把规矩对这条
-- 最要紧的链路整个关掉），要么让数据库自己填。选后者。
--
-- 默认值不是「省事」，是**不给调用方留口子**：生成的 Go 函数签名里根本没有
-- merchant_id 这个参数，所以「拿 A 店的上下文往 B 店名下建单」连编译都编不出来。
-- RLS 的 WITH CHECK 仍是第二道 —— DEFAULT 只在没写这一列时才生效，
-- 真有人写了别家的 merchant_id，策略会当场拒绝。
--
-- 数据模型 §4 / §5 / §12 的 DDL 已随本轮一起改，两边保持一份真相。
--
-- ===========================================================================
-- 二、orders.status 多一个「0 创建中」，并把它的两条出边登记进状态机
-- ===========================================================================
--
-- 这一条是 SAGA 分支签名逼出来的，值得完整记下来，因为它决定了下单的**顺序**。
--
-- `BranchFunc func(gid, branchID, op string) int` —— 分支只拿到三个字符串，
-- 而且它跑在任何 HTTP 请求之外（崩溃重启后由协调器重放，进程对原请求毫无记忆）。
-- 所以分支需要的一切必须能从 gid 推出来，**或者已经落在库里**。
-- 「扣哪些 SKU、各扣几件」推不出来，只能落库。
--
-- 于是顺序只能是：**先把订单与订单项写进库，再提交 SAGA**。反过来
-- （SAGA 先跑、建单分支再写行）做不到 —— 那个分支手里没有商品、没有数量、
-- 没有金额，它连要写什么都不知道。
--
-- 但「已落库」不等于「已成交」。一笔刚落库、SAGA 还没跑完的订单，如果就是
-- `10 待支付`，那么在库存分支失败与补偿跑完之间，它对用户是一笔可支付的订单，
-- 而它背后一件库存都没扣。所以落库时的状态必须是一个**用户看不见、也付不了钱**
-- 的态，由建单分支把它推到 10。
--
-- 0 这个值本身是安全的：`orders.status` 上没有 CHECK，契约的 OrderStatus 枚举
-- 也不含 0 —— 而这正是要的效果，**状态 0 的订单永远不会出现在任何响应里**：
-- 它要么被建单分支推到 10 再返回，要么被补偿关到 90。
--
-- 两条出边登记进 order_status_transitions，而不是让它游离在状态机之外：
-- 那张表是状态机的唯一真相源（数据模型 §5），一个不在表里的状态就是一个
-- 没人知道存在的状态。
--
-- 没有 (0,20)：一笔还没建成的订单不可能先被支付。
--
-- ### 崩溃窗口，说清楚
--
-- 进程若在「订单落库提交」与「SAGA 提交」之间死掉，库里会留下一行 status = 0
-- 的孤儿订单：它不可见、不可支付、不占库存，**但也没人来关它**。代价是一行
-- 垃圾数据，不是一次漏卖或超卖。清理它属于 Task 6 那个超时关单任务的范围
-- （扫 status = 0 且 expire_at < now() 的行，关到 90），本轮没有实现 ——
-- 这是一笔明写的欠账，不是一个被忽略的窗口。

-- +goose Up

ALTER TABLE orders         ALTER COLUMN merchant_id SET DEFAULT current_merchant();
ALTER TABLE order_items    ALTER COLUMN merchant_id SET DEFAULT current_merchant();
ALTER TABLE inventory_logs ALTER COLUMN merchant_id SET DEFAULT current_merchant();

-- 0 创建中 ──建单分支正向──► 10 待支付
--          └──全局补偿────► 90 已关闭
--
-- ON CONFLICT DO NOTHING 让这份迁移在已经跑过的库上可以重放
-- （TestMigrateIsIdempotent 会连跑两次）。
INSERT INTO order_status_transitions (from_status, to_status)
VALUES (0, 10), (0, 90)
ON CONFLICT (from_status, to_status) DO NOTHING;

-- +goose Down
DELETE FROM order_status_transitions WHERE from_status = 0;

ALTER TABLE inventory_logs ALTER COLUMN merchant_id DROP DEFAULT;
ALTER TABLE order_items    ALTER COLUMN merchant_id DROP DEFAULT;
ALTER TABLE orders         ALTER COLUMN merchant_id DROP DEFAULT;
