package shopify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/keel/keel/internal/channel"
)

// 推库存与价格。
//
// 库存：inventorySetQuantities 写绝对值，changeFromQuantity 是上次推出去的数（CAS；没推过给 null 跳过比对）。
// 实测（2026-10-02）**一批里只要有一条出错，整批都不生效**；冲突码是 CHANGE_FROM_QUANTITY_STALE。所以冲突的那几条
// 单拎出来回读现值、回 Conflict，其余的在同一次调用里重提一次（组成员变了，幂等键也跟着变）。
//
// 价格：挂在变体上，按商品分组一次 productVariantsBulkUpdate。只推 PushPrice 为真的条目。

const mutationSetQty = `mutation SetQty($in:InventorySetQuantitiesInput!,$k:String!){
  inventorySetQuantities(input:$in) @idempotent(key:$k){ inventoryAdjustmentGroup{ id } userErrors{ field message code } } }`

const mutationSetPrices = `mutation SetPrices($pid:ID!,$vs:[ProductVariantsBulkInput!]!){
  productVariantsBulkUpdate(productId:$pid, variants:$vs){ userErrors{ field message } } }`

const queryLevels = `query Levels($id:ID!){ inventoryItem(id:$id){
  inventoryLevels(first:10){ nodes{ location{ id } quantities(names:["available"]){ quantity } } } } }`

const staleCode = "CHANGE_FROM_QUANTITY_STALE"

// PushListings 见文件头。整批级别的失败（第一批就限流、凭据失效）返回 error；之后的批次失败记在各条的 Err 上。
func (a *Adapter) PushListings(ctx context.Context, b channel.Binding, ls []channel.Listing) ([]channel.ListingResult, error) {
	res := make([]channel.ListingResult, len(ls))
	extras := make([]variantExtra, len(ls))
	qtyErr := make([]error, len(ls))
	priceErr := make([]error, len(ls))
	var qtyIdx []int
	for i, l := range ls {
		res[i] = channel.ListingResult{StoreID: l.StoreID, SKUID: l.SKUID}
		if len(l.Extra) > 0 {
			_ = json.Unmarshal(l.Extra, &extras[i])
		}
		switch {
		case extras[i].Tracked != nil && !*extras[i].Tracked:
			// Shopify 上不跟踪库存的变体：没有可售数可推。
		case extras[i].InventoryItemID == "":
			qtyErr[i] = errors.New("SKU 映射里没有 inventory_item_id（重新拉一次商品）")
		default:
			qtyIdx = append(qtyIdx, i)
		}
	}
	anyDone := false
	for start := 0; start < len(qtyIdx); start += listingBatch {
		chunk := qtyIdx[start:min(start+listingBatch, len(qtyIdx))]
		if err := a.setQty(ctx, b, ls, extras, chunk, res, qtyErr); err != nil {
			if !anyDone {
				return nil, err
			}
			for _, i := range chunk {
				qtyErr[i] = err
			}
			continue
		}
		anyDone = true
	}

	byProduct := map[string][]int{}
	var products []string
	for i, l := range ls {
		if !l.PushPrice {
			continue
		}
		pid := extras[i].ProductID
		if pid == "" {
			priceErr[i] = errors.New("SKU 映射里没有 product_id（重新拉一次商品）")
			continue
		}
		if _, seen := byProduct[pid]; !seen {
			products = append(products, pid)
		}
		byProduct[pid] = append(byProduct[pid], i)
	}
	for _, pid := range products {
		if err := a.setPrices(ctx, b, ls, pid, byProduct[pid], priceErr); err != nil {
			if !anyDone && len(qtyIdx) == 0 {
				return nil, err
			}
			for _, i := range byProduct[pid] {
				priceErr[i] = err
			}
			continue
		}
		anyDone = true
	}
	for i := range res {
		if qtyErr[i] != nil || priceErr[i] != nil {
			res[i].Err = errors.Join(qtyErr[i], priceErr[i])
		}
	}
	return res, nil
}

// setQty 推一批可售数。返回的 error 是整批没推成（限流、凭据、网络）；逐条的结果写进 res / qtyErr。
func (a *Adapter) setQty(ctx context.Context, b channel.Binding, ls []channel.Listing, ex []variantExtra, idx []int,
	res []channel.ListingResult, qtyErr []error) error {
	for round := 0; len(idx) > 0; round++ {
		items := make([]map[string]any, len(idx))
		keys := make([]string, len(idx))
		for j, i := range idx {
			var from any
			if ls[i].PrevQty != nil {
				from = *ls[i].PrevQty
			}
			items[j] = map[string]any{"inventoryItemId": ex[i].InventoryItemID, "locationId": ls[i].ExternalStoreID,
				"quantity": ls[i].Qty, "changeFromQuantity": from}
			keys[j] = ls[i].IdemKey
		}
		in := map[string]any{"name": "available", "reason": "correction",
			"referenceDocumentUri": "keel://channel/" + strconv.FormatInt(b.ID, 10), "quantities": items}
		var out struct {
			R struct {
				UserErrors []userError `json:"userErrors"`
			} `json:"inventorySetQuantities"`
		}
		if err := a.gql(ctx, b, "SetQty", mutationSetQty, map[string]any{"in": in, "k": idemKey(keys)}, &out); err != nil {
			return err
		}
		if len(out.R.UserErrors) == 0 {
			return nil // 这一批全部生效
		}
		// 有错：整批都没生效。把出错的条目定下结果，干净的重提（只重提一次）。
		bad := map[int]bool{}
		var whole []string
		for _, u := range out.R.UserErrors {
			j, ok := u.index("quantities")
			if !ok || j < 0 || j >= len(idx) {
				whole = append(whole, u.Message)
				continue
			}
			i := idx[j]
			bad[i] = true
			if u.Code == staleCode {
				obs, found, err := a.Available(ctx, b, ex[i].InventoryItemID, ls[i].ExternalStoreID)
				if err != nil {
					qtyErr[i] = fmt.Errorf("Shopify 上的可售数被改过，回读现值失败：%w", err)
					continue
				}
				if found {
					res[i].Conflict, res[i].ObservedQty = true, &obs
				}
				qtyErr[i] = fmt.Errorf("Shopify 上的可售数和上次推的对不上（%s）", u.Message)
				continue
			}
			qtyErr[i] = fmt.Errorf("Shopify 拒绝了这一条：%s", u.Message)
		}
		if len(whole) > 0 {
			err := fmt.Errorf("Shopify 拒绝了这一批：%s", truncate(strings.Join(whole, "；"), 300))
			for _, i := range idx {
				if !bad[i] {
					qtyErr[i] = err
				}
			}
			return nil
		}
		var clean []int
		for _, i := range idx {
			if !bad[i] {
				clean = append(clean, i)
			}
		}
		if round >= 1 {
			for _, i := range clean {
				qtyErr[i] = errBatchNotApplied
			}
			return nil
		}
		idx = clean
	}
	return nil
}

// idemKey：同一组 IdemKey（重推同一次）得到同一个键，组成员不同得到不同的键。
func idemKey(keys []string) string {
	k := append([]string(nil), keys...)
	sort.Strings(k)
	h := sha256.Sum256([]byte(strings.Join(k, "\n")))
	return "keel-" + hex.EncodeToString(h[:])[:40]
}

func (a *Adapter) setPrices(ctx context.Context, b channel.Binding, ls []channel.Listing, productID string, idx []int, priceErr []error) error {
	vs := make([]map[string]any, len(idx))
	for j, i := range idx {
		vs[j] = map[string]any{"id": ls[i].ExternalSKUID, "price": formatCents(ls[i].PriceCents)}
	}
	var out struct {
		R struct {
			UserErrors []userError `json:"userErrors"`
		} `json:"productVariantsBulkUpdate"`
	}
	if err := a.gql(ctx, b, "SetPrices", mutationSetPrices, map[string]any{"pid": productID, "vs": vs}, &out); err != nil {
		return err
	}
	if len(out.R.UserErrors) == 0 {
		return nil
	}
	// 有错就当整组没生效（同库存那一条）：出错的记它自己的原因，其余的记「同批出错」，下一次重推。
	for _, u := range out.R.UserErrors {
		j, ok := u.index("variants")
		if !ok || j < 0 || j >= len(idx) {
			err := fmt.Errorf("Shopify 拒绝了改价：%s", u.Message)
			for _, i := range idx {
				priceErr[i] = err
			}
			return nil
		}
		priceErr[idx[j]] = fmt.Errorf("Shopify 拒绝了改价：%s", u.Message)
	}
	for _, i := range idx {
		if priceErr[i] == nil {
			priceErr[i] = errBatchNotApplied
		}
	}
	return nil
}

// Available 回读一个 inventory item 在一个 location 的 available。没有这个 location 的水位返回 found = false。
func (a *Adapter) Available(ctx context.Context, b channel.Binding, inventoryItemID, locationID string) (int32, bool, error) {
	var out struct {
		Item *struct {
			Levels struct {
				Nodes []struct {
					Location struct {
						ID string `json:"id"`
					} `json:"location"`
					Quantities []struct {
						Quantity int32 `json:"quantity"`
					} `json:"quantities"`
				} `json:"nodes"`
			} `json:"inventoryLevels"`
		} `json:"inventoryItem"`
	}
	if err := a.gql(ctx, b, "Levels", queryLevels, map[string]any{"id": inventoryItemID}, &out); err != nil {
		return 0, false, err
	}
	if out.Item == nil {
		return 0, false, nil
	}
	for _, n := range out.Item.Levels.Nodes {
		if n.Location.ID == locationID && len(n.Quantities) > 0 {
			return n.Quantities[0].Quantity, true, nil
		}
	}
	return 0, false, nil
}
