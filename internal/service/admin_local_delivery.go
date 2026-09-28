package service

import (
	"context"
	"fmt"

	"github.com/keel/keel/internal/repository"
)

// 同城配送配置（00110）：GET / PUT /admin/stores/{id}/local-delivery。
//
// 判权与门店模板相同（storeOperate）：大区管理员管本大区的店、门店管理员管自己的店 —— 配送费与门店价、
// 门店模板是同一类「这家店怎么卖」的事。配置对默认店与没有围栏的店也能存，只是不生效（它们走运费模板）；
// 响应里的 active 说明这一点，免得运营配了半天发现没用。

const (
	localDeliveryMaxTiers    = 10
	localDeliveryMaxWithinM  = 100_000    // 100 公里：同城配送再远就不是同城了
	localDeliveryMaxFeeCents = 100_00     // 单档配送费上限 100 元，挡住多打几个 0
	localDeliveryMaxAmount   = 100_000_00 // 起送价 / 免配送费门槛上限 10 万元
)

// LocalDeliveryView 是后台看到的一家店的同城配送配置。
type LocalDeliveryView struct {
	repository.LocalDelivery
	// Active：这家店现在是否按它收费（有围栏且不是默认店）。
	Active bool
}

// FindLocalDelivery 实现 GET /admin/stores/{id}/local-delivery。
func (s *AdminStoreService) FindLocalDelivery(ctx context.Context, storeID int64) (LocalDeliveryView, error) {
	var out LocalDeliveryView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := authorizeStore(ctx, tx, storeID, storeOperate); err != nil {
			return err
		}
		return loadLocalDeliveryView(ctx, tx, storeID, &out)
	})
	return out, err
}

// PutLocalDelivery 实现 PUT /admin/stores/{id}/local-delivery（整份替换）。
func (s *AdminStoreService) PutLocalDelivery(ctx context.Context, storeID int64,
	in repository.LocalDelivery) (LocalDeliveryView, error) {
	if err := validateLocalDelivery(in); err != nil {
		return LocalDeliveryView{}, err
	}
	var out LocalDeliveryView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := authorizeStore(ctx, tx, storeID, storeOperate); err != nil {
			return err
		}
		if _, err := tx.UpsertStoreLocalDelivery(ctx, storeID, in); err != nil {
			return err
		}
		return loadLocalDeliveryView(ctx, tx, storeID, &out)
	})
	return out, err
}

func loadLocalDeliveryView(ctx context.Context, tx repository.Tx, storeID int64, out *LocalDeliveryView) error {
	d, err := tx.FindStoreLocalDelivery(ctx, storeID)
	if err != nil {
		return err
	}
	p, err := tx.LocalDeliveryForPricing(ctx, storeID, nil, nil)
	if err != nil {
		return err
	}
	*out = LocalDeliveryView{LocalDelivery: d, Active: p.Local}
	return nil
}

// validateLocalDelivery：档数、within_m 严格递增且为正、金额非负且有上限。
func validateLocalDelivery(d repository.LocalDelivery) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrCatalogBadRequest}, a...)...)
	}
	if d.MinOrderCents < 0 || d.MinOrderCents > localDeliveryMaxAmount {
		return bad("min_order_cents 取 0–%d", localDeliveryMaxAmount)
	}
	if d.FreeOverCents < 0 || d.FreeOverCents > localDeliveryMaxAmount {
		return bad("free_over_cents 取 0–%d", localDeliveryMaxAmount)
	}
	if len(d.Tiers) > localDeliveryMaxTiers {
		return bad("fee_tiers 至多 %d 档", localDeliveryMaxTiers)
	}
	var prev int32
	for i, t := range d.Tiers {
		if t.WithinM <= prev || t.WithinM > localDeliveryMaxWithinM {
			return bad("fee_tiers[%d].within_m 必须比上一档大，且在 1–%d 米之间", i, localDeliveryMaxWithinM)
		}
		if t.FeeCents < 0 || t.FeeCents > localDeliveryMaxFeeCents {
			return bad("fee_tiers[%d].fee_cents 取 0–%d", i, localDeliveryMaxFeeCents)
		}
		prev = t.WithinM
	}
	return nil
}
