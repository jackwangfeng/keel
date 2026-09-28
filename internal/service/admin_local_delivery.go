package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keel/keel/internal/repository"
)

// 同城配送配置（00110）与同城配送模板（00111）。
//
//	GET    /admin/stores/{id}/local-delivery   这家店生效的规则、来源（自定义 / 模板 / 默认模板 / 无）
//	PUT    /admin/stores/{id}/local-delivery   选一个模板，或填自定义规则
//	DELETE /admin/stores/{id}/local-delivery   跟随默认模板
//	GET/POST /admin/local-delivery-templates, PUT/DELETE /admin/local-delivery-templates/{id}
//
// 判权：门店这一侧与门店模板相同（storeOperate：大区管理员管本大区的店、门店管理员管自己的店）；
// 模板是全店的东西，写要全店范围（管理员、操作员），读任何员工都行（门店管理员要从里面挑）。

const (
	localDeliveryMaxTiers    = 10
	localDeliveryMaxWithinM  = 100_000    // 100 公里：同城配送再远就不是同城了
	localDeliveryMaxFeeCents = 100_00     // 单档配送费上限 100 元，挡住多打几个 0
	localDeliveryMaxAmount   = 100_000_00 // 起送价 / 免配送费门槛上限 10 万元
	scopeLocalDeliveryTplNew = "admin.local-delivery-templates.create"
)

var (
	ErrLocalDeliveryTemplateNotFound = errors.New("同城配送模板不存在")
	// ErrLocalDeliveryTemplateInUse：模板被门店引用着，或它是默认模板（跟随默认的店会悄悄变成配送费 0）。
	ErrLocalDeliveryTemplateInUse = errors.New("同城配送模板正在使用，不能删除")
	// ErrLocalDeliveryTemplateConflict：重名。
	ErrLocalDeliveryTemplateConflict = errors.New("同城配送模板重名")
)

// LocalDeliveryView 是后台看到的一家店的同城配送。
type LocalDeliveryView struct {
	Active       bool   // 这家店现在是否按同城配送收费（有围栏且不是默认店）
	Source       string // repository.LocalDeliverySource*
	TemplateID   *int64
	TemplateName *string
	Effective    repository.LocalDelivery  // 生效的规则
	Custom       *repository.LocalDelivery // 这家店自己存着的自定义规则（没存过为 nil）；引用模板时也保留
	UpdatedAt    *time.Time
}

// StoreLocalDeliveryInput 是 PUT 的请求：TemplateID 与 Custom 恰好给一个。
type StoreLocalDeliveryInput struct {
	TemplateID *int64
	Custom     *repository.LocalDelivery
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

// PutLocalDelivery 实现 PUT /admin/stores/{id}/local-delivery。
func (s *AdminStoreService) PutLocalDelivery(ctx context.Context, storeID int64,
	in StoreLocalDeliveryInput) (LocalDeliveryView, error) {
	if (in.TemplateID == nil) == (in.Custom == nil) {
		return LocalDeliveryView{}, fmt.Errorf("%w: template_id 与自定义规则（min_order_cents / free_over_cents / fee_tiers）恰好给一个",
			ErrCatalogBadRequest)
	}
	own := repository.LocalDelivery{Tiers: []repository.DeliveryTier{}}
	if in.Custom != nil {
		if err := validateLocalDelivery(*in.Custom); err != nil {
			return LocalDeliveryView{}, err
		}
		own = *in.Custom
	}
	var out LocalDeliveryView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := authorizeStore(ctx, tx, storeID, storeOperate); err != nil {
			return err
		}
		if in.TemplateID != nil {
			if _, err := tx.FindLocalDeliveryTemplate(ctx, *in.TemplateID); err != nil {
				if errors.Is(err, repository.ErrLocalDeliveryTemplateNotFound) {
					return fmt.Errorf("%w: template_id=%d 不存在", ErrCatalogBadRequest, *in.TemplateID)
				}
				return err
			}
		}
		if err := tx.UpsertStoreLocalDelivery(ctx, storeID, own, in.TemplateID); err != nil {
			return err
		}
		return loadLocalDeliveryView(ctx, tx, storeID, &out)
	})
	return out, err
}

// ResetLocalDelivery 实现 DELETE /admin/stores/{id}/local-delivery：跟随默认模板（自定义规则一并清掉）。
func (s *AdminStoreService) ResetLocalDelivery(ctx context.Context, storeID int64) (LocalDeliveryView, error) {
	var out LocalDeliveryView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := authorizeStore(ctx, tx, storeID, storeOperate); err != nil {
			return err
		}
		if err := tx.DeleteStoreLocalDelivery(ctx, storeID); err != nil {
			return err
		}
		return loadLocalDeliveryView(ctx, tx, storeID, &out)
	})
	return out, err
}

func loadLocalDeliveryView(ctx context.Context, tx repository.Tx, storeID int64, out *LocalDeliveryView) error {
	row, err := tx.FindStoreLocalDelivery(ctx, storeID)
	if err != nil {
		return err
	}
	p, err := tx.LocalDeliveryForPricing(ctx, storeID, nil, nil)
	if err != nil {
		return err
	}
	*out = LocalDeliveryView{Active: p.Local, Source: p.Source, TemplateID: p.TemplateID, TemplateName: p.TemplateName,
		Effective: p.LocalDelivery, UpdatedAt: row.UpdatedAt}
	if row.Exists {
		own := row.Own
		out.Custom = &own
	}
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

// ---------------------------------------------------------------------------
// 模板
// ---------------------------------------------------------------------------

// LocalDeliveryTemplateInput 是建 / 改模板的请求。
type LocalDeliveryTemplateInput struct {
	Name      string
	IsDefault bool
	Rule      repository.LocalDelivery
}

func (in *LocalDeliveryTemplateInput) validate() error {
	in.Name = strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 50 {
		return fmt.Errorf("%w: name 取 1–50 个字", ErrCatalogBadRequest)
	}
	return validateLocalDelivery(in.Rule)
}

// ListLocalDeliveryTemplates 实现 GET /admin/local-delivery-templates。
func (s *AdminStoreService) ListLocalDeliveryTemplates(ctx context.Context) ([]repository.LocalDeliveryTemplate, error) {
	if _, err := requireStaff(ctx); err != nil {
		return nil, err
	}
	var out []repository.LocalDeliveryTemplate
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.ListLocalDeliveryTemplates(ctx)
		return e
	})
	return out, err
}

// CreateLocalDeliveryTemplate 实现 POST /admin/local-delivery-templates（Idempotency-Key 必填）。
func (s *AdminStoreService) CreateLocalDeliveryTemplate(ctx context.Context, in LocalDeliveryTemplateInput,
	idemKey string) (repository.LocalDeliveryTemplate, bool, error) {
	id, err := requireMerchantWide(ctx)
	if err != nil {
		return repository.LocalDeliveryTemplate{}, false, err
	}
	if idemKey == "" {
		return repository.LocalDeliveryTemplate{}, false, ErrIdempotencyKeyMissing
	}
	if err := in.validate(); err != nil {
		return repository.LocalDeliveryTemplate{}, false, err
	}
	hash, err := adminRequestHash(nil, in)
	if err != nil {
		return repository.LocalDeliveryTemplate{}, false, err
	}
	return idempotentTx(ctx, s.repo, repository.StaffSubject(id.StaffID), scopeLocalDeliveryTplNew, idemKey, hash,
		archivedCreated, func(tx repository.Tx) (repository.LocalDeliveryTemplate, error) {
			if in.IsDefault {
				if err := tx.ClearOtherDefaultLocalDeliveryTemplates(ctx, 0); err != nil {
					return repository.LocalDeliveryTemplate{}, err
				}
			}
			tid, err := tx.CreateLocalDeliveryTemplate(ctx, in.Name, in.IsDefault, in.Rule)
			if err != nil {
				return repository.LocalDeliveryTemplate{}, mapTemplateErr(err)
			}
			return tx.FindLocalDeliveryTemplate(ctx, tid)
		})
}

// UpdateLocalDeliveryTemplate 实现 PUT /admin/local-delivery-templates/{id}（整体替换）。
func (s *AdminStoreService) UpdateLocalDeliveryTemplate(ctx context.Context, tid int64,
	in LocalDeliveryTemplateInput) (repository.LocalDeliveryTemplate, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.LocalDeliveryTemplate{}, err
	}
	if err := in.validate(); err != nil {
		return repository.LocalDeliveryTemplate{}, err
	}
	var out repository.LocalDeliveryTemplate
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if in.IsDefault {
			if err := tx.ClearOtherDefaultLocalDeliveryTemplates(ctx, tid); err != nil {
				return err
			}
		}
		if err := tx.UpdateLocalDeliveryTemplate(ctx, tid, in.Name, in.IsDefault, in.Rule); err != nil {
			return mapTemplateErr(err)
		}
		var e error
		out, e = tx.FindLocalDeliveryTemplate(ctx, tid)
		return e
	})
	return out, err
}

// DeleteLocalDeliveryTemplate 实现 DELETE /admin/local-delivery-templates/{id}。
// 被门店引用着、或是默认模板：409（先让那些店换模板 / 先取消默认）。
func (s *AdminStoreService) DeleteLocalDeliveryTemplate(ctx context.Context, tid int64) error {
	if _, err := requireMerchantWide(ctx); err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		t, err := tx.FindLocalDeliveryTemplate(ctx, tid)
		if err != nil {
			return mapTemplateErr(err)
		}
		if t.IsDefault {
			return fmt.Errorf("%w: 它是默认模板，先把别的模板设为默认或取消默认", ErrLocalDeliveryTemplateInUse)
		}
		n, err := tx.CountStoresReferencingTemplate(ctx, tid)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: 还有 %d 家门店在用", ErrLocalDeliveryTemplateInUse, n)
		}
		return mapTemplateErr(tx.DeleteLocalDeliveryTemplate(ctx, tid))
	})
}

func mapTemplateErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrLocalDeliveryTemplateNotFound):
		return fmt.Errorf("%w: %v", ErrLocalDeliveryTemplateNotFound, err)
	case errors.Is(err, repository.ErrLocalDeliveryTemplateConflict):
		return fmt.Errorf("%w: %v", ErrLocalDeliveryTemplateConflict, err)
	}
	return err
}
