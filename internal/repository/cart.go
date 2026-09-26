package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 购物车（数据模型 §10）。
//
// 「这是谁的车」只在 FindCart / EnsureCart 里按 userID 回答一次；之后每个条目方法
// 都收 cartID，而条目 SQL 上的 cart_id 条件就是越权过滤本身
// （db/queries/carts.sql 的文件头）。条目方法因此**不收** userID：给它两个参数
// 等于给调用方一个「传了 userID 却传错 cartID」的机会。

var (
	// ErrCartNotFound：这个买家还没有车（从没加购过）。读路径据此回一辆空车。
	ErrCartNotFound = errors.New("购物车不存在")
	// ErrCartLineNotFound：条目不在这辆车里（含别人车里的条目）。
	ErrCartLineNotFound = errors.New("购物车条目不存在")
	// ErrCartLineQuantityCap：累加后超过 999，upsert 没有写。
	ErrCartLineQuantityCap = errors.New("购物车条目数量超过上限")
	// ErrSKUNotFound：这个 SKU 在本店查不到（不存在，或属于别家店 —— RLS 让两者同形）。
	ErrSKUNotFound = errors.New("SKU 不存在")
)

// CartLine 是车里的一行，带上判断「现在能不能买」的素材。**没有价格**：
// 价格走 ListSKUsForPricing（与试算、下单同一条查询），见 carts.sql 文件头。
type CartLine struct {
	ID         int64
	SKUID      int64
	ProductID  int64
	Quantity   int32
	Selected   bool
	Title      string
	SpecValues []byte
	ImageURL   *string

	// OnShelf 与定价查询的四个在架条件逐字对应。为假即失效（off_shelf）。
	OnShelf bool
	// AvailableQty 这家门店的可售量；没有库存行记为 0。
	AvailableQty int32
}

// CartSKU 是加购时取的 SKU 状态。
type CartSKU struct {
	ID           int64
	ProductID    int64
	OnShelf      bool
	AvailableQty int32
}

// CartTx 是购物车这一面。
type CartTx interface {
	// FindCart 取这个买家的车。没有返回 ErrCartNotFound。
	FindCart(ctx context.Context, userID int64) (int64, error)
	// EnsureCart 取这个买家的车，没有就建。
	EnsureCart(ctx context.Context, userID int64) (int64, error)

	// ListCartLines 整辆车，按加购时间倒序。storeID 决定 AvailableQty 按哪家店算；
	// 0 表示没有门店（不在服务范围），此时 AvailableQty 恒为 0。
	ListCartLines(ctx context.Context, cartID, storeID int64) ([]CartLine, error)
	// FindSKUForCart 取一个 SKU 在这家店的状态。查不到返回 ErrSKUNotFound。
	FindSKUForCart(ctx context.Context, skuID, storeID int64) (CartSKU, error)
	// CountCartLines 车里有几种商品。
	CountCartLines(ctx context.Context, cartID int64) (int64, error)
	// CartLineQuantity 车里某个 SKU 当前的数量；不在车里返回 0。
	CartLineQuantity(ctx context.Context, cartID, skuID int64) (int32, error)
	// AddCartLine 加购（upsert，数量累加）。累加后超过 999 返回 ErrCartLineQuantityCap。
	AddCartLine(ctx context.Context, cartID, skuID, productID int64, qty int32) error

	// FindCartLine 这辆车里的一行。不在返回 ErrCartLineNotFound。
	FindCartLine(ctx context.Context, cartID, itemID int64) (CartLine, error)
	// UpdateCartLine 改数量 / 勾选态，nil 表示不改。不在返回 ErrCartLineNotFound。
	UpdateCartLine(ctx context.Context, cartID, itemID int64, qty *int32, selected *bool) error
	// SetAllCartSelection 全选 / 全不选。
	SetAllCartSelection(ctx context.Context, cartID int64, selected bool) error
	// SetCartSelection 按 id 勾选，返回这辆车里确实存在的那些 id。
	SetCartSelection(ctx context.Context, cartID int64, itemIDs []int64, selected bool) ([]int64, error)
	// DeleteCartLines 按 id 删除，返回这辆车里确实删掉的那些 id。
	DeleteCartLines(ctx context.Context, cartID int64, itemIDs []int64) ([]int64, error)
	// DeleteSelectedCartLines 删除全部已勾选条目。
	DeleteSelectedCartLines(ctx context.Context, cartID int64) error
	// DeleteCartLine 删一行。不在返回 ErrCartLineNotFound。
	DeleteCartLine(ctx context.Context, cartID, itemID int64) error
	// ClearCart 清空。
	ClearCart(ctx context.Context, cartID int64) error
}

func (t tenantTx) FindCart(ctx context.Context, userID int64) (int64, error) {
	id, err := t.q.FindCartID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrCartNotFound
	}
	return id, err
}

func (t tenantTx) EnsureCart(ctx context.Context, userID int64) (int64, error) {
	return t.q.EnsureCart(ctx, userID)
}

func (t tenantTx) ListCartLines(ctx context.Context, cartID, storeID int64) ([]CartLine, error) {
	rows, err := t.q.ListCartLines(ctx, db.ListCartLinesParams{CartID: cartID, StoreID: storeID})
	if err != nil {
		return nil, err
	}
	out := make([]CartLine, 0, len(rows))
	for _, r := range rows {
		out = append(out, CartLine{
			ID:           r.ID,
			SKUID:        r.SkuID,
			ProductID:    r.ProductID,
			Quantity:     r.Quantity,
			Selected:     r.Selected,
			Title:        r.Title,
			SpecValues:   r.SpecValues,
			ImageURL:     r.ImageUrl,
			OnShelf:      r.OnShelf,
			AvailableQty: r.AvailableQty,
		})
	}
	return out, nil
}

func (t tenantTx) FindSKUForCart(ctx context.Context, skuID, storeID int64) (CartSKU, error) {
	r, err := t.q.FindSKUForCart(ctx, db.FindSKUForCartParams{SkuID: skuID, StoreID: storeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return CartSKU{}, ErrSKUNotFound
	}
	if err != nil {
		return CartSKU{}, err
	}
	return CartSKU{ID: r.ID, ProductID: r.ProductID, OnShelf: r.OnShelf, AvailableQty: r.AvailableQty}, nil
}

func (t tenantTx) CountCartLines(ctx context.Context, cartID int64) (int64, error) {
	return t.q.CountCartLines(ctx, cartID)
}

func (t tenantTx) CartLineQuantity(ctx context.Context, cartID, skuID int64) (int32, error) {
	r, err := t.q.FindCartLineBySKU(ctx, db.FindCartLineBySKUParams{CartID: cartID, SkuID: skuID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return r.Quantity, nil
}

func (t tenantTx) AddCartLine(ctx context.Context, cartID, skuID, productID int64, qty int32) error {
	_, err := t.q.AddCartLine(ctx, db.AddCartLineParams{
		CartID: cartID, SkuID: skuID, ProductID: productID, Quantity: qty,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// DO UPDATE 的 WHERE 不成立（累加后超过 999）时 RETURNING 不返回行。
		return ErrCartLineQuantityCap
	}
	return err
}

func (t tenantTx) FindCartLine(ctx context.Context, cartID, itemID int64) (CartLine, error) {
	r, err := t.q.FindCartLine(ctx, db.FindCartLineParams{ID: itemID, CartID: cartID})
	if errors.Is(err, pgx.ErrNoRows) {
		return CartLine{}, ErrCartLineNotFound
	}
	if err != nil {
		return CartLine{}, err
	}
	return CartLine{ID: r.ID, SKUID: r.SkuID, Quantity: r.Quantity, Selected: r.Selected}, nil
}

func (t tenantTx) UpdateCartLine(ctx context.Context, cartID, itemID int64, qty *int32, selected *bool) error {
	_, err := t.q.UpdateCartLine(ctx, db.UpdateCartLineParams{
		Quantity: qty, Selected: selected, ID: itemID, CartID: cartID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCartLineNotFound
	}
	return err
}

func (t tenantTx) SetAllCartSelection(ctx context.Context, cartID int64, selected bool) error {
	return t.q.SetAllCartSelection(ctx, db.SetAllCartSelectionParams{CartID: cartID, Selected: selected})
}

func (t tenantTx) SetCartSelection(ctx context.Context, cartID int64, itemIDs []int64, selected bool) ([]int64, error) {
	return t.q.SetCartSelection(ctx, db.SetCartSelectionParams{
		Selected: selected, CartID: cartID, ItemIds: itemIDs,
	})
}

func (t tenantTx) DeleteCartLines(ctx context.Context, cartID int64, itemIDs []int64) ([]int64, error) {
	return t.q.DeleteCartLines(ctx, db.DeleteCartLinesParams{CartID: cartID, ItemIds: itemIDs})
}

func (t tenantTx) DeleteSelectedCartLines(ctx context.Context, cartID int64) error {
	return t.q.DeleteSelectedCartLines(ctx, cartID)
}

func (t tenantTx) DeleteCartLine(ctx context.Context, cartID, itemID int64) error {
	n, err := t.q.DeleteCartLine(ctx, db.DeleteCartLineParams{ID: itemID, CartID: cartID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrCartLineNotFound
	}
	return nil
}

func (t tenantTx) ClearCart(ctx context.Context, cartID int64) error {
	return t.q.ClearCart(ctx, cartID)
}
