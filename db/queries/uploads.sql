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

-- name: CreateUserUpload :one
-- C 端买家传的文件（契约 POST /uploads）：头像（2）或退款凭证（3）。
-- 填 user_id，不填 staff_id —— 与 CreateStaffUpload 是两条路，理由见那一条的注释；
-- chk_upload_owner 钉着二选一。purpose 是入参，但只收 2 / 3，由服务层判
-- （商品图是后台的事，走 CreateStaffUpload）。
INSERT INTO uploads (user_id, purpose, driver, storage_key,
                     content_type, size_bytes, sha256)
VALUES (sqlc.arg(user_id), sqlc.arg(purpose), sqlc.arg(driver),
        sqlc.arg(storage_key), sqlc.arg(content_type), sqlc.arg(size_bytes),
        sqlc.arg(sha256))
RETURNING id, user_id, staff_id, purpose, driver, storage_key,
          content_type, size_bytes, sha256, referenced, created_at;

-- name: ListEvidenceRefundStores :many
-- 引用了这个凭证地址的退款单，各自所属订单的履约门店（去重）。
-- 后台读退款凭证（GET /admin/uploads/{upload_id}）按它判权：能看引用它的那张退款单，
-- 才能看这张凭证 —— 与后台退款单详情同一个判据。
--
-- 按地址而不是按 upload id 找，是因为 evidence_urls 存的就是地址（契约 Upload.url）；
-- 申请时服务端已把每一项核成 /api/v1/uploads/{id} 这一个形状，所以精确匹配就够。
-- 包含运算走 00038 的 GIN 索引。
SELECT DISTINCT o.store_id
  FROM refunds r
  JOIN orders o ON o.id = r.order_id
 WHERE r.evidence_urls @> ARRAY[sqlc.arg(url)::text]
 ORDER BY o.store_id;

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
-- **不带 AND NOT referenced**：同一个文件被第二次引用是正常的（被驳回的退款重新申请时
-- 带着同一批凭证、同一张图挂到第二个规格上），而带着那个谓词时第二次影响 0 行，
-- 下面那条「0 行 = 不在视野内」的推论就成了假话 —— 症状是第二次引用回 404 / 422。
-- 再写一次 TRUE 没有任何副作用。
--
-- rows_affected = 0 只可能是这条文件不在本租户视野内（RLS），
-- 而那一步在整组替换里已经由 GetUpload 挡过一次；这里返回行数是第二道 ——
-- 一个静默的 0 行会让 referenced 永远停在 FALSE，然后 24 小时后被回收掉。
UPDATE uploads SET referenced = TRUE
 WHERE id = $1;

-- name: ListOrphanUploads :many
-- 孤儿回收的候选（数据模型 §13 的 24 小时规则，service/upload_gc.go）：没被任何业务对象引用、
-- 创建早于截止（now() 减 24 小时，由调用方算好）的文件，只取这个 driver 的（删文件要同一个 driver）。
-- 买家凭证、头像、后台商品图一视同仁。按创建时间从早到晚。部分索引 idx_uploads_orphan 对上这条扫描。
-- 这只是预筛；删不删由 DeleteOrphanUpload 的谓词在行锁之下再判一次。
SELECT id, storage_key
  FROM uploads
 WHERE NOT referenced
   AND created_at < sqlc.arg(cutoff)::timestamptz
   AND driver = sqlc.arg(driver)
 ORDER BY created_at, id
 LIMIT sqlc.arg(page_limit);

-- name: DeleteOrphanUpload :one
-- 删一条孤儿记录，返回它的 storage_key（调用方在事务提交之后删存储里的文件）。
--
-- **谓词把「没被引用、够老」再判一遍，这是与引用赛跑的裁判**：提交售后时标引用
-- （MarkUploadReferenced）与这条 DELETE 撞在同一行上时，谁先拿到行锁谁赢 ——
-- 标引用先拿到：这条 DELETE 等它提交，PostgreSQL 按新版本重算谓词，NOT referenced 不成立，
-- 0 行，文件留着；这条先拿到并提交：标引用那条 UPDATE 影响 0 行，售后申请以「凭证不存在」失败，
-- 而不是落库一张带着 404 图片的申请。只按 id 删的话，前一种交错会删掉一张刚被引用的凭证。
DELETE FROM uploads
 WHERE id = sqlc.arg(id) AND NOT referenced
   AND created_at < sqlc.arg(cutoff)::timestamptz
RETURNING storage_key;

-- name: UnmarkUploadReferenced :execrows
-- 取消引用（头像换掉之后旧头像，PATCH /me）。之后它就是一个普通的孤儿，
-- 创建超过 24 小时即由 upload_gc 回收。只动这个买家自己传的头像：别人的、别的用途的不碰。
UPDATE uploads SET referenced = FALSE
 WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND purpose = 2;

-- name: ListUploadsByDriver :many
-- 存量迁移（cmd/keel-uploads migrate）：本店写在 @driver 上的文件，按 id 往后翻页（id > @after_id）。
SELECT u.id, u.storage_key, u.content_type, u.size_bytes, u.sha256
  FROM uploads u
 WHERE u.driver = sqlc.arg(driver)
   AND u.id > sqlc.arg(after_id)
 ORDER BY u.id
 LIMIT sqlc.arg(page_limit)::int;

-- name: MoveUploadDriver :execrows
-- 存量迁移的最后一步：字节已经拷到新 driver、核对过了，把这一行改指过去。条件带上旧 driver：
-- 两个迁移进程同时跑时第二个改不到（0 行），不会把已经改过的再改一遍。
UPDATE uploads SET driver = sqlc.arg(to_driver)
 WHERE id = sqlc.arg(id) AND driver = sqlc.arg(from_driver);
