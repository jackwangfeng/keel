-- 店铺设置的「商家自己能改的那一半」：shop_preferences（契约 GET / PUT /admin/shop-settings）。
-- DDL 照抄数据模型设计 §2，那里是唯一真相源。
--
-- ===========================================================================
-- 为什么是一张新表，而不是给 keel_app 开 shop_settings 的写权限
-- ===========================================================================
--
-- 要改的几项（时区、自动确认收货天数、退货寄回时限、客服电话）本来有两项就躺在
-- shop_settings 上（timezone / auto_confirm_days，00001）。最省事的写法是
-- GRANT UPDATE ON shop_settings TO keel_app —— 这正是 00005 文件头点名要防的那一步：
--
--   shop_settings 是 tenant-root 类，**没有 RLS**（租户解析要在 SET LOCAL 之前读它，
--   挂上策略整站第一跳 404），防护全压在 GRANT 面上。给了 UPDATE，
--   同一条连接就能改别家的 domain（域名劫持）、改别家的 extra（那里放着支付回调的验签密钥，
--   见 repository.ChannelNotifySecret —— 改了它就能给别家店伪造一笔已支付）。
--
-- 两条缓解都试过，都不如拆表：
--
--   · 列级 GRANT（只给 timezone / auto_confirm_days 的 UPDATE）：挡住了 domain 与 extra，
--     挡不住「租户 1 的上下文里改租户 2 的时区与自动确认天数」—— 没有 RLS，
--     谓词 WHERE merchant_id = ? 是应用层写的，而本仓库的规矩是租户隔离不靠应用层记得。
--   · 给 shop_settings 挂「读放开、写按租户」的策略：要为它新造一个租户类别
--     （tenancy.json 与两侧闸门各改一处），而它守的仍然是一张混着全局物理资源（domain）
--     与支付密钥的表 —— 写权限一旦开了口，列清单就是唯一的防线。
--
-- 拆出来之后，这张表就是一张普通的 tenant 类表：merchant_id = current_merchant() 的
-- 标准策略、四权 GRANT、闸门全部现成，一行新机制都不需要。shop_settings 回到它本来的样子：
-- 平台管的、解析期要读的、keel_app 只读的那一半（domain / logo_url / currency / extra）。
--
-- ===========================================================================
-- 两列从 shop_settings 搬过来，不是抄一份
-- ===========================================================================
--
-- timezone 与 auto_confirm_days 在 shop_settings 上删掉。留着就是两份真相：
-- 接口改的是新表，而任何一个还读旧列的地方（报表按它切天、自动确认按它算截止）
-- 会静默地按旧值跑，没有任何报错。读它们的两处（repository.ShopTimezone /
-- AutoConfirmDays）同一次提交改成读新表。
--
-- 已有的值原样搬过来：有 shop_settings 行的店在新表里各得一行。没有那一行的店
-- （开店不写 shop_settings，00021）在新表里也没有 —— 读的时候按列默认值算，
-- 与今天的行为一致（repository.DefaultShopTimezone / DefaultAutoConfirmDays）。
--
-- ===========================================================================
-- 新增的两列
-- ===========================================================================
--
--   return_ship_days  退货退款审核通过（20 待买家退货）之后多少天没填寄回物流就自动关闭
--                     （20 → 60，service/return_timeout.go）。口径从 audited_at 起算：
--                     那是「商家同意退货」的时刻，买家从这一刻起知道要寄；
--                     从申请时间算会把商家审核拖延的时间算到买家头上。默认 7 天。
--   service_phone     客服电话，展示用。可空。
--
-- 店铺名称**不在**这里：它是 merchants.name，由平台经 merchant_revisions 改（00024，
-- 商家目录是平台的，租户改不了）。GET /admin/shop-settings 只读地回显它。
--
-- 天数的上限与 chk_auto_confirm_days（00036）同为 365：下限 1 是业务上的（0 天 = 审核
-- 通过的同时就超时），上限只挡录错的值。

-- +goose Up

CREATE TABLE shop_preferences (
    merchant_id       BIGINT      PRIMARY KEY DEFAULT current_merchant() REFERENCES merchants(id),
    service_phone     TEXT,                              -- 客服电话，展示用
    timezone          TEXT        NOT NULL DEFAULT 'Asia/Shanghai',  -- IANA 名；报表按它切自然日
    auto_confirm_days SMALLINT    NOT NULL DEFAULT 7,    -- 发货后多少天自动确认收货
    return_ship_days  SMALLINT    NOT NULL DEFAULT 7,    -- 退货审核通过后多少天未寄回自动关闭
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_shop_pref_auto_confirm_days CHECK (auto_confirm_days BETWEEN 1 AND 365),
    CONSTRAINT chk_shop_pref_return_ship_days  CHECK (return_ship_days BETWEEN 1 AND 365),
    CONSTRAINT chk_shop_pref_text CHECK (
        timezone <> '' AND length(timezone) <= 64
        AND (service_phone IS NULL OR (service_phone <> '' AND length(service_phone) <= 32))
    )
);

INSERT INTO shop_preferences (merchant_id, timezone, auto_confirm_days)
SELECT merchant_id, timezone, auto_confirm_days FROM shop_settings;

ALTER TABLE shop_settings DROP CONSTRAINT chk_auto_confirm_days;
ALTER TABLE shop_settings DROP COLUMN auto_confirm_days;
ALTER TABLE shop_settings DROP COLUMN timezone;

ALTER TABLE shop_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE shop_preferences FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON shop_preferences
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_shop_preferences_updated_at
    BEFORE UPDATE ON shop_preferences FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON shop_preferences TO keel_app;

-- +goose Down
ALTER TABLE shop_settings ADD COLUMN timezone TEXT NOT NULL DEFAULT 'Asia/Shanghai';
ALTER TABLE shop_settings ADD COLUMN auto_confirm_days SMALLINT NOT NULL DEFAULT 7;
ALTER TABLE shop_settings ADD CONSTRAINT chk_auto_confirm_days
    CHECK (auto_confirm_days BETWEEN 1 AND 365);
-- 改过设置而没有 shop_settings 行的店要补一行，否则回滚会把它们的设置丢掉。
INSERT INTO shop_settings (merchant_id, timezone, auto_confirm_days)
SELECT merchant_id, timezone, auto_confirm_days FROM shop_preferences
ON CONFLICT (merchant_id) DO UPDATE
   SET timezone = EXCLUDED.timezone, auto_confirm_days = EXCLUDED.auto_confirm_days;
REVOKE ALL ON shop_preferences FROM keel_app;
DROP TABLE shop_preferences;
