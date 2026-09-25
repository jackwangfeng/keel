-- 超时补偿定时任务（Task 6）要扫的两类行，其中一类今天没有索引。
--
-- 00006 建了 `idx_orders_status_expire ON orders(merchant_id, status, expire_at)
-- WHERE status = 10` —— 正好覆盖第一类（正常的超时未支付）。
--
-- 第二类是 00013 文件头里那笔明写的欠账：**status = 0 的孤儿草稿**，进程在
-- 「订单落库提交」与「SAGA 提交」之间死掉留下的。它不可见、不可支付、不占库存，
-- 但也没人来关它。那一轮写的是「清理它属于 Task 6 的范围（扫 status = 0 且
-- expire_at < now() 的行，关到 90）」。
--
-- 这两类行的谓词形状一样、处置完全不同，所以是两条扫描、两个索引，
-- 而不是把 00006 那个部分索引的 WHERE 放宽成 `status IN (0, 10)` ——
-- 后者要改 00006（不许），而且会让一条「待支付超时」的扫描顺带翻过所有草稿。
--
-- ### 为什么索引以 merchant_id 打头，而数据模型 §12 的 jobs 出队索引不这样
--
-- §12 那三个索引刻意不带 merchant_id 前缀，前提写在那里：「worker 出队以平台
-- 身份执行，不走 RLS 注入」。**本仓库没有那个前提。** keel_app 不能绕过 RLS
-- （db.NewPool 的自检会拒绝一个能绕过 RLS 的角色启动），也不该有那样一份凭据
-- （理由与 internal/app 里 KEEL_DTM_DSN 那段是同一条：应用握着能绕过 RLS 的
-- 连接，比任何一次越权读取都更难发现）。
--
-- 所以本任务的扫描是**逐租户**的：定时任务先读 merchants（tenant-root 类，
-- 没有 RLS，正是为了让租户解析能在 SET LOCAL 之前读它），再一家一家进
-- WithTenant。于是每条扫描的真实谓词都是
-- `merchant_id = current_merchant() AND status = ? AND expire_at < now()`，
-- 索引前缀这条规矩在这条路径上原样成立，不需要豁免。
--
-- 公平调度因此落在应用层而不是 SQL 层，形状照 §12 的「上限 + 兜底」，
-- 差别逐条写在 internal/service/sweep.go 的文件头。

-- +goose Up

CREATE INDEX idx_orders_draft_expire ON orders(merchant_id, expire_at)
    WHERE status = 0;

-- +goose Down
DROP INDEX idx_orders_draft_expire;
