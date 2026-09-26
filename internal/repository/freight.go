package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 运费模板（数据模型 §7「运费模板」，迁移 00041）。
//
// 这一层只搬数据：模板成不成立（恰好一条默认规则、省不重叠、区划码认不认识）
// 在 service/admin_freight.go 判，运费怎么算在 service/freight_calc.go 算。

var (
	// ErrFreightTemplateNotFound：模板不存在、已删除或属于别家店（RLS 下三者同形）。
	ErrFreightTemplateNotFound = errors.New("运费模板不存在")

	// ErrFreightStoreTemplateTaken：这家门店已经有一个门店模板（uk_freight_templates_store）。
	ErrFreightStoreTemplateTaken = errors.New("这家门店已经有门店运费模板")
)

// FreightRule 是一条计费规则（freight_template_rules 的一行）。
type FreightRule struct {
	RegionCodes        []string // 空 = 默认规则
	FirstUnit          int32
	FirstFeeCents      int64
	AdditionalUnit     int32
	AdditionalFeeCents int64
	FreeThresholdCents int64
	FreeQuantity       int32
}

// IsDefault 是不是「其余地区」那条默认规则。
func (r FreightRule) IsDefault() bool { return len(r.RegionCodes) == 0 }

// FreightTemplate 是一个模板连同它的全部规则（默认规则排在最后）。
type FreightTemplate struct {
	ID                       int64
	Name                     string
	StoreID                  *int64
	ChargeMode               int16
	IsDefault                bool
	UndeliverableRegionCodes []string
	Rules                    []FreightRule
}

// AdminFreightTemplate 是后台看到的模板：模板本身 + 挂着它的商品数 + 时间戳。
type AdminFreightTemplate struct {
	FreightTemplate
	ProductCount int32
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// FreightTemplateWrite 是新建与整体替换共用的写入参数（已经校验过）。
type FreightTemplateWrite struct {
	Name                     string
	StoreID                  *int64
	ChargeMode               int16
	IsDefault                bool
	UndeliverableRegionCodes []string
	Rules                    []FreightRule
}

// SKUFreightInfo 是一行商品计运费要的两样东西。
type SKUFreightInfo struct {
	SKUID      int64
	WeightGram int32
	// TemplateID 是商品单独挂的模板；nil = 没单独挂。
	TemplateID *int64
}

// FreightTx 是运费这一面。
type FreightTx interface {
	// ListSKUFreightInfo 取一批 SKU 的重量与商品单独挂的模板。
	ListSKUFreightInfo(ctx context.Context, skuIDs []int64) (map[int64]SKUFreightInfo, error)
	// ListFreightTemplatesForPricing 取计价要用到的模板（连同规则）：
	// templateIDs 里那几个、这家门店的门店模板、全店默认模板。按 id 索引。
	ListFreightTemplatesForPricing(ctx context.Context, templateIDs []int64, storeID int64) (map[int64]FreightTemplate, error)

	AdminListFreightTemplates(ctx context.Context, storeID *int64, limit, offset int64) ([]AdminFreightTemplate, error)
	AdminCountFreightTemplates(ctx context.Context, storeID *int64) (int64, error)
	// AdminGetFreightTemplate 取一个未删除的模板；查不到 ErrFreightTemplateNotFound。
	AdminGetFreightTemplate(ctx context.Context, id int64) (AdminFreightTemplate, error)
	// LockFreightTemplate 锁住模板行、回它此刻的归属（store_id）。查不到 ErrFreightTemplateNotFound。
	LockFreightTemplate(ctx context.Context, id int64) (storeID *int64, err error)
	// LockFreightTemplateForLink 确认 id 是未删除的全店模板并以共享锁钉住它；
	// 不是（不存在、已删除、是门店模板）时 ErrFreightTemplateNotFound。
	LockFreightTemplateForLink(ctx context.Context, id int64) error
	CountProductsOnFreightTemplate(ctx context.Context, id int64) (int64, error)
	// CreateFreightTemplate 插模板与规则，回新 id。设默认时先取消旧默认。
	CreateFreightTemplate(ctx context.Context, w FreightTemplateWrite) (int64, error)
	// ReplaceFreightTemplate 整体替换（模板行 + 全部规则）。调用方已经锁过这一行。
	ReplaceFreightTemplate(ctx context.Context, id int64, w FreightTemplateWrite) error
	// SoftDeleteFreightTemplate 置 deleted_at。查不到 ErrFreightTemplateNotFound。
	SoftDeleteFreightTemplate(ctx context.Context, id int64) error
}

func (t tenantTx) ListSKUFreightInfo(ctx context.Context, skuIDs []int64) (map[int64]SKUFreightInfo, error) {
	rows, err := t.q.ListSKUFreightInfo(ctx, skuIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]SKUFreightInfo, len(rows))
	for _, r := range rows {
		out[r.ID] = SKUFreightInfo{SKUID: r.ID, WeightGram: r.WeightGram, TemplateID: r.FreightTemplateID}
	}
	return out, nil
}

func (t tenantTx) freightRules(ctx context.Context, ids []int64) (map[int64][]FreightRule, error) {
	out := make(map[int64][]FreightRule, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := t.q.ListFreightRules(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		codes := r.RegionCodes
		if codes == nil {
			codes = []string{}
		}
		out[r.TemplateID] = append(out[r.TemplateID], FreightRule{
			RegionCodes: codes, FirstUnit: r.FirstUnit, FirstFeeCents: r.FirstFeeCents,
			AdditionalUnit: r.AdditionalUnit, AdditionalFeeCents: r.AdditionalFeeCents,
			FreeThresholdCents: r.FreeThresholdCents, FreeQuantity: r.FreeQuantity,
		})
	}
	return out, nil
}

func nonNilCodes(c []string) []string {
	if c == nil {
		return []string{}
	}
	return c
}

func (t tenantTx) ListFreightTemplatesForPricing(ctx context.Context, templateIDs []int64,
	storeID int64) (map[int64]FreightTemplate, error) {
	if templateIDs == nil {
		templateIDs = []int64{}
	}
	rows, err := t.q.ListFreightTemplatesForPricing(ctx, db.ListFreightTemplatesForPricingParams{
		TemplateIds: templateIDs, StoreID: &storeID,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	rules, err := t.freightRules(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]FreightTemplate, len(rows))
	for _, r := range rows {
		out[r.ID] = FreightTemplate{
			ID: r.ID, Name: r.Name, StoreID: r.StoreID, ChargeMode: r.ChargeMode,
			IsDefault: r.IsDefault, UndeliverableRegionCodes: nonNilCodes(r.UndeliverableRegionCodes),
			Rules: rules[r.ID],
		}
	}
	return out, nil
}

func adminFreightFromRow(r db.AdminListFreightTemplatesRow, rules []FreightRule) AdminFreightTemplate {
	if rules == nil {
		rules = []FreightRule{}
	}
	return AdminFreightTemplate{
		FreightTemplate: FreightTemplate{
			ID: r.ID, Name: r.Name, StoreID: r.StoreID, ChargeMode: r.ChargeMode,
			IsDefault: r.IsDefault, UndeliverableRegionCodes: nonNilCodes(r.UndeliverableRegionCodes),
			Rules: rules,
		},
		ProductCount: r.ProductCount,
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
}

func (t tenantTx) AdminListFreightTemplates(ctx context.Context, storeID *int64,
	limit, offset int64) ([]AdminFreightTemplate, error) {
	l, o, err := clampPage(limit, offset)
	if err != nil {
		return nil, err
	}
	rows, err := t.q.AdminListFreightTemplates(ctx, db.AdminListFreightTemplatesParams{
		StoreID: storeID, RowLimit: l, RowOffset: o,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	rules, err := t.freightRules(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]AdminFreightTemplate, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminFreightFromRow(r, rules[r.ID]))
	}
	return out, nil
}

func (t tenantTx) AdminCountFreightTemplates(ctx context.Context, storeID *int64) (int64, error) {
	return t.q.AdminCountFreightTemplates(ctx, storeID)
}

func (t tenantTx) AdminGetFreightTemplate(ctx context.Context, id int64) (AdminFreightTemplate, error) {
	r, err := t.q.AdminGetFreightTemplate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminFreightTemplate{}, fmt.Errorf("freight template %d: %w", id, ErrFreightTemplateNotFound)
	}
	if err != nil {
		return AdminFreightTemplate{}, err
	}
	rules, err := t.freightRules(ctx, []int64{id})
	if err != nil {
		return AdminFreightTemplate{}, err
	}
	return adminFreightFromRow(db.AdminListFreightTemplatesRow(r), rules[id]), nil
}

func (t tenantTx) LockFreightTemplate(ctx context.Context, id int64) (*int64, error) {
	r, err := t.q.LockFreightTemplate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("freight template %d: %w", id, ErrFreightTemplateNotFound)
	}
	if err != nil {
		return nil, err
	}
	return r.StoreID, nil
}

func (t tenantTx) LockFreightTemplateForLink(ctx context.Context, id int64) error {
	_, err := t.q.LockFreightTemplateForLink(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("freight template %d 不是未删除的全店模板: %w", id, ErrFreightTemplateNotFound)
	}
	return err
}

func (t tenantTx) CountProductsOnFreightTemplate(ctx context.Context, id int64) (int64, error) {
	return t.q.CountProductsOnFreightTemplate(ctx, &id)
}

// writeFreightRules 插一个模板的全部规则，sort_order 即数组下标。
func (t tenantTx) writeFreightRules(ctx context.Context, id int64, rules []FreightRule) error {
	for i, r := range rules {
		if err := t.q.InsertFreightRule(ctx, db.InsertFreightRuleParams{
			TemplateID: id, SortOrder: int32(i), RegionCodes: nonNilCodes(r.RegionCodes),
			FirstUnit: r.FirstUnit, FirstFeeCents: r.FirstFeeCents,
			AdditionalUnit: r.AdditionalUnit, AdditionalFeeCents: r.AdditionalFeeCents,
			FreeThresholdCents: r.FreeThresholdCents, FreeQuantity: r.FreeQuantity,
		}); err != nil {
			return err
		}
	}
	return nil
}

// mapFreightWriteErr 把两条部分唯一索引与门店外键的违例翻成领域错误。
func mapFreightWriteErr(err error, w FreightTemplateWrite) error {
	switch {
	case isUniqueViolation(err, "uk_freight_templates_store"):
		return fmt.Errorf("store %d: %w", derefInt64(w.StoreID), ErrFreightStoreTemplateTaken)
	case isForeignKeyViolation(err) && w.StoreID != nil:
		// 门店不存在或属于别家店（复合外键）。「已软删的门店」由 service 先查过。
		return fmt.Errorf("store %d: %w", *w.StoreID, ErrCatalogNotFound)
	default:
		return err
	}
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (t tenantTx) CreateFreightTemplate(ctx context.Context, w FreightTemplateWrite) (int64, error) {
	if w.IsDefault {
		// 新行还没有 id：except_id = 0 取消全部旧默认。
		if _, err := t.q.ClearDefaultFreightTemplate(ctx, 0); err != nil {
			return 0, err
		}
	}
	id, err := t.q.InsertFreightTemplate(ctx, db.InsertFreightTemplateParams{
		Name: w.Name, StoreID: w.StoreID, ChargeMode: w.ChargeMode, IsDefault: w.IsDefault,
		UndeliverableRegionCodes: nonNilCodes(w.UndeliverableRegionCodes),
	})
	if err != nil {
		return 0, mapFreightWriteErr(err, w)
	}
	if err := t.writeFreightRules(ctx, id, w.Rules); err != nil {
		return 0, err
	}
	return id, nil
}

func (t tenantTx) ReplaceFreightTemplate(ctx context.Context, id int64, w FreightTemplateWrite) error {
	if w.IsDefault {
		if _, err := t.q.ClearDefaultFreightTemplate(ctx, id); err != nil {
			return err
		}
	}
	n, err := t.q.UpdateFreightTemplate(ctx, db.UpdateFreightTemplateParams{
		ID: id, Name: w.Name, StoreID: w.StoreID, ChargeMode: w.ChargeMode, IsDefault: w.IsDefault,
		UndeliverableRegionCodes: nonNilCodes(w.UndeliverableRegionCodes),
	})
	if err != nil {
		return mapFreightWriteErr(err, w)
	}
	if n == 0 {
		return fmt.Errorf("freight template %d: %w", id, ErrFreightTemplateNotFound)
	}
	if _, err := t.q.DeleteFreightRules(ctx, id); err != nil {
		return err
	}
	return t.writeFreightRules(ctx, id, w.Rules)
}

func (t tenantTx) SoftDeleteFreightTemplate(ctx context.Context, id int64) error {
	n, err := t.q.SoftDeleteFreightTemplate(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("freight template %d: %w", id, ErrFreightTemplateNotFound)
	}
	return nil
}
