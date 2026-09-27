package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// 门店 / 大区后台那 21 条接口的业务层（契约 Store tag 里 /admin/ 前缀那一段）。
// 数据模型 §4。
//
// ===========================================================================
// 这一层同样**不重新定义一套错误**
// ===========================================================================
//
// 与 admin_catalog.go 的文件头一字不差：repository 那一层已经把失败分成了与
// 契约一一对应的 sentinel（ErrRegionCodeConflict、ErrStoreFenceRequired、
// *InvalidFenceError …），在这里再翻一遍等于造出第二张会与第一张分叉的映射表。
// 所以这一层原样放行，由 handler 的 writeStoreError 一次性翻成状态码。
//
// 这一层自己产生的失败只有一种：请求体本身不成立（空字段、超长、负价、
// 坐标只给了一半）。它复用 ErrCatalogBadRequest —— 同一个 handler 包里
// 再造一个「也翻成 422」的 sentinel，只会让两条路以后有机会分叉。
//
// ===========================================================================
// 为什么几乎每个方法都在一个事务里做两件事
// ===========================================================================
//
// 门店维度的每一条写路径都要 (store_id, region_id) 这一对，而 region_id 只能
// 从 stores 表读回来。把「读 scope」和「写」分成两次事务的话，中间的一次
// 「换大区」会让这次写落在**旧大区**的作用域里 —— 而大区是两层覆盖的外层，
// 结果是一个在后台看起来已经生效、买家却看不到的改动，没有任何东西会响。
//
// 所以下面凡是需要 scope 的地方，scope 与写都在同一个 WithTenant 回调里。
// 这条纪律的执行者是形状本身：repository 从不向上层暴露 pgx.Tx，
// 想「先解析再写」就必须开两个事务，而那件事在这个文件里一次都没有写出来。

// AdminStoreRepository 是本服务需要的仓储能力。
//
// **只有 WithTenant，没有 WithPlatform**，理由与 AdminCatalogRepository
// 一字不差：这 21 条全是商家级路径，门店与大区都挂在某一家店名下。
// 给它一个平台入口，等于让「以平台作用域建一家门店」成为一句写得出来的代码，
// 而那时 current_merchant() 是 NULL，stores.merchant_id 的 DEFAULT 取到 NULL，
// INSERT 以 NOT NULL 违例失败 —— 失败方向对，但错误里没有任何东西指向
// 「作用域选错了」。
type AdminStoreRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// AdminStoreService 实现那 21 条。
type AdminStoreService struct {
	repo AdminStoreRepository
	// inv 是库存服务（微服务拆分阶段 1a）：门店库存清单的水位与两条改库存都经它。
	inv inventory.Service
}

func NewAdminStoreService(r AdminStoreRepository, inv inventory.Service) *AdminStoreService {
	return &AdminStoreService{repo: r, inv: inv}
}

// ---------------------------------------------------------------------------
// 分页信封
// ---------------------------------------------------------------------------
//
// 四张列表各一个类型，而不是一个泛型的 Page[T]：Items 的元素类型不同，
// 而 AdminStorePage 还多一个 HasDefault —— 它不是分页信息，是契约在
// AdminStoreList 上要求必返的一个业务事实。

// RegionPage 是 GET /admin/regions 的一页。
type RegionPage struct {
	Items    []repository.Region
	Total    int64
	Page     int
	PageSize int
}

// AdminStorePage 是 GET /admin/stores 的一页。
//
// HasDefault 是「这家商家有没有默认门店」，契约把它定成必返。它**不是**
// 「本页里有没有默认门店」—— 默认门店可能在第三页上，而后台首页要据此决定
// 挂不挂那条提示：没有默认店时，所有未授权定位的访客都会拿到
// match_type = none（不在服务范围），而那看起来像「商品没上架」。
type AdminStorePage struct {
	Items      []repository.Store
	HasDefault bool
	Total      int64
	Page       int
	PageSize   int
}

// ScopedListingPage 是两条「作用域下的商品」列表的一页。
type ScopedListingPage struct {
	Items    []repository.ScopedListing
	Total    int64
	Page     int
	PageSize int
}

// StoreInventoryPage 是 GET /admin/stores/{store_id}/inventories 的一页。
type StoreInventoryPage struct {
	Items    []repository.StoreInventory
	Total    int64
	Page     int
	PageSize int
}

// ---------------------------------------------------------------------------
// 大区
// ---------------------------------------------------------------------------

// ListRegions 实现 GET /admin/regions。
func (s *AdminStoreService) ListRegions(ctx context.Context, page, pageSize int,
	includeDeleted bool) (RegionPage, error) {

	page, pageSize = clampPaging(page, pageSize)
	out := RegionPage{Items: []repository.Region{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 大区 / 门店管理员只看得见范围内的（authz.go 的 listFilter）。
		only, _, err := listFilter(ctx, tx)
		if err != nil {
			return err
		}
		items, total, err := tx.AdminListRegions(ctx, includeDeleted, only,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if err != nil {
			return err
		}
		out.Items, out.Total = items, total
		return nil
	})
	if err != nil {
		return RegionPage{}, err
	}
	return out, nil
}

// CreateRegion 实现 POST /admin/regions。
func (s *AdminStoreService) CreateRegion(ctx context.Context,
	n repository.NewRegion) (repository.Region, error) {

	// 大区管理员**不能建新大区**：建出来的大区不在他的范围里，而要让它在，
	// 就得有人给他扩范围 —— 那一步只有管理员能做，建大区也就只该由全店范围的人做。
	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.Region{}, err
	}
	if err := checkCode("code", n.Code); err != nil {
		return repository.Region{}, err
	}
	if err := checkName(n.Name); err != nil {
		return repository.Region{}, err
	}
	var out repository.Region
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.CreateRegion(ctx, n)
		return e
	})
	return out, err
}

// UpdateRegion 实现 PATCH /admin/regions/{region_id}。
func (s *AdminStoreService) UpdateRegion(ctx context.Context, id int64,
	p repository.RegionPatch) (repository.Region, error) {

	if _, err := authorizeRegion(ctx, id); err != nil {
		return repository.Region{}, err
	}
	// 一个字段都没传是 422，不是「原样返回 200」。契约在三条 PATCH 上都写着
	// minProperties: 1，而一次什么都不改的 PATCH 拿到 200，
	// 调用方会以为自己那个拼错名字的字段生效了。
	if p.Code == nil && p.Name == nil && p.Status == nil {
		return repository.Region{}, fmt.Errorf("%w: PATCH 至少要带一个字段", ErrCatalogBadRequest)
	}
	if p.Code != nil {
		if err := checkCode("code", *p.Code); err != nil {
			return repository.Region{}, err
		}
	}
	if p.Name != nil {
		if err := checkName(*p.Name); err != nil {
			return repository.Region{}, err
		}
	}
	if p.Status != nil {
		if err := checkStatus01(*p.Status); err != nil {
			return repository.Region{}, err
		}
	}
	var out repository.Region
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.UpdateRegion(ctx, id, p)
		return e
	})
	return out, err
}

// DeleteRegion 实现 DELETE /admin/regions/{region_id}（软删）。
//
// 名下还有未软删门店时 repository 回 ErrRegionHasStores（409）。**不做级联**：
// 级联软删一个大区会连带让它下面所有门店接不到单，而调用方在点下删除时
// 看到的只是一个大区名。
func (s *AdminStoreService) DeleteRegion(ctx context.Context, id int64) error {
	if _, err := authorizeRegion(ctx, id); err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.SoftDeleteRegion(ctx, id)
	})
}

// ---------------------------------------------------------------------------
// 门店
// ---------------------------------------------------------------------------

// ListStores 实现 GET /admin/stores。
func (s *AdminStoreService) ListStores(ctx context.Context, regionID *int64,
	includeDeleted bool, page, pageSize int) (AdminStorePage, error) {

	page, pageSize = clampPaging(page, pageSize)
	out := AdminStorePage{Items: []repository.Store{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		_, only, e := listFilter(ctx, tx)
		if e != nil {
			return e
		}
		items, total, hasDefault, e := tx.AdminListStores(ctx, regionID, includeDeleted, only,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if e != nil {
			return e
		}
		out.Items, out.Total, out.HasDefault = items, total, hasDefault
		return nil
	})
	if err != nil {
		return AdminStorePage{}, err
	}
	return out, nil
}

// FindStore 实现 GET /admin/stores/{store_id}。
func (s *AdminStoreService) FindStore(ctx context.Context, id int64) (repository.Store, error) {
	var out repository.Store
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := authorizeStore(ctx, tx, id, storeOperate); e != nil {
			return e
		}
		var e error
		out, e = tx.FindStore(ctx, id)
		return e
	})
	return out, err
}

// CreateStore 实现 POST /admin/stores。
//
// **围栏不在这里传**：建店与画围栏是两个人在两个时刻做的事，强制会把第一步
// 卡死在第二步上（契约在 AdminStoreList.has_default 上写着同一条推理）。
// 代价是「非默认店没有围栏」这个中间态合法，而它是一家永远接不到单的店 ——
// 后台列表据 fence = null 挂「未完成」提示，那是客户端的事。
func (s *AdminStoreService) CreateStore(ctx context.Context,
	n repository.NewStore) (repository.Store, error) {

	if _, err := authorizeNewStore(ctx, n.RegionID, n.IsDefault); err != nil {
		return repository.Store{}, err
	}
	if n.RegionID <= 0 {
		return repository.Store{}, fmt.Errorf("%w: region_id 必须是正整数", ErrCatalogBadRequest)
	}
	if err := checkCode("code", n.Code); err != nil {
		return repository.Store{}, err
	}
	if err := checkName(n.Name); err != nil {
		return repository.Store{}, err
	}
	if err := checkStoreText(n.Phone, n.Province, n.City, n.District, n.Address); err != nil {
		return repository.Store{}, err
	}
	if err := checkCoordPair(n.Lat, n.Lng); err != nil {
		return repository.Store{}, err
	}
	// 门店必须有坐标（2026-09-27）：后台建店用地图选点。建出来就没有坐标的店，
	// 之后画围栏时判不了「门店在不在围栏内」（repository.ErrStoreLocationRequired）。
	if n.Lat == nil || n.Lng == nil {
		return repository.Store{}, fmt.Errorf("%w: lat / lng 必填，门店必须有坐标", ErrCatalogBadRequest)
	}
	var out repository.Store
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.CreateStore(ctx, n)
		return e
	})
	return out, err
}

// UpdateStore 实现 PATCH /admin/stores/{store_id}。
//
// **改不了 fence 与 is_default**（StorePatch 上刻意没有这两个字段）：它们各有
// 自己的端点，因为它们不是普通字段 —— 一个要过 ST_IsValid，另一个要在同一个
// 事务里先清旧再置新。混进来就等于给那两条纪律留第二条绕过去的路。
func (s *AdminStoreService) UpdateStore(ctx context.Context, id int64,
	p repository.StorePatch) (repository.Store, error) {

	if p.RegionID == nil && p.Code == nil && p.Name == nil && p.Phone == nil &&
		p.Province == nil && p.City == nil && p.District == nil && p.Address == nil &&
		p.Status == nil && !p.SetLocation {
		return repository.Store{}, fmt.Errorf("%w: PATCH 至少要带一个字段", ErrCatalogBadRequest)
	}
	if p.RegionID != nil && *p.RegionID <= 0 {
		return repository.Store{}, fmt.Errorf("%w: region_id 必须是正整数", ErrCatalogBadRequest)
	}
	if p.Code != nil {
		if err := checkCode("code", *p.Code); err != nil {
			return repository.Store{}, err
		}
	}
	if p.Name != nil {
		if err := checkName(*p.Name); err != nil {
			return repository.Store{}, err
		}
	}
	if err := checkOptText("phone", p.Phone, 32); err != nil {
		return repository.Store{}, err
	}
	if err := checkOptText("province", p.Province, 64); err != nil {
		return repository.Store{}, err
	}
	if err := checkOptText("city", p.City, 64); err != nil {
		return repository.Store{}, err
	}
	if err := checkOptText("district", p.District, 64); err != nil {
		return repository.Store{}, err
	}
	if err := checkOptText("address", p.Address, 200); err != nil {
		return repository.Store{}, err
	}
	if p.Status != nil {
		if err := checkStatus01(*p.Status); err != nil {
			return repository.Store{}, err
		}
	}
	if p.SetLocation {
		if err := checkCoordPair(p.Lat, p.Lng); err != nil {
			return repository.Store{}, err
		}
	}
	var out repository.Store
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 换大区时新旧两个大区都得在范围内（authorizeStoreMove 的注释）。
		// 与写同一个事务：判权时读到的「旧大区」必须就是写的时候那一个。
		if _, e := authorizeStoreMove(ctx, tx, id, p.RegionID); e != nil {
			return e
		}
		var e error
		out, e = tx.UpdateStore(ctx, id, p)
		return e
	})
	return out, err
}

// DeleteStore 实现 DELETE /admin/stores/{store_id}（软删）。
//
// 硬删不掉：inventories / orders / inventory_logs 三张表对它有外键。
// 那不是这一层要处理的事 —— 软删是这条端点的语义，不是绕过外键的手段。
func (s *AdminStoreService) DeleteStore(ctx context.Context, id int64) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := authorizeStore(ctx, tx, id, storeManage); e != nil {
			return e
		}
		return tx.SoftDeleteStore(ctx, id)
	})
}

// SetFence 实现 PUT /admin/stores/{store_id}/fence。
//
// geojson 为 nil 即清空围栏。**清空只对默认门店合法**：一家非默认店没有围栏
// 就是一家永远接不到单的店。那条判定在 repository.SetStoreFence 里
// （ErrStoreFenceRequired → 409），**不是数据库约束** —— 做成约束的那一版
// 实测挡死了建普通店与切换默认店两条主路径，论证在 00020 里 stores 的定义上。
//
// 合法性由 PostGIS 的 ST_IsValid 判，不在这里自己写一遍：自交多边形在
// ST_Intersects 下行为未定义，而「哪里自交」这句话（ST_IsValidReason）
// 是运营唯一能拿来定位自己画错在哪儿的东西。
func (s *AdminStoreService) SetFence(ctx context.Context, id int64,
	geojson *string) (repository.Store, error) {

	var out repository.Store
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := authorizeStore(ctx, tx, id, storeManage); e != nil {
			return e
		}
		var e error
		out, e = tx.SetStoreFence(ctx, id, geojson)
		return e
	})
	return out, err
}

// MakeDefault 实现 PUT /admin/stores/{store_id}/default。
func (s *AdminStoreService) MakeDefault(ctx context.Context, id int64) (repository.Store, error) {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return repository.Store{}, err
	}
	var out repository.Store
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.MakeStoreDefault(ctx, id)
		return e
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 两个作用域下的可见性
// ---------------------------------------------------------------------------

// ListStoreProducts 实现 GET /admin/stores/{store_id}/products。
//
// listed 为 nil 即两者都要 —— 契约在那个参数上刻意没有 default，
// 因为缺省被代入会静默改变「返回哪些行」。
func (s *AdminStoreService) ListStoreProducts(ctx context.Context, storeID int64,
	listed *bool, page, pageSize int) (ScopedListingPage, error) {

	page, pageSize = clampPaging(page, pageSize)
	out := ScopedListingPage{Items: []repository.ScopedListing{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := authorizeStore(ctx, tx, storeID, storeOperate); e != nil {
			return e
		}
		// scope 与读在同一个事务里：分成两次的话，中间的一次「换大区」会让
		// 这一页按旧大区算可见性与价格，而大区是两层覆盖的外层。
		_, regionID, e := tx.StoreScope(ctx, storeID)
		if e != nil {
			return e
		}
		items, total, e := tx.ListStoreProducts(ctx, storeID, regionID, listed,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if e != nil {
			return e
		}
		out.Items, out.Total = items, total
		return nil
	})
	if err != nil {
		return ScopedListingPage{}, err
	}
	return out, nil
}

// ListRegionProducts 实现 GET /admin/regions/{region_id}/products。
func (s *AdminStoreService) ListRegionProducts(ctx context.Context, regionID int64,
	listed *bool, page, pageSize int) (ScopedListingPage, error) {

	if _, err := authorizeRegion(ctx, regionID); err != nil {
		return ScopedListingPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	out := ScopedListingPage{Items: []repository.ScopedListing{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 大区这一侧要先确认它存在：契约把「大区不存在或不属于当前租户」
		// 定成 404，而 ListRegionProducts 对一个不存在的 region_id
		// 只会返回空页 —— 那看起来像「这个大区什么都不卖」。
		if _, e := tx.FindRegion(ctx, regionID); e != nil {
			return e
		}
		items, total, e := tx.ListRegionProducts(ctx, regionID, listed,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if e != nil {
			return e
		}
		out.Items, out.Total = items, total
		return nil
	})
	if err != nil {
		return ScopedListingPage{}, err
	}
	return out, nil
}

// SetStoreListing 实现 PUT /admin/stores/{store_id}/products/{product_id}/listing。
//
// 两张 overrides 表都是**排除表**：缺一行即在售（「开店即营业」）。所以
// listed = true 是删那一行，listed = false 是插一行 —— 而**大区排掉的，
// 门店这一层捞不回来**（effective_listed 是两层的与，不是或）。
func (s *AdminStoreService) SetStoreListing(ctx context.Context, storeID, productID int64,
	listed bool) (repository.ScopedListing, error) {

	var out repository.ScopedListing
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		staff, e := authorizeStore(ctx, tx, storeID, storeOperate)
		if e != nil {
			return e
		}
		_, regionID, e := tx.StoreScope(ctx, storeID)
		if e != nil {
			return e
		}
		out, e = tx.SetStoreProductListing(ctx, storeID, regionID, productID, listed,
			operatorOf(staff))
		return e
	})
	return out, err
}

// SetRegionListing 实现 PUT /admin/regions/{region_id}/products/{product_id}/listing。
func (s *AdminStoreService) SetRegionListing(ctx context.Context, regionID, productID int64,
	listed bool) (repository.ScopedListing, error) {

	staff, err := authorizeRegion(ctx, regionID)
	if err != nil {
		return repository.ScopedListing{}, err
	}
	var out repository.ScopedListing
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := tx.FindRegion(ctx, regionID); e != nil {
			return e
		}
		var e error
		out, e = tx.SetRegionProductListing(ctx, regionID, productID, listed, operatorOf(staff))
		return e
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 三层定价的内两层
// ---------------------------------------------------------------------------

// SetStorePrice 实现 PUT /admin/stores/{store_id}/skus/{sku_id}/price。
func (s *AdminStoreService) SetStorePrice(ctx context.Context, storeID, skuID, cents int64) (
	repository.ScopedPrice, error) {

	// 非负同时由 chk_store_price_nonneg 兜底。两道都要：数据库那道挡的是
	// 任何路径，这一道给的是一句能读懂的话（契约的 422）。
	if err := checkNonNeg("price_cents", cents); err != nil {
		return repository.ScopedPrice{}, err
	}
	var out repository.ScopedPrice
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := authorizeStore(ctx, tx, storeID, storeOperate); e != nil {
			return e
		}
		var e error
		out, e = tx.SetStorePrice(ctx, storeID, skuID, cents)
		return e
	})
	return out, err
}

// ClearStorePrice 实现 DELETE /admin/stores/{store_id}/skus/{sku_id}/price。
//
// **本来就没有那一行时同样返回 204，不是 404**：调用方的意图是「这家店不要
// 自己的价」，那个意图在两种情况下都已经达成。404 留给「门店或 SKU 根本不
// 存在」，而那一条由 repository 在删之前单独确认。
func (s *AdminStoreService) ClearStorePrice(ctx context.Context, storeID, skuID int64) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := authorizeStore(ctx, tx, storeID, storeOperate); e != nil {
			return e
		}
		// 先确认这两个 id 真的存在 —— **这一步不能省，而且只能在这里做**。
		//
		// 那条 DELETE 对「本来就没有那一行」与「门店根本不存在」返回的东西
		// 一模一样（都是 0 行，都成功），而契约把两者分成 204 与 404。
		// 少了这一步，`DELETE /admin/stores/99999/skus/1/price` 会回 204 ——
		// 调用方会以为自己撤销了一家并不存在的门店的价格，
		// 而它下一次读回来仍然是老价。
		if _, _, e := tx.StoreScope(ctx, storeID); e != nil {
			return e
		}
		if _, e := tx.AdminFindSKU(ctx, skuID); e != nil {
			return e
		}
		return tx.ClearStorePrice(ctx, storeID, skuID)
	})
}

// SetRegionPrice 实现 PUT /admin/regions/{region_id}/skus/{sku_id}/price。
func (s *AdminStoreService) SetRegionPrice(ctx context.Context, regionID, skuID, cents int64) (
	repository.ScopedPrice, error) {

	if _, err := authorizeRegion(ctx, regionID); err != nil {
		return repository.ScopedPrice{}, err
	}
	if err := checkNonNeg("price_cents", cents); err != nil {
		return repository.ScopedPrice{}, err
	}
	var out repository.ScopedPrice
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.SetRegionPrice(ctx, regionID, skuID, cents)
		return e
	})
	return out, err
}

// ClearRegionPrice 实现 DELETE /admin/regions/{region_id}/skus/{sku_id}/price。
//
// 撤销大区价回到基准价，**不动门店价**：门店那一层如果自己定了价，
// 撤销大区价之后它仍然生效。覆盖是逐层独立的。
func (s *AdminStoreService) ClearRegionPrice(ctx context.Context, regionID, skuID int64) error {
	if _, err := authorizeRegion(ctx, regionID); err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 同 ClearStorePrice：DELETE 分不开「没有覆盖」与「大区不存在」，
		// 而契约把它们分成 204 与 404。
		if _, e := tx.FindRegion(ctx, regionID); e != nil {
			return e
		}
		if _, e := tx.AdminFindSKU(ctx, skuID); e != nil {
			return e
		}
		return tx.ClearRegionPrice(ctx, regionID, skuID)
	})
}

// ---------------------------------------------------------------------------
// 按门店的库存
// ---------------------------------------------------------------------------

// ListStoreInventories 实现 GET /admin/stores/{store_id}/inventories。
//
// **缺行要显示成 0，不能漏掉**：一家刚开的店在录库存之前每个 SKU 都缺行，
// 漏掉它们会让后台看起来「这家店一个 SKU 都没有」。驱动是 core 的 SKU 分页，
// 水位由库存服务补（缺行记 0），见 inventory_admin.go 的 listStoreInventories。
func (s *AdminStoreService) ListStoreInventories(ctx context.Context, storeID int64,
	lowStockOnly bool, page, pageSize int) (StoreInventoryPage, error) {
	return listStoreInventories(ctx, s.repo, s.inv, storeID, lowStockOnly, page, pageSize)
}

// SetStoreInventory 实现 PUT /admin/stores/{store_id}/skus/{sku_id}/inventory。
//
// CAS 的条件里**不能漏 store_id** —— 漏了会一次改掉该 SKU 在所有门店的行。
// 那件事由库存服务的 SQL 保证（InvSetStock 的 WHERE 里两列都在），
// 这一层负责把两个数量校到非负：chk_qty_nonneg 兜不住负的 expected，
// 而一个负的 expected 永远匹配不上任何一行，症状是「怎么改都 409」——
// 而 409 的含义是「重读一次再试就能成功」，于是调用方会一直试下去。
func (s *AdminStoreService) SetStoreInventory(ctx context.Context, storeID, skuID int64,
	in repository.InventorySet) (repository.StoreInventory, error) {

	if err := checkNonNeg("available_qty", int64(in.AvailableQty)); err != nil {
		return repository.StoreInventory{}, err
	}
	if err := checkNonNeg("expected_available_qty", int64(in.ExpectedAvailableQty)); err != nil {
		return repository.StoreInventory{}, err
	}
	if in.WarningQty != nil {
		if err := checkNonNeg("warning_qty", int64(*in.WarningQty)); err != nil {
			return repository.StoreInventory{}, err
		}
	}
	bizID, err := inventorySetBizID(ctx)
	if err != nil {
		return repository.StoreInventory{}, err
	}
	in.BizID = bizID
	return setStockInStore(ctx, s.repo, s.inv, storeID, skuID, in)
}

// InventoryAdjustInput 是契约 InventoryAdjustRequest 在业务层的形状。
type InventoryAdjustInput struct {
	Delta  int32   `json:"delta"`
	Reason *string `json:"reason,omitempty"`
}

// 相对调整的边界（契约 InventoryAdjustRequest）。
const (
	maxInventoryAdjustDelta  = 1_000_000
	maxInventoryAdjustReason = 200
)

// AdjustStoreInventory 实现 POST /admin/stores/{store_id}/skus/{sku_id}/inventory/adjustments。
// 返回的 bool 为真表示幂等重放。业务全在 inventory_admin.go 的 adjustInventory。
func (s *AdminStoreService) AdjustStoreInventory(ctx context.Context, storeID, skuID int64,
	in InventoryAdjustInput, idemKey string) (repository.StoreInventory, bool, error) {
	return adjustInventory(ctx, s.repo, s.inv, storeID, skuID, in, idemKey)
}

// ---------------------------------------------------------------------------
// 校验小工具
// ---------------------------------------------------------------------------
//
// 长度一律按**字符**数，不按字节，理由与 admin_catalog.go 那一组一字不差：
// 契约里 maxLength 说的是 JSON 字符串的长度，而 64 个汉字在 UTF-8 里是
// 192 字节。

// operatorOf 取「是谁下的架」，落进 *_product_overrides.updated_by。
//
// 返回指针而不是裸 int64：那两列可空，而 0 不是一个可空 id 的合法表达 ——
// 它会在后台的操作审计里显示成一个不存在的员工。这里 StaffID 恒非零
// （requireStaff 已经保证请求带着一个后台会话），但类型上留住 nil
// 这条路，是为了让「平台级脚本改的」那天有一个诚实的落点。
//
// 只有下架那一支用得上它：上架是**删掉排除表里那一行**，删掉之后没有任何一行
// 可以记「是谁上的架」。这处不对称是表设计决定的（缺一行即在售），
// 不是漏了 —— 真要记上架审计，那是 inventory_logs 那种独立流水表的事。
func operatorOf(staff auth.StaffIdentity) *int64 {
	id := staff.StaffID
	return &id
}

func checkCode(field, v string) error {
	n := utf8.RuneCountInString(strings.TrimSpace(v))
	if n == 0 {
		return fmt.Errorf("%w: %s 不能为空", ErrCatalogBadRequest, field)
	}
	if n > 64 {
		return fmt.Errorf("%w: %s 有 %d 个字，契约上限是 64", ErrCatalogBadRequest, field, n)
	}
	return nil
}

// checkStatus01 挡住枚举外的取值。契约把 status 定成 enum: [0, 1]。
//
// 不靠数据库兜底：stores.status / regions.status 上没有 CHECK（一个 smallint
// 装得下 2），所以一个 status = 7 会安静地落库，而它既不是营业也不是停业 ——
// 围栏判定里 `status = 1` 会把它当成停业，后台列表会把它显示成一个未知状态。
func checkStatus01(v int16) error {
	if v != 0 && v != 1 {
		return fmt.Errorf("%w: status 是 %d，契约只允许 0（停用）或 1（启用）",
			ErrCatalogBadRequest, v)
	}
	return nil
}

func checkStoreText(phone, province, city, district, address string) error {
	for _, f := range []struct {
		name string
		v    string
		max  int
	}{
		{"phone", phone, 32}, {"province", province, 64}, {"city", city, 64},
		{"district", district, 64}, {"address", address, 200},
	} {
		if n := utf8.RuneCountInString(f.v); n > f.max {
			return fmt.Errorf("%w: %s 有 %d 个字，契约上限是 %d",
				ErrCatalogBadRequest, f.name, n, f.max)
		}
	}
	return nil
}

// checkCoordPair 挡住「只给了一半坐标」与越界。
//
// 两个值同时给或同时不给，与 /stores/resolve 的 lat/lng 是同一条规矩：
// 只给 lat 的话 stores.location 会被拼成一个 (0, lat) 的点 ——
// 那是几内亚湾上的一个真实坐标，而 ST_Distance 会照常算出一个距离，
// 于是这家店在按距离排序里永远排在最后，看起来完全正常。
func checkCoordPair(lat, lng *float64) error {
	if (lat == nil) != (lng == nil) {
		return fmt.Errorf("%w: lat 与 lng 必须同时给或同时不给", ErrCatalogBadRequest)
	}
	if lat == nil {
		return nil
	}
	if *lat < -90 || *lat > 90 {
		return fmt.Errorf("%w: lat 是 %v，超出 [-90, 90]", ErrCatalogBadRequest, *lat)
	}
	if *lng < -180 || *lng > 180 {
		return fmt.Errorf("%w: lng 是 %v，超出 [-180, 180]", ErrCatalogBadRequest, *lng)
	}
	return nil
}

// inventorySetBizID 给一次比较并设置（PUT .../inventory）拼流水的 biz_id：
// 「set:<staff_id>:<16 位随机十六进制>」。PUT 天然幂等、不收 Idempotency-Key，
// 所以「哪一次」只能现取一个随机串 —— 它只需要让两次覆盖在流水里分得开；
// 「谁」是员工 id，与相对调整的「adj:<staff_id>:<Idempotency-Key>」同一个形状。
func inventorySetBizID(ctx context.Context) (string, error) {
	staff, err := requireStaff(ctx)
	if err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("取随机串失败: %w", err)
	}
	return fmt.Sprintf("set:%d:%s", staff.StaffID, hex.EncodeToString(b[:])), nil
}
