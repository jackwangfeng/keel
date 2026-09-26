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

// 运费模板的后台（契约 /admin/freight-templates，数据模型 §7「运费模板」，迁移 00055）。
//
// ===========================================================================
// 权限（契约 StaffRole 矩阵「运费模板」两行）
// ===========================================================================
//
//   - 读（列表、详情）：后台四种角色都能读 —— 与商品目录同一个口径（requireStaff）。
//     大区 / 门店管理员要看得到商品挂的是哪个模板，才说得清一单的运费是怎么来的。
//   - 全店模板（store_id 为 NULL）的写：requireMerchantWide。理由同基准价：它决定
//     全店每一单的运费，而且商品只能挂全店模板 —— 一个店长改了它，别的店的单跟着变。
//   - 门店模板的写：authorizeStore(storeOperate)，与门店价同一个判据（本大区的大区
//     管理员、这家店的门店管理员也能写）。它只影响这家店发货的单。
//   - 改归属（全店 ↔ 门店、换门店）：新旧两边都要过，理由同 authorizeStoreMove ——
//     只判一边的话，一个店长能把全店模板「领」成自己的门店模板。
//
// 每条写接口恰好调这几个判据之一（authz.go 文件头那条纪律），在同一个事务里。

var (
	// ErrFreightBadRequest：模板不成立（没有 / 多于一条默认规则、区划码不认识、
	// 同一个省出现两次、门店模板设成全店默认、门店不存在……）。422。
	ErrFreightBadRequest = errors.New("运费模板的配置不成立")

	// ErrFreightTemplateNotFound：模板不存在、已删除或属于别家店。404。
	ErrFreightTemplateNotFound = errors.New("运费模板不存在")

	// ErrFreightTemplateConflict：这家门店已经有门店模板。409 freight-template-conflict。
	ErrFreightTemplateConflict = errors.New("这家门店已经有门店运费模板")

	// ErrFreightTemplateInUse：还有商品挂着它（删除、或改成门店模板时）。409 freight-template-in-use。
	ErrFreightTemplateInUse = errors.New("还有商品挂着这个运费模板")
)

const (
	scopeAdminFreightCreate = "admin.freight_templates.create"

	maxFreightNameRunes = 60
	maxFreightRules     = 35
	maxFreightUnit      = 1_000_000     // 首件 / 续件件数，或首重 / 续重克数（1000 千克）
	maxFreightFeeCents  = 1_000_000     // 首费 / 续费上限 1 万元：只挡录错的值（多敲几个 0）
	maxFreightThreshold = 1_000_000_000 // 满额包邮门槛上限 1000 万元
	maxFreightFreeQty   = 999_999
)

// FreightTemplateInput 是新建与整体替换共用的入参（契约 FreightTemplateInput）。
type FreightTemplateInput struct {
	Name                     string
	StoreID                  *int64
	ChargeMode               int16
	IsDefault                bool
	Rules                    []repository.FreightRule
	UndeliverableRegionCodes []string
}

// AdminFreightPage 是 GET /admin/freight-templates 的一页。
type AdminFreightPage struct {
	Items    []repository.AdminFreightTemplate
	Total    int64
	Page     int
	PageSize int
}

// AdminFreightService 实现运费模板的五条后台接口。
type AdminFreightService struct {
	repo tenantRunner
	now  func() time.Time
}

func NewAdminFreightService(r tenantRunner) *AdminFreightService {
	return &AdminFreightService{repo: r, now: time.Now}
}

// validateFreightTemplate 在进库之前把模板的每一条规矩用人话说一遍（纯函数）。
// 数据库只兜得住取值（区划码在 34 个之内、金额非负、至多一条默认规则）；
// 「恰好一条默认」「省不重叠」「不配送与规则不重叠」只在这里。
func validateFreightTemplate(in FreightTemplateInput) (repository.FreightTemplateWrite, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrFreightBadRequest, fmt.Sprintf(format, args...))
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > maxFreightNameRunes {
		return repository.FreightTemplateWrite{}, bad("name 必须是 1 到 %d 个字", maxFreightNameRunes)
	}
	if in.ChargeMode != freightChargeByPiece && in.ChargeMode != freightChargeByWeight {
		return repository.FreightTemplateWrite{}, bad("charge_mode 只能是 1 按件 / 2 按重量，实得 %d", in.ChargeMode)
	}
	if in.StoreID != nil && *in.StoreID <= 0 {
		return repository.FreightTemplateWrite{}, bad("store_id 必须是正整数")
	}
	if in.IsDefault && in.StoreID != nil {
		return repository.FreightTemplateWrite{}, bad("门店模板不能设成全店默认（is_default 只对全店模板有意义）")
	}
	if len(in.Rules) == 0 || len(in.Rules) > maxFreightRules {
		return repository.FreightTemplateWrite{}, bad("rules 要有 1 到 %d 条", maxFreightRules)
	}

	owner := map[string]string{} // 区划码 → 它出现在哪（给报错用）
	claim := func(code, where string) error {
		if !validProvinceCode(code) {
			return bad("%s 里的 %q 不是省级行政区划码（6 位，如 110000 北京）", where, code)
		}
		if prev, dup := owner[code]; dup {
			return bad("%s（%s）同时出现在 %s 与 %s：一个省只能有一种运费", provinceName(code), code, prev, where)
		}
		owner[code] = where
		return nil
	}

	var defaults int
	rules := make([]repository.FreightRule, 0, len(in.Rules))
	var def repository.FreightRule
	for i, r := range in.Rules {
		where := fmt.Sprintf("rules[%d]", i)
		if r.FirstUnit < 1 || r.FirstUnit > maxFreightUnit || r.AdditionalUnit < 1 || r.AdditionalUnit > maxFreightUnit {
			return repository.FreightTemplateWrite{}, bad("%s 的首件（首重）与续件（续重）单位必须在 1 到 %d 之间", where, maxFreightUnit)
		}
		if r.FirstFeeCents < 0 || r.FirstFeeCents > maxFreightFeeCents ||
			r.AdditionalFeeCents < 0 || r.AdditionalFeeCents > maxFreightFeeCents {
			return repository.FreightTemplateWrite{}, bad("%s 的首费与续费必须在 0 到 %d 分之间", where, maxFreightFeeCents)
		}
		if r.FreeThresholdCents < 0 || r.FreeThresholdCents > maxFreightThreshold {
			return repository.FreightTemplateWrite{}, bad("%s 的满额包邮门槛必须在 0 到 %d 分之间", where, maxFreightThreshold)
		}
		if r.FreeQuantity < 0 || r.FreeQuantity > maxFreightFreeQty {
			return repository.FreightTemplateWrite{}, bad("%s 的满件包邮件数必须在 0 到 %d 之间", where, maxFreightFreeQty)
		}
		if len(r.RegionCodes) == 0 {
			defaults++
			r.RegionCodes = []string{}
			def = r
			continue
		}
		for _, c := range r.RegionCodes {
			if err := claim(c, where); err != nil {
				return repository.FreightTemplateWrite{}, err
			}
		}
		rules = append(rules, r)
	}
	if defaults != 1 {
		return repository.FreightTemplateWrite{}, bad("要恰好一条默认规则（region_codes 为空数组，管「其余地区」），实得 %d 条", defaults)
	}
	// 默认规则排在最后存：读的顺序（ListFreightRules）也是它在最后，写读一致。
	rules = append(rules, def)

	undeliverable := make([]string, 0, len(in.UndeliverableRegionCodes))
	for _, c := range in.UndeliverableRegionCodes {
		if err := claim(c, "undeliverable_region_codes"); err != nil {
			return repository.FreightTemplateWrite{}, err
		}
		undeliverable = append(undeliverable, c)
	}

	return repository.FreightTemplateWrite{
		Name: name, StoreID: in.StoreID, ChargeMode: in.ChargeMode, IsDefault: in.IsDefault,
		UndeliverableRegionCodes: undeliverable, Rules: rules,
	}, nil
}

// authorizeFreightOwner 是「能不能写这个归属的模板」：nil = 全店模板 → 全店范围；
// 门店模板 → 门店价那一条判据，并确认门店存在（请求体里指名的门店不存在是 422）。
func authorizeFreightOwner(ctx context.Context, tx repository.Tx, storeID *int64) error {
	if storeID == nil {
		_, err := requireMerchantWide(ctx)
		return err
	}
	if _, err := authorizeStore(ctx, tx, *storeID, storeOperate); err != nil {
		if errors.Is(err, repository.ErrCatalogNotFound) {
			return fmt.Errorf("%w: store_id=%d 不存在或已删除", ErrFreightBadRequest, *storeID)
		}
		return err
	}
	if _, _, err := tx.StoreScope(ctx, *storeID); err != nil {
		if errors.Is(err, repository.ErrCatalogNotFound) {
			return fmt.Errorf("%w: store_id=%d 不存在或已删除", ErrFreightBadRequest, *storeID)
		}
		return err
	}
	return nil
}

// mapFreightRepoErr 把 repository 的两个 sentinel 翻成这一层的。
func mapFreightRepoErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrFreightTemplateNotFound):
		return fmt.Errorf("%w: %v", ErrFreightTemplateNotFound, err)
	case errors.Is(err, repository.ErrFreightStoreTemplateTaken):
		return fmt.Errorf("%w: %v", ErrFreightTemplateConflict, err)
	case errors.Is(err, repository.ErrCatalogNotFound):
		return fmt.Errorf("%w: %v", ErrFreightBadRequest, err)
	default:
		return err
	}
}

// List 实现 GET /admin/freight-templates。
func (s *AdminFreightService) List(ctx context.Context, storeID *int64, page, pageSize int) (AdminFreightPage, error) {
	if _, err := requireStaff(ctx); err != nil {
		return AdminFreightPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	out := AdminFreightPage{Items: []repository.AdminFreightTemplate{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		items, err := tx.AdminListFreightTemplates(ctx, storeID, int64(pageSize), int64((page-1)*pageSize))
		if err != nil {
			return err
		}
		out.Items = items
		out.Total, err = tx.AdminCountFreightTemplates(ctx, storeID)
		return err
	})
	if err != nil {
		return AdminFreightPage{}, err
	}
	return out, nil
}

// Get 实现 GET /admin/freight-templates/{template_id}。
func (s *AdminFreightService) Get(ctx context.Context, id int64) (repository.AdminFreightTemplate, error) {
	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminFreightTemplate{}, err
	}
	var out repository.AdminFreightTemplate
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		t, err := tx.AdminGetFreightTemplate(ctx, id)
		out = t
		return err
	})
	if err != nil {
		return repository.AdminFreightTemplate{}, mapFreightRepoErr(err)
	}
	return out, nil
}

// Create 实现 POST /admin/freight-templates（Idempotency-Key 必填）。
// 返回的 bool 为真表示幂等重放。
func (s *AdminFreightService) Create(ctx context.Context, in FreightTemplateInput,
	idemKey string) (repository.AdminFreightTemplate, bool, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return repository.AdminFreightTemplate{}, false, err
	}
	// 校验排在抢占幂等键之前：一个注定被拒的请求不该占掉客户端那把钥匙
	// （同 AdminCatalogService.CreateProduct）。
	w, err := validateFreightTemplate(in)
	if err != nil {
		return repository.AdminFreightTemplate{}, false, err
	}
	hash, err := adminRequestHash(nil, w)
	if err != nil {
		return repository.AdminFreightTemplate{}, false, err
	}
	out, replayed, err := idempotentTx(ctx, s.repo, repository.StaffSubject(id.StaffID),
		scopeAdminFreightCreate, idemKey, hash, archivedCreated,
		func(tx repository.Tx) (repository.AdminFreightTemplate, error) {
			if err := authorizeFreightOwner(ctx, tx, w.StoreID); err != nil {
				return repository.AdminFreightTemplate{}, err
			}
			newID, err := tx.CreateFreightTemplate(ctx, w)
			if err != nil {
				return repository.AdminFreightTemplate{}, err
			}
			return tx.AdminGetFreightTemplate(ctx, newID)
		})
	if err != nil {
		return repository.AdminFreightTemplate{}, false, mapFreightRepoErr(err)
	}
	return out, replayed, nil
}

// Replace 实现 PUT /admin/freight-templates/{template_id}（整体替换）。
func (s *AdminFreightService) Replace(ctx context.Context, templateID int64,
	in FreightTemplateInput) (repository.AdminFreightTemplate, error) {
	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminFreightTemplate{}, err
	}
	w, err := validateFreightTemplate(in)
	if err != nil {
		return repository.AdminFreightTemplate{}, err
	}
	var out repository.AdminFreightTemplate
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 先锁住这一行、拿到它此刻的归属：判权与写落在同一个归属上
		// （同 authorizeStore 上那段「scope 与写在同一个事务里」）。
		oldStore, err := tx.LockFreightTemplate(ctx, templateID)
		if err != nil {
			return err
		}
		if err := authorizeFreightOwner(ctx, tx, oldStore); err != nil {
			return err
		}
		if !sameStore(oldStore, w.StoreID) {
			if err := authorizeFreightOwner(ctx, tx, w.StoreID); err != nil {
				return err
			}
		}
		if oldStore == nil && w.StoreID != nil {
			// 全店模板改成门店模板：商品只能挂全店模板，还挂着的话它们会悄悄变成
			// 「挂着一个门店模板」—— 那是 lockFreightTemplateForLink 不许写出来的状态。
			n, err := tx.CountProductsOnFreightTemplate(ctx, templateID)
			if err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("%w: 还有 %d 件商品挂着它，先把它们改挂别的全店模板，再改成门店模板",
					ErrFreightTemplateInUse, n)
			}
		}
		if err := tx.ReplaceFreightTemplate(ctx, templateID, w); err != nil {
			return err
		}
		out, err = tx.AdminGetFreightTemplate(ctx, templateID)
		return err
	})
	if err != nil {
		return repository.AdminFreightTemplate{}, mapFreightRepoErr(err)
	}
	return out, nil
}

// Delete 实现 DELETE /admin/freight-templates/{template_id}（软删）。
func (s *AdminFreightService) Delete(ctx context.Context, templateID int64) error {
	if _, err := requireStaff(ctx); err != nil {
		return err
	}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		store, err := tx.LockFreightTemplate(ctx, templateID)
		if err != nil {
			return err
		}
		if err := authorizeFreightOwner(ctx, tx, store); err != nil {
			return err
		}
		n, err := tx.CountProductsOnFreightTemplate(ctx, templateID)
		if err != nil {
			return err
		}
		if n > 0 {
			// 静默解除关联会让这些商品悄悄落回门店模板或全店默认 —— 运费变了而商家不知道。
			return fmt.Errorf("%w: 还有 %d 件商品挂着它，先把它们改挂别的模板", ErrFreightTemplateInUse, n)
		}
		return tx.SoftDeleteFreightTemplate(ctx, templateID)
	})
	return mapFreightRepoErr(err)
}

func sameStore(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
