package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// DeliveryTier 是同城配送的一档：距离 ≤ WithinM 米收 FeeCents（00110）。
type DeliveryTier struct {
	WithinM  int32 `json:"within_m"`
	FeeCents int64 `json:"fee_cents"`
}

// LocalDelivery 是一家店的同城配送配置。没配过的店是零值（不设起送价、配送费 0）。
type LocalDelivery struct {
	MinOrderCents int64
	FreeOverCents int64
	Tiers         []DeliveryTier
	UpdatedAt     *time.Time // 没配过为 nil
}

// LocalDeliveryPricing 是计价时读到的：这家店走不走同城配送、配置、门店到收货坐标的距离。
type LocalDeliveryPricing struct {
	Local bool
	LocalDelivery
	// DistanceM 为 nil 当且仅当算不出（没传坐标，或门店没有坐标）。
	DistanceM *float64
}

// LocalDeliveryTx 是同城配送这一面（嵌在 StoreTx 里）。
type LocalDeliveryTx interface {
	FindStoreLocalDelivery(ctx context.Context, storeID int64) (LocalDelivery, error)
	UpsertStoreLocalDelivery(ctx context.Context, storeID int64, d LocalDelivery) (LocalDelivery, error)
	// LocalDeliveryForPricing：门店不存在（或已软删）返回 ErrCatalogNotFound。lat / lng 为 nil = 没有收货坐标。
	LocalDeliveryForPricing(ctx context.Context, storeID int64, lat, lng *float64) (LocalDeliveryPricing, error)
}

func decodeTiers(raw []byte) ([]DeliveryTier, error) {
	tiers := []DeliveryTier{}
	if len(raw) == 0 {
		return tiers, nil
	}
	if err := json.Unmarshal(raw, &tiers); err != nil {
		return nil, fmt.Errorf("store_local_delivery.fee_tiers 解不开: %w", err)
	}
	return tiers, nil
}

func (t tenantTx) FindStoreLocalDelivery(ctx context.Context, storeID int64) (LocalDelivery, error) {
	r, err := t.q.GetStoreLocalDelivery(ctx, storeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalDelivery{Tiers: []DeliveryTier{}}, nil
	}
	if err != nil {
		return LocalDelivery{}, err
	}
	tiers, err := decodeTiers(r.FeeTiers)
	if err != nil {
		return LocalDelivery{}, err
	}
	at := r.UpdatedAt.Time
	return LocalDelivery{MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, Tiers: tiers, UpdatedAt: &at}, nil
}

func (t tenantTx) UpsertStoreLocalDelivery(ctx context.Context, storeID int64, d LocalDelivery) (LocalDelivery, error) {
	if d.Tiers == nil {
		d.Tiers = []DeliveryTier{}
	}
	raw, err := json.Marshal(d.Tiers)
	if err != nil {
		return LocalDelivery{}, err
	}
	r, err := t.q.UpsertStoreLocalDelivery(ctx, db.UpsertStoreLocalDeliveryParams{StoreID: storeID,
		MinOrderCents: d.MinOrderCents, FreeOverCents: d.FreeOverCents, FeeTiers: raw})
	if err != nil {
		return LocalDelivery{}, err
	}
	tiers, err := decodeTiers(r.FeeTiers)
	if err != nil {
		return LocalDelivery{}, err
	}
	at := r.UpdatedAt.Time
	return LocalDelivery{MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, Tiers: tiers, UpdatedAt: &at}, nil
}

func (t tenantTx) LocalDeliveryForPricing(ctx context.Context, storeID int64, lat, lng *float64) (LocalDeliveryPricing, error) {
	p := db.LocalDeliveryForPricingParams{StoreID: storeID, HasPoint: lat != nil && lng != nil}
	if p.HasPoint {
		p.Lat, p.Lng = *lat, *lng
	}
	r, err := t.q.LocalDeliveryForPricing(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalDeliveryPricing{}, fmt.Errorf("store %d: %w", storeID, ErrCatalogNotFound)
	}
	if err != nil {
		return LocalDeliveryPricing{}, err
	}
	tiers, err := decodeTiers(r.FeeTiers)
	if err != nil {
		return LocalDeliveryPricing{}, err
	}
	out := LocalDeliveryPricing{Local: r.Local,
		LocalDelivery: LocalDelivery{MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, Tiers: tiers}}
	if r.DistanceM >= 0 {
		d := r.DistanceM
		out.DistanceM = &d
	}
	return out, nil
}
