package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 购物车（/cart，数据模型 §10）。
//
// # 价格：与试算共用同一条定价查询
//
// 车里不存价格（§10「购物车不存价格快照」）。展示价在读的时候现算，而且**走的是
// priceOrder 用的那一条** tx.ListSKUsForPricing —— 同一个视图、同样的两条门店 /
// 大区排除、同样的四个在架条件。于是「购物车显示的价」与「/orders/preview 试算
// 的价」在同一家店上不可能不一样：没有第二份实现可以跑偏（pricing.go 文件头那条
// 纪律，边界从「试算与下单」扩到「购物车、试算与下单」）。
//
// 由此也得出了 not_sold_in_store 的判法：定价查询没返回、但这一行在架 ——
// 那只能是门店或大区把它排除了。排除规则在定价查询里只有一份，这里不再写一遍。
//
// # 门店：与读接口同一条解析链
//
// 「多少钱、有没有货、卖不卖」都按门店分，所以每一条返回 Cart 的接口都收 store_id，
// 解析走 scopeIn —— 与 /products、/search 逐字同一段代码（不传走回落链，指名一家
// 不存在的门店报 422）。
//
// # 失效行不删
//
// 下架、删除、这家店不卖、缺货的行都留在车里，按 CartLineStatus 标出来（§10：
// 「替用户默默删东西，比让他看到一条划掉的商品更讨人嫌」）。合计只算此刻买得到的行。

// CartLineStatus 是契约 CartItemStatus。判定顺序即常量顺序，先命中的先报。
type CartLineStatus string

const (
	CartLineOffShelf          CartLineStatus = "off_shelf"
	CartLineNotSoldInStore    CartLineStatus = "not_sold_in_store"
	CartLineOutOfStock        CartLineStatus = "out_of_stock"
	CartLineInsufficientStock CartLineStatus = "insufficient_stock"
	CartLineAvailable         CartLineStatus = "available"
)

// 购物车的两个上限。
//
//   - maxCartQuantity 是契约写死的 999（累加后超过返回 422 cart-quantity-exceeded），
//     与 priceOrder 的 maxLineQuantity 同一个数：车里能放的数量必须是下得了单的数量，
//     否则用户会带着一行 1000 件走到结算页才被拒。
//   - maxCartLines 挡的是「一辆车无限长」：每次读车都要对全部行跑一次定价查询，
//     §10 的推理（「购物车行数天然很少，几十行到头了」）需要一个执行者。
const (
	maxCartQuantity = maxLineQuantity
	maxCartLines    = 100
)

var (
	// ErrCartItemNotFound：条目不在当前买家的车里（含别人车里的条目）。404。
	ErrCartItemNotFound = errors.New("购物车条目不存在")

	// ErrCartQuantityExceeded：累加后超过 999。契约 422 cart-quantity-exceeded，
	// detail 给出当前数量与上限。**不做静默截断**。
	ErrCartQuantityExceeded = errors.New("购物车里这件商品的数量超过上限")

	// ErrCartFull：车里已有 maxCartLines 种商品。422 invalid-request。
	ErrCartFull = errors.New("购物车已满")
)

// CartRepository 是本服务需要的仓储能力。
type CartRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// CartService 实现 /cart 那 7 个操作。
type CartService struct{ repo CartRepository }

func NewCartService(r CartRepository) *CartService { return &CartService{repo: r} }

// 幂等作用域（POST /cart/items 与 POST /cart/items/batch-delete 都声明了必填的
// Idempotency-Key）。每条一个，理由同 admin_idempotency.go 那一段。
const (
	scopeCartAdd         = "cart.items.add"
	scopeCartBatchDelete = "cart.items.batch-delete"
)

// CartLineView 是响应里的一行。
type CartLineView struct {
	repository.CartLine

	// PriceCents 是这家店此刻的生效价；Status 为 off_shelf / not_sold_in_store 时为 nil。
	PriceCents *int64
	Status     CartLineStatus

	// Undeliverable 非 nil 表示这一行送不到 CartView.AddressID 那个地址（00056）。
	// 只对 available 的行判：别的行此刻本来就买不了。
	Undeliverable *FreightUndeliverable
}

// CartView 是一整辆车（契约 Cart）。它也是幂等存档里存的东西，所以字段全部导出。
type CartView struct {
	Lines              []CartLineView
	TotalCents         int64
	SelectedTotalCents int64
	Store              StoreContext

	// AddressID / Freight：按哪个收货地址算的预估运费（00056）。没有地址时都为 nil ——
	// 没有地址就没有运费可算，那不是「包邮」。
	AddressID *int64
	Freight   *FreightBreakdown
}

// cartScope 是一次请求解析出来的门店与收货地址。MatchNone 时 Scope 为零值；
// 没有地址（没指名、也没有默认地址）时 AddressID / Dest 为 nil。
type cartScope struct {
	Scope repository.StoreScope
	Match MatchType

	AddressID *int64
	Dest      *FreightDestination
}

// resolveCartScope 走与读接口同一段 scopeIn。不在服务范围不是错误：读车照样
// 回一辆车（每一行都不可买），由需要门店的写操作自己决定报不报错。
//
// 收货地址（00056，契约 CartAddressId）：指名了就用它（不存在或不是你的 → 422，
// 不静默改用默认地址），没指名用默认地址，都没有就不算运费。
func resolveCartScope(ctx context.Context, tx repository.Tx, userID int64,
	storeID, addressID *int64) (cartScope, error) {
	aid, dest, err := buyerDestination(ctx, tx, userID, addressID)
	if err != nil {
		return cartScope{}, err
	}
	sc, mt, err := scopeIn(ctx, tx, storeID)
	if errors.Is(err, ErrOutOfServiceArea) {
		return cartScope{Match: MatchNone, AddressID: aid, Dest: dest}, nil
	}
	if err != nil {
		return cartScope{}, err
	}
	return cartScope{Scope: sc, Match: mt, AddressID: aid, Dest: dest}, nil
}

func (cs cartScope) requireStore() error {
	if cs.Match == MatchNone {
		return ErrOutOfServiceArea
	}
	return nil
}

// buildCart 读出整辆车并标注每一行。cartID 为 0 表示这个买家还没有车。
func buildCart(ctx context.Context, tx repository.Tx, cartID int64, cs cartScope) (CartView, error) {
	out := CartView{Lines: []CartLineView{}, Store: storeContextOf(cs.Scope, cs.Match)}
	if cartID == 0 {
		return withCartFreight(ctx, tx, out, cs)
	}
	lines, err := tx.ListCartLines(ctx, cartID, cs.Scope.StoreID)
	if err != nil {
		return CartView{}, err
	}
	if len(lines) == 0 {
		return withCartFreight(ctx, tx, out, cs)
	}

	// 定价：与 priceOrder 同一条查询、同一家店。只问在架的那些 —— 失效行问了也是
	// 白问（定价查询按同样的四个条件把它们滤掉）。
	priced := map[int64]int64{}
	if cs.Match != MatchNone {
		ids := make([]int64, 0, len(lines))
		for _, ln := range lines {
			if ln.OnShelf {
				ids = append(ids, ln.SKUID)
			}
		}
		if len(ids) > 0 {
			rows, err := tx.ListSKUsForPricing(ctx, cs.Scope, ids)
			if err != nil {
				return CartView{}, err
			}
			for _, r := range rows {
				priced[r.ID] = r.PriceCents
			}
		}
	}

	for _, ln := range lines {
		v := CartLineView{CartLine: ln}
		price, ok := priced[ln.SKUID]
		switch {
		case !ln.OnShelf:
			v.Status = CartLineOffShelf
		case !ok:
			// 在架、却没有价：只能是这家店或它所在大区把它排除了（或者根本没有门店）。
			v.Status = CartLineNotSoldInStore
		case ln.AvailableQty <= 0:
			v.Status = CartLineOutOfStock
		case ln.AvailableQty < ln.Quantity:
			v.Status = CartLineInsufficientStock
		default:
			v.Status = CartLineAvailable
		}
		if ok && v.Status != CartLineOffShelf {
			p := price
			v.PriceCents = &p
		}
		if v.Status == CartLineAvailable {
			amount := price * int64(ln.Quantity)
			out.TotalCents += amount
			if ln.Selected {
				out.SelectedTotalCents += amount
			}
		}
		out.Lines = append(out.Lines, v)
	}
	return withCartFreight(ctx, tx, out, cs)
}

// withCartFreight 给一辆车标上「送不送得到」并算预估运费（00056，契约 Cart.freight）。
//
// 与试算走同一份实现（freightContext.quote），两遍：
//  1. 全部可买的行过一遍，挑出送不到的、给行打标（不只看勾选的：用户要在勾选之前就看得到）；
//  2. 已勾选、可买、送得到的行按它们的金额合计算运费 —— 购物车不算券，所以满额包邮比的是
//     「用券之前」的金额，最终以 /orders/preview 为准（契约写明）。
func withCartFreight(ctx context.Context, tx repository.Tx, out CartView, cs cartScope) (CartView, error) {
	if cs.Dest == nil {
		return out, nil
	}
	var all []FreightItem
	for _, ln := range out.Lines {
		if ln.Status == CartLineAvailable {
			all = append(all, FreightItem{SKUID: ln.SKUID, Quantity: ln.Quantity})
		}
	}
	ids := make([]int64, 0, len(all))
	for _, it := range all {
		ids = append(ids, it.SKUID)
	}
	fc, err := loadFreightContext(ctx, tx, cs.Scope, ids)
	if err != nil {
		return CartView{}, err
	}
	_, bad, err := fc.quote(cs.Dest.ProvinceCode, all, 0)
	if err != nil {
		return CartView{}, err
	}
	badBySKU := make(map[int64]FreightUndeliverable, len(bad))
	for _, b := range bad {
		badBySKU[b.SKUID] = b
	}
	var sel []FreightItem
	var goods int64
	for i := range out.Lines {
		ln := &out.Lines[i]
		if ln.Status != CartLineAvailable {
			continue
		}
		if b, ok := badBySKU[ln.SKUID]; ok {
			b := b
			ln.Undeliverable = &b
			continue
		}
		if ln.Selected && ln.PriceCents != nil {
			sel = append(sel, FreightItem{SKUID: ln.SKUID, Quantity: ln.Quantity})
			goods += *ln.PriceCents * int64(ln.Quantity)
		}
	}
	b, _, err := fc.quote(cs.Dest.ProvinceCode, sel, goods)
	if err != nil {
		return CartView{}, err
	}
	out.AddressID = cs.AddressID
	out.Freight = &b
	return out, nil
}

// Get 实现 GET /cart。
func (s *CartService) Get(ctx context.Context, storeID, addressID *int64) (CartView, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return CartView{}, err
	}
	var out CartView
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		cs, err := resolveCartScope(ctx, tx, id.UserID, storeID, addressID)
		if err != nil {
			return err
		}
		cartID, err := tx.FindCart(ctx, id.UserID)
		if errors.Is(err, repository.ErrCartNotFound) {
			cartID, err = 0, nil
		}
		if err != nil {
			return err
		}
		out, err = buildCart(ctx, tx, cartID, cs)
		return err
	})
	return out, err
}

// AddRequest 是 POST /cart/items。StoreID 来自 query，也进幂等哈希：
// 同一个请求体按两家店算是两个不同的请求（响应里的价不一样）。
type AddRequest struct {
	StoreID *int64
	// AddressID 同 StoreID：来自 query、进幂等哈希（响应里的运费按它算）。
	AddressID *int64
	SKUID     int64
	Quantity  int32
}

// Add 实现 POST /cart/items。返回的 bool 为真表示幂等重放。
func (s *CartService) Add(ctx context.Context, req AddRequest, idemKey string) (CartView, bool, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return CartView{}, false, err
	}
	if req.SKUID <= 0 {
		return CartView{}, false, fmt.Errorf("%w: sku_id 必须为正", ErrBadRequest)
	}
	if req.Quantity < 1 || req.Quantity > maxCartQuantity {
		return CartView{}, false, fmt.Errorf("%w: quantity 必须在 [1, %d] 内", ErrBadRequest, maxCartQuantity)
	}
	hash, err := adminRequestHash(nil, req)
	if err != nil {
		return CartView{}, false, err
	}
	return idempotentTenantWrite(ctx, s.repo, scopeCartAdd, repository.BuyerSubject(id.UserID),
		idemKey, hash, archivedOK, func(tx repository.Tx) (CartView, error) {
			cs, err := resolveCartScope(ctx, tx, id.UserID, req.StoreID, req.AddressID)
			if err != nil {
				return CartView{}, err
			}
			if err := cs.requireStore(); err != nil {
				return CartView{}, err
			}

			// 卖不卖：先判在架，再问定价查询（与 buildCart 的判定顺序一致）。
			sku, err := tx.FindSKUForCart(ctx, req.SKUID, cs.Scope.StoreID)
			if errors.Is(err, repository.ErrSKUNotFound) {
				return CartView{}, fmt.Errorf("%w: sku %d", ErrSKUUnavailable, req.SKUID)
			}
			if err != nil {
				return CartView{}, err
			}
			if !sku.OnShelf {
				return CartView{}, fmt.Errorf("%w: sku %d 已停售或已下架", ErrSKUUnavailable, req.SKUID)
			}
			rows, err := tx.ListSKUsForPricing(ctx, cs.Scope, []int64{req.SKUID})
			if err != nil {
				return CartView{}, err
			}
			if len(rows) == 0 {
				return CartView{}, fmt.Errorf("%w: sku %d @ store %d",
					repository.ErrSKUNotSoldInStore, req.SKUID, cs.Scope.StoreID)
			}

			cartID, err := tx.EnsureCart(ctx, id.UserID)
			if err != nil {
				return CartView{}, err
			}
			cur, err := tx.CartLineQuantity(ctx, cartID, req.SKUID)
			if err != nil {
				return CartView{}, err
			}
			if cur+req.Quantity > maxCartQuantity {
				return CartView{}, quantityExceeded(cur, req.Quantity)
			}
			if cur == 0 {
				n, err := tx.CountCartLines(ctx, cartID)
				if err != nil {
					return CartView{}, err
				}
				if n >= maxCartLines {
					return CartView{}, fmt.Errorf("%w: 至多 %d 种商品", ErrCartFull, maxCartLines)
				}
			}
			// 够不够：按累加后的数量比这家店的可售量。这只是加购这一刻的判断，
			// 不是预留 —— 真正的判定点仍是下单时 SAGA 的库存分支（架构 §5）。
			if cur+req.Quantity > sku.AvailableQty {
				return CartView{}, fmt.Errorf("%w: 这家店可售 %d 件，车里已有 %d 件，再加 %d 件不够",
					ErrInsufficientStock, sku.AvailableQty, cur, req.Quantity)
			}
			if err := tx.AddCartLine(ctx, cartID, req.SKUID, sku.ProductID, req.Quantity); err != nil {
				if errors.Is(err, repository.ErrCartLineQuantityCap) {
					// 并发的另一次加购在我们读 cur 之后提交了：upsert 自己的条件兜住了。
					return CartView{}, quantityExceeded(cur, req.Quantity)
				}
				return CartView{}, err
			}
			return buildCart(ctx, tx, cartID, cs)
		})
}

func quantityExceeded(cur, add int32) error {
	return fmt.Errorf("%w: 车里已有 %d 件，再加 %d 件超过上限 %d",
		ErrCartQuantityExceeded, cur, add, maxCartQuantity)
}

// PatchRequest 是 PATCH /cart/items/{item_id}。
type PatchRequest struct {
	StoreID   *int64
	AddressID *int64
	ItemID    int64
	Quantity  *int32
	Selected  *bool
}

// Patch 实现 PATCH /cart/items/{item_id}。
//
// 调大数量才判库存；调小永远放行（契约原话：「车里 5 件、店里只剩 2 件时，用户把 5 改成 4
// 不该被拒」）。
func (s *CartService) Patch(ctx context.Context, req PatchRequest) (CartView, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return CartView{}, err
	}
	if req.Quantity == nil && req.Selected == nil {
		return CartView{}, fmt.Errorf("%w: quantity 与 selected 至少给一个", ErrBadRequest)
	}
	if req.Quantity != nil && (*req.Quantity < 1 || *req.Quantity > maxCartQuantity) {
		return CartView{}, fmt.Errorf("%w: quantity 必须在 [1, %d] 内", ErrBadRequest, maxCartQuantity)
	}
	var out CartView
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		cs, err := resolveCartScope(ctx, tx, id.UserID, req.StoreID, req.AddressID)
		if err != nil {
			return err
		}
		cartID, err := s.ownCart(ctx, tx, id.UserID)
		if err != nil {
			return err
		}
		line, err := tx.FindCartLine(ctx, cartID, req.ItemID)
		if errors.Is(err, repository.ErrCartLineNotFound) {
			return ErrCartItemNotFound
		}
		if err != nil {
			return err
		}
		if req.Quantity != nil && *req.Quantity > line.Quantity {
			if err := cs.requireStore(); err != nil {
				return err
			}
			sku, err := tx.FindSKUForCart(ctx, line.SKUID, cs.Scope.StoreID)
			if err != nil {
				return err
			}
			if *req.Quantity > sku.AvailableQty {
				return fmt.Errorf("%w: 这家店可售 %d 件，要改成 %d 件不够",
					ErrInsufficientStock, sku.AvailableQty, *req.Quantity)
			}
		}
		if err := tx.UpdateCartLine(ctx, cartID, req.ItemID, req.Quantity, req.Selected); err != nil {
			if errors.Is(err, repository.ErrCartLineNotFound) {
				return ErrCartItemNotFound
			}
			return err
		}
		out, err = buildCart(ctx, tx, cartID, cs)
		return err
	})
	return out, err
}

// ownCart 取这个买家的车；没有车时，任何「指名一个条目」的操作都是 404。
func (s *CartService) ownCart(ctx context.Context, tx repository.Tx, userID int64) (int64, error) {
	cartID, err := tx.FindCart(ctx, userID)
	if errors.Is(err, repository.ErrCartNotFound) {
		return 0, ErrCartItemNotFound
	}
	return cartID, err
}

// dedupIDs 去重并排序。请求里同一个 id 写两遍不算「多一个不存在的」。
func dedupIDs(in []int64) []int64 {
	seen := make(map[int64]bool, len(in))
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Select 实现 PUT /cart/selection。itemIDs 为 nil 即全车。
//
// 给了 item_ids 时要么全改、要么一行不改：只要有一个不在这辆车里就回滚并 404
// （契约：item_ids 中存在不属于当前用户购物车的条目）。
func (s *CartService) Select(ctx context.Context, storeID, addressID *int64, selected bool,
	itemIDs *[]int64) (CartView, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return CartView{}, err
	}
	var out CartView
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		cs, err := resolveCartScope(ctx, tx, id.UserID, storeID, addressID)
		if err != nil {
			return err
		}
		cartID, err := tx.FindCart(ctx, id.UserID)
		if errors.Is(err, repository.ErrCartNotFound) {
			if itemIDs != nil && len(*itemIDs) > 0 {
				return ErrCartItemNotFound
			}
			out, err = buildCart(ctx, tx, 0, cs)
			return err
		}
		if err != nil {
			return err
		}
		if itemIDs == nil {
			if err := tx.SetAllCartSelection(ctx, cartID, selected); err != nil {
				return err
			}
		} else if ids := dedupIDs(*itemIDs); len(ids) > 0 {
			got, err := tx.SetCartSelection(ctx, cartID, ids, selected)
			if err != nil {
				return err
			}
			if len(got) != len(ids) {
				return fmt.Errorf("%w: 请求 %d 个，车里只有其中 %d 个", ErrCartItemNotFound, len(ids), len(got))
			}
		}
		out, err = buildCart(ctx, tx, cartID, cs)
		return err
	})
	return out, err
}

// BatchDeleteRequest 是 POST /cart/items/batch-delete。
type BatchDeleteRequest struct {
	StoreID   *int64
	AddressID *int64
	ItemIDs   *[]int64
	Selected  *bool
}

// BatchDelete 实现 POST /cart/items/batch-delete。返回的 bool 为真表示幂等重放。
func (s *CartService) BatchDelete(ctx context.Context, req BatchDeleteRequest, idemKey string) (CartView, bool, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return CartView{}, false, err
	}
	switch {
	case req.ItemIDs != nil && req.Selected != nil:
		return CartView{}, false, fmt.Errorf("%w: item_ids 与 selected 互斥", ErrBadRequest)
	case req.ItemIDs == nil && req.Selected == nil:
		return CartView{}, false, fmt.Errorf("%w: item_ids 与 selected 必须给一个", ErrBadRequest)
	case req.Selected != nil && !*req.Selected:
		// 「selected: false」读起来像「删掉没勾选的」，而契约只定义了 true。
		// 猜一个意思去删用户的东西，比报错糟得多。
		return CartView{}, false, fmt.Errorf("%w: selected 只能是 true", ErrBadRequest)
	case req.ItemIDs != nil && len(*req.ItemIDs) == 0:
		return CartView{}, false, fmt.Errorf("%w: item_ids 不能是空数组", ErrBadRequest)
	}
	norm := req
	if req.ItemIDs != nil {
		ids := dedupIDs(*req.ItemIDs)
		norm.ItemIDs = &ids
	}
	hash, err := adminRequestHash(nil, norm)
	if err != nil {
		return CartView{}, false, err
	}
	return idempotentTenantWrite(ctx, s.repo, scopeCartBatchDelete, repository.BuyerSubject(id.UserID),
		idemKey, hash, archivedOK, func(tx repository.Tx) (CartView, error) {
			cs, err := resolveCartScope(ctx, tx, id.UserID, req.StoreID, req.AddressID)
			if err != nil {
				return CartView{}, err
			}
			cartID, err := tx.FindCart(ctx, id.UserID)
			if errors.Is(err, repository.ErrCartNotFound) {
				if norm.ItemIDs != nil {
					return CartView{}, ErrCartItemNotFound
				}
				return buildCart(ctx, tx, 0, cs)
			}
			if err != nil {
				return CartView{}, err
			}
			if norm.ItemIDs != nil {
				got, err := tx.DeleteCartLines(ctx, cartID, *norm.ItemIDs)
				if err != nil {
					return CartView{}, err
				}
				if len(got) != len(*norm.ItemIDs) {
					return CartView{}, fmt.Errorf("%w: 请求 %d 个，车里只有其中 %d 个",
						ErrCartItemNotFound, len(*norm.ItemIDs), len(got))
				}
			} else if err := tx.DeleteSelectedCartLines(ctx, cartID); err != nil {
				return CartView{}, err
			}
			return buildCart(ctx, tx, cartID, cs)
		})
}

// DeleteItem 实现 DELETE /cart/items/{item_id}。
func (s *CartService) DeleteItem(ctx context.Context, itemID int64) error {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		cartID, err := s.ownCart(ctx, tx, id.UserID)
		if err != nil {
			return err
		}
		if err := tx.DeleteCartLine(ctx, cartID, itemID); err != nil {
			if errors.Is(err, repository.ErrCartLineNotFound) {
				return ErrCartItemNotFound
			}
			return err
		}
		return nil
	})
}

// Clear 实现 DELETE /cart。没有车时什么都不做 —— 清空一辆空车本来就该是 204。
func (s *CartService) Clear(ctx context.Context) error {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		cartID, err := tx.FindCart(ctx, id.UserID)
		if errors.Is(err, repository.ErrCartNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return tx.ClearCart(ctx, cartID)
	})
}
