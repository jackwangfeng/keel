package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 新人礼：首单前的买家自动收到一张指定的券（数据模型 §7「营销活动」，类型 5）。
//
// # 触发点：登录成功之后
//
// 本项目**没有注册接口**（auth.go 的 Login 注释：密码路径刻意不「首登即注册」，
// 验证码 / 微信登录还是 501），买家账号只从种子与后台导入进来。所以「新注册」落不到一个
// 注册事件上，改用可以判定的条件：
//
//	新人 ⇔ 这个买家还没有一笔进过 SAGA 且没被关掉的订单（UserHasPlacedOrder）
//
// 触发点是登录成功 —— 那是买家每一次会话的起点，首单之前一定会经过它。将来有了注册接口，
// 在注册成功的同一处再调一次 GrantNewBuyerGifts 即可，判据与发放逻辑都不用动。
//
// # 复用券的定向发放能力
//
// 占名额走 BumpTemplateForGrant（与后台定向发放同一条条件 UPDATE：总量不超发、停用不发、
// 过期不发），落券走 issueCoupon（有效期两种模式同一份算法），source 记 3 新人礼 ——
// 与「商家手工定向发放」分开，报表才分得清哪些券是规则自动发的。
//
// # 一人一张
//
// promotion_gift_grants 的主键（活动 × 买家）是最终仲裁：两次并发登录都走到发放，只有一次
// 插得进发放记录，另一次整个事务回滚（连同它占的名额与刚落的券）。
//
// # 尽力而为
//
// 发不出来（模板停用、领完、过期）只记日志，**绝不让登录失败** —— 买家来登录不是来领券的，
// 为一张赠券把人挡在门外是本末倒置。每个活动一个事务：一个活动发不出来不影响另一个。

// errGiftAlreadyGranted 让「这个买家在这个活动里已经领过」回滚整个发放事务。
var errGiftAlreadyGranted = errors.New("新人礼已经发过")

// GrantNewBuyerGifts 给一个买家补发此刻生效的全部新人礼。返回这次新发出的张数。
func GrantNewBuyerGifts(ctx context.Context, repo AuthRepository, userID int64, now time.Time,
	log *slog.Logger) int {
	if log == nil {
		log = slog.Default()
	}
	var gifts []repository.Promotion
	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		live, err := tx.ListLivePromotions(ctx, now)
		if err != nil {
			return err
		}
		for _, p := range live {
			if p.Type == repository.PromoNewBuyerGift && p.GiftTemplateID != nil {
				gifts = append(gifts, p)
			}
		}
		if len(gifts) == 0 {
			return nil
		}
		placed, err := tx.UserHasPlacedOrder(ctx, userID)
		if err != nil {
			return err
		}
		if placed {
			gifts = nil
		}
		return nil
	})
	if err != nil {
		log.WarnContext(ctx, "新人礼：读活动失败，这次登录不发", "user_id", userID, "err", err)
		return 0
	}

	granted := 0
	for _, p := range gifts {
		err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			had, err := tx.HasGiftGrant(ctx, p.ID, userID)
			if err != nil || had {
				if had {
					return errGiftAlreadyGranted
				}
				return err
			}
			win, err := tx.BumpTemplateForGrant(ctx, *p.GiftTemplateID, 1, now)
			if err != nil {
				return err
			}
			cid, err := issueCoupon(ctx, tx, *p.GiftTemplateID, userID,
				repository.CouponSourceNewBuyerGift, win, now)
			if err != nil {
				return err
			}
			inserted, err := tx.InsertGiftGrant(ctx, p.ID, userID, cid)
			if err != nil {
				return err
			}
			if !inserted {
				// 并发的另一次登录先插进去了：回滚，把占的名额与这张券一起撤掉。
				return errGiftAlreadyGranted
			}
			return nil
		})
		switch {
		case err == nil:
			granted++
			log.InfoContext(ctx, "新人礼：已发放", "user_id", userID, "promotion_id", p.ID,
				"template_id", *p.GiftTemplateID)
		case errors.Is(err, errGiftAlreadyGranted):
		case errors.Is(err, repository.ErrCouponTemplateExhausted):
			log.WarnContext(ctx, "新人礼：券模板停用、领完或已过期，没有发出去", "user_id", userID,
				"promotion_id", p.ID, "template_id", *p.GiftTemplateID)
		default:
			log.ErrorContext(ctx, "新人礼：发放失败", "user_id", userID, "promotion_id", p.ID, "err", err)
		}
	}
	return granted
}
