package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/keel/keel/internal/repository"
)

// 运费一段的入口：取齐素材（loadFreightContext）、按地址算（freightContext.quote，
// 纯函数在 freight_calc.go）。计价顺序与口径写在 freight_calc.go 的文件头。

// ErrRegionNotDeliverable：有商品送不到这个收货地址。契约：422 region-not-deliverable，
// 响应体的 undeliverable_items 逐行列出。具体的行在 *UndeliverableError 里。
var ErrRegionNotDeliverable = errors.New("有商品送不到这个收货地址")

// UndeliverableError 带着送不到的那几行。errors.Is(err, ErrRegionNotDeliverable) 为真。
//
// 它必须是一个错误，不能是「把这几行的运费算成 0 继续」：那会让一笔注定发不出去的
// 订单成交，而发现它的是仓库里拿着面单的人。
type UndeliverableError struct {
	Lines []FreightUndeliverable
}

func (e *UndeliverableError) Error() string {
	parts := make([]string, 0, len(e.Lines))
	for _, l := range e.Lines {
		parts = append(parts, "sku "+strconv.FormatInt(l.SKUID, 10)+"："+l.Reason)
	}
	return ErrRegionNotDeliverable.Error() + "：" + strings.Join(parts, "；")
}

func (e *UndeliverableError) Unwrap() error { return ErrRegionNotDeliverable }

// FreightDestination 是「运费往哪里送」：收货地址归到的省（归不到为 ""）。
type FreightDestination struct {
	ProvinceCode string
}

// destinationOf 把一个收货地址变成 FreightDestination（freight_region.go 的 provinceOf）。
func destinationOf(a repository.Address) FreightDestination {
	return FreightDestination{ProvinceCode: provinceOf(a.RegionCode, a.Province)}
}

// FreightRequest 是运费一段的全部输入 —— 与营销活动那一段约定的接口形状：
//
//	优惠后的商品金额（GoodsPayableCents）+ 件数 / 重量（Items，重量由 SKU 查）+
//	收货地址（Dest）+ 门店（Store）
//
// GoodsPayableCents 必须是**营销活动与优惠券都减完之后**的应付商品金额：
// 满额包邮比的就是它（数据模型 §7「优惠计算顺序」）。
type FreightRequest struct {
	Store             repository.StoreScope
	Dest              FreightDestination
	Items             []FreightItem
	GoodsPayableCents int64
}

// FreightQuote 是运费一段的结果。Undeliverable 非空时 Breakdown 只覆盖送得到的那些组。
type FreightQuote struct {
	Breakdown     FreightBreakdown
	Undeliverable []FreightUndeliverable
}

// loadFreightContext 取齐一批 SKU 在这家门店计运费要的全部素材：每一行的重量与
// 商品单独挂的模板、这些模板 + 门店模板 + 全店默认（连同规则）。两条查询。
func loadFreightContext(ctx context.Context, tx repository.Tx, store repository.StoreScope,
	skuIDs []int64) (freightContext, error) {
	fc := freightContext{store: store, info: map[int64]repository.SKUFreightInfo{},
		templates: map[int64]repository.FreightTemplate{}}
	if len(skuIDs) == 0 {
		return fc, nil
	}
	info, err := tx.ListSKUFreightInfo(ctx, skuIDs)
	if err != nil {
		return freightContext{}, err
	}
	fc.info = info
	seen := map[int64]bool{}
	ids := []int64{}
	for _, in := range info {
		if in.TemplateID != nil && !seen[*in.TemplateID] {
			seen[*in.TemplateID] = true
			ids = append(ids, *in.TemplateID)
		}
	}
	tpls, err := tx.ListFreightTemplatesForPricing(ctx, ids, store.StoreID)
	if err != nil {
		return freightContext{}, err
	}
	fc.templates = tpls
	return fc, nil
}

// quoteFreight 是「取齐 + 算」一步到位的版本。
func quoteFreight(ctx context.Context, tx repository.Tx, req FreightRequest) (FreightQuote, error) {
	ids := make([]int64, 0, len(req.Items))
	for _, it := range req.Items {
		ids = append(ids, it.SKUID)
	}
	fc, err := loadFreightContext(ctx, tx, req.Store, ids)
	if err != nil {
		return FreightQuote{}, err
	}
	b, bad, err := fc.quote(req.Dest.ProvinceCode, req.Items, req.GoodsPayableCents)
	if err != nil {
		return FreightQuote{}, err
	}
	return FreightQuote{Breakdown: b, Undeliverable: bad}, nil
}

// freightItemsOf 把定价结果变成运费的输入。
func freightItemsOf(lines []PricedLine) []FreightItem {
	out := make([]FreightItem, len(lines))
	for i, ln := range lines {
		out[i] = FreightItem{SKUID: ln.SKUID, Quantity: ln.Quantity}
	}
	return out
}

// buyerDestination 取买家的一个收货地址并归到省：addressID 为 nil 时用默认地址，
// 默认地址也没有时返回 nil（没有地址就没有运费可算 —— 不是「包邮」）。
// 指名的地址不存在或不属于他：ErrAddressNotFound（422）。
func buyerDestination(ctx context.Context, tx repository.Tx, userID int64,
	addressID *int64) (*int64, *FreightDestination, error) {
	if addressID != nil {
		a, err := tx.FindAddress(ctx, *addressID, userID)
		if errors.Is(err, repository.ErrAddressNotFound) {
			return nil, nil, fmt.Errorf("%w: address_id=%d", ErrAddressNotFound, *addressID)
		}
		if err != nil {
			return nil, nil, err
		}
		d := destinationOf(a)
		id := a.ID
		return &id, &d, nil
	}
	a, ok, err := tx.FindDefaultAddress(ctx, userID)
	if err != nil || !ok {
		return nil, nil, err
	}
	d := destinationOf(a)
	id := a.ID
	return &id, &d, nil
}

// loadFreightSnapshot 读回一单的运费明细快照（orders.freight_snapshot）。00056 之前的
// 订单那一列是 NULL，返回 nil —— 调用方让字段整个不出现，而不是编一份「不计运费」。
// 解不开是我们自己写坏了（这一列由 placeDraft 写），报出来。
func loadFreightSnapshot(ctx context.Context, tx repository.Tx, orderID int64,
	orderNo string) (*FreightBreakdown, error) {
	raw, err := tx.FindOrderFreightSnapshot(ctx, orderID)
	if err != nil || raw == nil {
		return nil, err
	}
	var b FreightBreakdown
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("订单 %s 的运费快照解不开: %w", orderNo, err)
	}
	if b.Groups == nil {
		b.Groups = []FreightGroup{}
	}
	return &b, nil
}
