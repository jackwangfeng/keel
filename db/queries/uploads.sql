-- 文件元数据（数据模型 §13，契约 POST /admin/uploads）。M4 Task 2。
--
-- 同样刻意不带 WHERE merchant_id，理由见 db/queries/products.sql 的文件头。
-- 注释里一个反引号都不许有（理由见 db/queries/inventories.sql 第三条）。

-- name: CreateStaffUpload :one
-- 后台操作员传的文件。purpose 由**路径**决定（/admin/uploads 固定 1 商品图），
-- 不是一个入参：请求体里给它一个参数位，等于让客户端能把一张退款凭证登记成
-- 商品图，而 chk_upload_owner 只管 user_id / staff_id 二选一，管不了这个。
--
-- 与 C 端的 POST /uploads 是**两条路**，不是一条路的两种用法：那一条填
-- user_id，这一条填 staff_id。同一个入口同时接受两种身份，等于把「这次是谁传
-- 的」变成一个要靠 token 形状猜的东西，而猜错的代价是「运营的操作记在某个
-- C 端用户名下」。
--
-- referenced 走列默认值 FALSE。置 TRUE 是 MarkUploadReferenced 的事，
-- 而且必须与引用它的业务对象在同一个事务里 —— 否则孤儿回收会在提交与扫描
-- 之间的窗口里删掉刚用上的图。
INSERT INTO uploads (staff_id, purpose, driver, storage_key,
                     content_type, size_bytes, sha256)
VALUES (sqlc.arg(staff_id), sqlc.arg(purpose), sqlc.arg(driver),
        sqlc.arg(storage_key), sqlc.arg(content_type), sqlc.arg(size_bytes),
        sqlc.arg(sha256))
RETURNING id, user_id, staff_id, purpose, driver, storage_key,
          content_type, size_bytes, sha256, referenced, created_at;

-- name: GetUpload :one
-- 取一条文件元数据。跨租户的那一条在 RLS 之下返回 0 行，
-- 服务层把它翻成契约的 422 upload-not-found —— 与「这个 id 不存在」同一个
-- 响应，不给探测器留下区分两者的口子。
--
-- purpose 一起返回，因为整组替换那条接口要挡「拿退款凭证当商品图挂上去」
-- （422 upload-wrong-purpose）。**这是应用层的检查，不是数据库能表达的** ——
-- 数据库挡得住的是跨租户（复合外键 (upload_id, merchant_id)），挡不住
-- 「是自己的文件，但用途不对」。两道闸门挡的不是同一件事，缺一不可。
-- 返回列里**不含租户列**：那个词一出现在 db/queries 里，
-- scripts/check_query_tenancy.py 就会红 —— 它挡的是「应用层再过滤一遍租户」，
-- 而它的判据是这个词出现过没有，不区分你是拿它过滤还是只是回读。
-- 这里不需要回读：这一行能被读出来，本身就已经证明它属于当前租户。
SELECT id, user_id, staff_id, purpose, driver, storage_key,
       content_type, size_bytes, sha256, referenced, created_at
  FROM uploads
 WHERE id = $1;

-- name: MarkUploadReferenced :execrows
-- 把 referenced 置为 TRUE。**必须与引用它的业务对象在同一个事务里**
-- （数据模型 §13 明写）：否则存在这样的窗口 —— 商品图刚提交、清理任务恰好
-- 扫到、文件被删，而商品详情页上那张图已经是 404。
--
-- rows_affected = 0 只可能是这条文件不在本租户视野内（RLS），
-- 而那一步在整组替换里已经由 GetUpload 挡过一次；这里返回行数是第二道 ——
-- 一个静默的 0 行会让 referenced 永远停在 FALSE，然后 24 小时后被回收掉。
UPDATE uploads SET referenced = TRUE
 WHERE id = $1 AND NOT referenced;
