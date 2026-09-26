package handler

import (
	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/service"
)

// 营销活动在买家侧响应里的三种形状（契约 PromotionHit / OrderPromotion / PromotionTag）。
// 试算、购物车、订单、商品列表与详情共用这一处渲染：同一个活动在四个页面上不该长得不一样。

// apiPromotionHits 把活动计算的结果装成契约的 PromotionHit。nil 渲染成空数组 ——
// 契约里它是必返的，「没有任何活动」是一个查过的答案。
func apiPromotionHits(hits []service.PromotionHit) []api.PromotionHit {
	out := make([]api.PromotionHit, 0, len(hits))
	for _, h := range hits {
		stack := h.StackWithCoupon
		ph := api.PromotionHit{
			PromotionId:      h.PromotionID,
			Name:             h.Name,
			PromotionType:    api.PromotionType(h.Type),
			Applied:          h.Applied,
			DiscountCents:    api.Money(h.DiscountCents),
			StackWithCoupon:  &stack,
			ReachedThreshold: h.ReachedThreshold,
			NextThreshold:    h.NextThreshold,
			Shortfall:        h.Shortfall,
			SkuIds:           append([]int64{}, h.SKUIDs...),
			Message:          h.Message,
		}
		if h.ThresholdUnit != 0 {
			u := api.PromotionHitThresholdUnit(h.ThresholdUnit)
			ph.ThresholdUnit = &u
		}
		out = append(out, ph)
	}
	return out
}

// apiOrderPromotions 把订单上的活动快照（JSONB）装成契约的 OrderPromotion。
//
// 解不开时给空数组，**不让整条请求失败** —— 与 apiOrderItems 对规格快照的处置同一个理由：
// 那是一份历史快照，为一行读不懂的旧快照让用户打不开自己的订单，换来的不是正确性。
func apiOrderPromotions(raw []byte) []api.OrderPromotion {
	out := []api.OrderPromotion{}
	snaps, err := service.DecodeOrderPromotions(raw)
	if err != nil {
		return out
	}
	for _, p := range snaps {
		out = append(out, api.OrderPromotion{
			PromotionId:   p.PromotionID,
			Name:          p.Name,
			PromotionType: api.PromotionType(p.Type),
			DiscountCents: api.Money(p.DiscountCents),
			SkuIds:        append([]int64{}, p.SKUIDs...),
		})
	}
	return out
}

// ptrTags 是 apiPromotionTags 的指针形状（ProductSummary 上它是可选字段，但我们总是给）。
func ptrTags(tags []service.ProductPromotionTag) *[]api.PromotionTag {
	out := apiPromotionTags(tags)
	return &out
}

func moneyPtrOf(v *int64) *api.Money {
	if v == nil {
		return nil
	}
	m := api.Money(*v)
	return &m
}

// apiPromotionTags 把商品的活动标签装成契约的 PromotionTag。
func apiPromotionTags(tags []service.ProductPromotionTag) []api.PromotionTag {
	out := make([]api.PromotionTag, 0, len(tags))
	for _, t := range tags {
		out = append(out, api.PromotionTag{
			PromotionId:   t.PromotionID,
			PromotionType: api.PromotionType(t.Type),
			Label:         t.Label,
			EndsAt:        t.EndsAt,
		})
	}
	return out
}
