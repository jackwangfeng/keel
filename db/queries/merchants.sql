-- 租户目录本身（数据模型 §2，契约 POST /admin/merchants）。M4 Task 4。
--
-- 这个文件只有一条语句，而它是**全仓库唯一一条写 merchants 的应用侧 SQL**。
-- 建店以外的一切都不该出现在这里：keel_app 在这张表上只有 SELECT 与 INSERT
-- （00005 收窄、00021 把 INSERT 还回来，两份迁移的文件头写了为什么），
-- 也就是说这里写一条 UPDATE 或 DELETE 出来，症状是运行期 42501。
--
-- 注释里一个反引号都不许有（理由见 db/queries/inventories.sql 第三条）。

-- name: CreateMerchant :one
-- 开一家店。
--
-- 只给 code 与 name 两列：status 走 DEFAULT 1 正常，其余是时间戳。
-- **刻意不给 status 一个参数位** —— 契约的 MerchantCreateRequest 里没有它，
-- 而给它一个参数位等于让调用方能直接开出一家「已停用」或「待审核」的店，
-- 那是一条绕过审核流程（本轮还不存在）的路，将来补审核时没人会想起这里。
--
-- code 撞车时报 23505 / merchants_code_key，repository 那一层挑成
-- ErrMerchantCodeTaken（契约里那个 409）。这里**不写 ON CONFLICT**：
-- 「这个 code 已经有人用了」是一次要如实告诉调用方的失败，不是一次可以
-- 沉默跳过的重复 —— DO NOTHING 会让第二次开店返回零行，而那条路上调用方
-- 拿到的会是一个「建成功了但没有 id」的空洞。
INSERT INTO merchants (code, name)
VALUES (sqlc.arg(code), sqlc.arg(name))
RETURNING id, code, name, status, created_at;
