package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 同城配送（00110）与同城配送模板（00111）。来源的判定写在 db/queries/local_delivery.sql 的 LocalDeliveryForPricing 上。

// DeliveryTier 是同城配送的一档：距离 ≤ WithinM 米收 FeeCents。
type DeliveryTier struct {
	WithinM  int32 `json:"within_m"`
	FeeCents int64 `json:"fee_cents"`
}

// LocalDelivery 是一套同城配送规则（门店自定义的，或模板的）。
type LocalDelivery struct {
	MinOrderCents int64
	FreeOverCents int64
	Tiers         []DeliveryTier
}

// 规则来源（契约 AdminLocalDelivery.source）。
const (
	LocalDeliverySourceCustom          = "custom"
	LocalDeliverySourceTemplate        = "template"
	LocalDeliverySourceDefaultTemplate = "default_template"
	LocalDeliverySourceNone            = "none"
)

// StoreLocalDeliveryRow 是一家店自己的那一行。Exists = false 即「跟随默认模板」。
type StoreLocalDeliveryRow struct {
	Exists     bool
	TemplateID *int64
	Own        LocalDelivery // template_id 非空时不生效，保留着
	UpdatedAt  *time.Time
}

// LocalDeliveryPricing 是计价（与后台展示）时读到的生效规则。
type LocalDeliveryPricing struct {
	Local        bool   // 这家店走同城配送（有围栏且不是默认店）
	Source       string // LocalDeliverySourceCustom / …
	TemplateID   *int64
	TemplateName *string
	LocalDelivery
	// DistanceM 为 nil 当且仅当算不出（没传坐标，或门店没有坐标）。
	DistanceM *float64
}

// LocalDeliveryTemplate 是一个同城配送模板。
type LocalDeliveryTemplate struct {
	ID        int64
	Name      string
	IsDefault bool
	LocalDelivery
	StoreCount int64 // 几家门店在用（显式引用 + 默认模板时跟随默认的围栏店）；只在列表里有
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

var (
	ErrLocalDeliveryTemplateNotFound = errors.New("同城配送模板不存在")
	// ErrLocalDeliveryTemplateConflict：重名，或并发地把两个模板设成默认。
	ErrLocalDeliveryTemplateConflict = errors.New("同城配送模板重名或默认模板冲突")
)

// LocalDeliveryTx 是同城配送这一面（嵌在 StoreTx 里）。
type LocalDeliveryTx interface {
	FindStoreLocalDelivery(ctx context.Context, storeID int64) (StoreLocalDeliveryRow, error)
	UpsertStoreLocalDelivery(ctx context.Context, storeID int64, own LocalDelivery, templateID *int64) error
	DeleteStoreLocalDelivery(ctx context.Context, storeID int64) error
	// LocalDeliveryForPricing：门店不存在（或已软删）返回 ErrCatalogNotFound。lat / lng 为 nil = 没有收货坐标。
	LocalDeliveryForPricing(ctx context.Context, storeID int64, lat, lng *float64) (LocalDeliveryPricing, error)

	ListLocalDeliveryTemplates(ctx context.Context) ([]LocalDeliveryTemplate, error)
	FindLocalDeliveryTemplate(ctx context.Context, id int64) (LocalDeliveryTemplate, error)
	CreateLocalDeliveryTemplate(ctx context.Context, name string, isDefault bool, r LocalDelivery) (int64, error)
	UpdateLocalDeliveryTemplate(ctx context.Context, id int64, name string, isDefault bool, r LocalDelivery) error
	ClearOtherDefaultLocalDeliveryTemplates(ctx context.Context, keepID int64) error
	CountStoresReferencingTemplate(ctx context.Context, id int64) (int64, error)
	DeleteLocalDeliveryTemplate(ctx context.Context, id int64) error
}

func decodeTiers(raw []byte) ([]DeliveryTier, error) {
	tiers := []DeliveryTier{}
	if len(raw) == 0 {
		return tiers, nil
	}
	if err := json.Unmarshal(raw, &tiers); err != nil {
		return nil, fmt.Errorf("同城配送的 fee_tiers 解不开: %w", err)
	}
	return tiers, nil
}

func encodeTiers(t []DeliveryTier) ([]byte, error) {
	if t == nil {
		t = []DeliveryTier{}
	}
	return json.Marshal(t)
}

func templateConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%s: %w", pgErr.ConstraintName, ErrLocalDeliveryTemplateConflict)
	}
	return err
}

func (t tenantTx) FindStoreLocalDelivery(ctx context.Context, storeID int64) (StoreLocalDeliveryRow, error) {
	r, err := t.q.GetStoreLocalDelivery(ctx, storeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoreLocalDeliveryRow{Own: LocalDelivery{Tiers: []DeliveryTier{}}}, nil
	}
	if err != nil {
		return StoreLocalDeliveryRow{}, err
	}
	tiers, err := decodeTiers(r.FeeTiers)
	if err != nil {
		return StoreLocalDeliveryRow{}, err
	}
	at := r.UpdatedAt.Time
	return StoreLocalDeliveryRow{Exists: true, TemplateID: r.TemplateID, UpdatedAt: &at,
		Own: LocalDelivery{MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, Tiers: tiers}}, nil
}

func (t tenantTx) UpsertStoreLocalDelivery(ctx context.Context, storeID int64, own LocalDelivery, templateID *int64) error {
	raw, err := encodeTiers(own.Tiers)
	if err != nil {
		return err
	}
	_, err = t.q.UpsertStoreLocalDelivery(ctx, db.UpsertStoreLocalDeliveryParams{StoreID: storeID,
		MinOrderCents: own.MinOrderCents, FreeOverCents: own.FreeOverCents, FeeTiers: raw, TemplateID: templateID})
	return err
}

func (t tenantTx) DeleteStoreLocalDelivery(ctx context.Context, storeID int64) error {
	return t.q.DeleteStoreLocalDelivery(ctx, storeID)
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
	out := LocalDeliveryPricing{Local: r.Local, Source: r.Source, TemplateID: r.TemplateID, TemplateName: r.TemplateName,
		LocalDelivery: LocalDelivery{MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, Tiers: tiers}}
	if r.DistanceM >= 0 {
		d := r.DistanceM
		out.DistanceM = &d
	}
	return out, nil
}

func (t tenantTx) ListLocalDeliveryTemplates(ctx context.Context) ([]LocalDeliveryTemplate, error) {
	rows, err := t.q.ListLocalDeliveryTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LocalDeliveryTemplate, 0, len(rows))
	for _, r := range rows {
		tiers, err := decodeTiers(r.FeeTiers)
		if err != nil {
			return nil, err
		}
		out = append(out, LocalDeliveryTemplate{ID: r.ID, Name: r.Name, IsDefault: r.IsDefault,
			LocalDelivery: LocalDelivery{MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, Tiers: tiers},
			StoreCount:    r.StoreCount, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time})
	}
	return out, nil
}

func (t tenantTx) FindLocalDeliveryTemplate(ctx context.Context, id int64) (LocalDeliveryTemplate, error) {
	r, err := t.q.GetLocalDeliveryTemplate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalDeliveryTemplate{}, fmt.Errorf("template %d: %w", id, ErrLocalDeliveryTemplateNotFound)
	}
	if err != nil {
		return LocalDeliveryTemplate{}, err
	}
	tiers, err := decodeTiers(r.FeeTiers)
	if err != nil {
		return LocalDeliveryTemplate{}, err
	}
	return LocalDeliveryTemplate{ID: r.ID, Name: r.Name, IsDefault: r.IsDefault,
		LocalDelivery: LocalDelivery{MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, Tiers: tiers},
		CreatedAt:     r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}, nil
}

func (t tenantTx) CreateLocalDeliveryTemplate(ctx context.Context, name string, isDefault bool, r LocalDelivery) (int64, error) {
	raw, err := encodeTiers(r.Tiers)
	if err != nil {
		return 0, err
	}
	id, err := t.q.InsertLocalDeliveryTemplate(ctx, db.InsertLocalDeliveryTemplateParams{Name: name, IsDefault: isDefault,
		MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, FeeTiers: raw})
	return id, templateConflict(err)
}

func (t tenantTx) UpdateLocalDeliveryTemplate(ctx context.Context, id int64, name string, isDefault bool, r LocalDelivery) error {
	raw, err := encodeTiers(r.Tiers)
	if err != nil {
		return err
	}
	n, err := t.q.UpdateLocalDeliveryTemplate(ctx, db.UpdateLocalDeliveryTemplateParams{ID: id, Name: name,
		IsDefault: isDefault, MinOrderCents: r.MinOrderCents, FreeOverCents: r.FreeOverCents, FeeTiers: raw})
	if err != nil {
		return templateConflict(err)
	}
	if n == 0 {
		return fmt.Errorf("template %d: %w", id, ErrLocalDeliveryTemplateNotFound)
	}
	return nil
}

func (t tenantTx) ClearOtherDefaultLocalDeliveryTemplates(ctx context.Context, keepID int64) error {
	return t.q.ClearOtherDefaultLocalDeliveryTemplates(ctx, keepID)
}

func (t tenantTx) CountStoresReferencingTemplate(ctx context.Context, id int64) (int64, error) {
	return t.q.CountStoresReferencingTemplate(ctx, &id)
}

func (t tenantTx) DeleteLocalDeliveryTemplate(ctx context.Context, id int64) error {
	n, err := t.q.DeleteLocalDeliveryTemplate(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("template %d: %w", id, ErrLocalDeliveryTemplateNotFound)
	}
	return nil
}
