-- 上传的第三种主体：渠道 binding（渠道适配层第二期，商品源拉进来的商品图）。
--
-- 商品图原先只有后台操作员会传（staff_id）。Shopify 当商品源时，图是系统从商品源下载的，没有哪个员工传过它；
-- 记到某个员工名下等于伪造操作记录。于是加一个可空的 channel_binding_id，chk_upload_owner 从「二选一」
-- 改成「三选一」（恰好一个非空）。复合外键（与 user_id 同一种待遇）：别家租户的 binding 在物理上挂不上来。
-- binding 不会被删（应用里只有停用），所以外键用默认的 NO ACTION。
-- +goose Up
ALTER TABLE uploads ADD COLUMN channel_binding_id BIGINT;
ALTER TABLE uploads ADD CONSTRAINT fk_uploads_channel_binding
    FOREIGN KEY (channel_binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id);
ALTER TABLE uploads DROP CONSTRAINT chk_upload_owner;
ALTER TABLE uploads ADD CONSTRAINT chk_upload_owner CHECK (num_nonnulls(user_id, staff_id, channel_binding_id) = 1);

-- +goose Down
-- 有渠道下载的图时回退会失败（chk_upload_owner 回到二选一）：先人工处理那些行。
ALTER TABLE uploads DROP CONSTRAINT chk_upload_owner;
ALTER TABLE uploads ADD CONSTRAINT chk_upload_owner CHECK ((user_id IS NOT NULL) <> (staff_id IS NOT NULL));
ALTER TABLE uploads DROP CONSTRAINT fk_uploads_channel_binding;
ALTER TABLE uploads DROP COLUMN channel_binding_id;
