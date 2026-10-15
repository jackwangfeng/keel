-- +goose Up
-- 券的精准发放（带对照组），docs/superpowers/specs/2026-10-16-ai-ops-onboarding-restock-coupon-design.md 第三节。
--
-- 一条带 target 的发券提案批准执行时，按那一刻的人群名单随机分成两组：发券组（treated = true）逐个发券，
-- 对照组（treated = false）不发。两组都记在这里，复盘时比两组的下单率，差值才是这张券带来的生意 ——
-- 没有对照组的话，「领了券的人下单多」分不清是券的作用，还是这群人本来就会买。
--
-- 只追加、不改：分组是实验设计，执行之后再动它，复盘的数就不可信了。应用里只有 INSERT 与 SELECT 两条语句；
-- GRANT 仍按 tenant 类的默认给全（db/tenancy.json 的类别是逐表登记的 GRANT 面，为一张表开一个新类别不值），
-- 「不改」由代码守，不由权限守。
-- 复合外键要对端有 (id, merchant_id) 唯一：agent_proposals 之前没有人用复合键指它。表很小（提案条数），直接加。
ALTER TABLE agent_proposals ADD CONSTRAINT uk_agent_proposals_id_merchant UNIQUE (id, merchant_id);

CREATE TABLE coupon_target_members (
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant(),
    proposal_id    BIGINT      NOT NULL,
    user_id        BIGINT      NOT NULL,
    treated        BOOLEAN     NOT NULL,
    user_coupon_id BIGINT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, proposal_id, user_id),
    CONSTRAINT chk_coupon_target_members_coupon CHECK ((user_coupon_id IS NOT NULL) = treated),
    FOREIGN KEY (proposal_id, merchant_id) REFERENCES agent_proposals(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (user_id, merchant_id)     REFERENCES users(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (user_coupon_id, merchant_id) REFERENCES user_coupons(id, merchant_id)
);

ALTER TABLE coupon_target_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE coupon_target_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON coupon_target_members USING (merchant_id = current_merchant())
    WITH CHECK (merchant_id = current_merchant());
GRANT SELECT, INSERT, UPDATE, DELETE ON coupon_target_members TO keel_app;

-- +goose Down
DROP TABLE coupon_target_members;
ALTER TABLE agent_proposals DROP CONSTRAINT uk_agent_proposals_id_merchant;
