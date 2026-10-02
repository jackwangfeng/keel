package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/keel/keel/internal/channel"
)

// 拉商品：分页 products（每件最多 100 个变体、每个变体最多 10 个 location；联调发现不够再分页）。礼品卡滤掉 ——
// keel 卖的是实物，礼品卡在 Shopify 上也不跟踪库存。

const productFields = `id title descriptionHtml status isGiftCard
  media(first:20){ nodes{ ... on MediaImage { image{ url } } } }
  variants(first:100){ nodes{ id sku price selectedOptions{ name value } image{ url }
    inventoryItem{ id tracked inventoryLevels(first:10){ nodes{ location{ id } quantities(names:["available"]){ quantity } } } } } }`

const queryProducts = `query Products($first:Int!,$after:String){ products(first:$first, after:$after){
  pageInfo{ hasNextPage endCursor } nodes{ ` + productFields + ` } } }`

const queryProduct = `query Product($id:ID!){ product(id:$id){ ` + productFields + ` } }`

type productNode struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	DescriptionHTML string `json:"descriptionHtml"`
	Status          string `json:"status"`
	IsGiftCard      bool   `json:"isGiftCard"`
	Media           struct {
		Nodes []struct {
			Image *struct {
				URL string `json:"url"`
			} `json:"image"`
		} `json:"nodes"`
	} `json:"media"`
	Variants struct {
		Nodes []variantNode `json:"nodes"`
	} `json:"variants"`
}

type variantNode struct {
	ID              string  `json:"id"`
	SKU             *string `json:"sku"`
	Price           string  `json:"price"`
	SelectedOptions []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"selectedOptions"`
	Image *struct {
		URL string `json:"url"`
	} `json:"image"`
	InventoryItem struct {
		ID              string `json:"id"`
		Tracked         bool   `json:"tracked"`
		InventoryLevels struct {
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

// variantExtra 是存进 channel_item_links.extra 的变体附带 ID（推库存 / 改价要用）。
type variantExtra struct {
	InventoryItemID string `json:"inventory_item_id"`
	ProductID       string `json:"product_id"`
	Tracked         *bool  `json:"tracked,omitempty"`
}

// PullCatalog 取一页商品。cursor 为空取第一页。
func (a *Adapter) PullCatalog(ctx context.Context, b channel.Binding, cursor string) (channel.CatalogPage, error) {
	vars := map[string]any{"first": a.o.PageSize}
	if cursor != "" {
		vars["after"] = cursor
	}
	var out struct {
		Products struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []productNode `json:"nodes"`
		} `json:"products"`
	}
	if err := a.gql(ctx, b, "Products", queryProducts, vars, &out); err != nil {
		return channel.CatalogPage{}, err
	}
	var page channel.CatalogPage
	for _, n := range out.Products.Nodes {
		item, ok, err := toItem(n)
		if err != nil {
			return channel.CatalogPage{}, err
		}
		if ok {
			page.Items = append(page.Items, item)
		}
	}
	if out.Products.PageInfo.HasNextPage {
		page.NextCursor = out.Products.PageInfo.EndCursor
	}
	return page, nil
}

// PullItem 按 gid 取一件商品。商品删了（或是礼品卡）返回 found = false。
func (a *Adapter) PullItem(ctx context.Context, b channel.Binding, externalID string) (channel.CatalogItem, bool, error) {
	var out struct {
		Product *productNode `json:"product"`
	}
	if err := a.gql(ctx, b, "Product", queryProduct, map[string]any{"id": externalID}, &out); err != nil {
		return channel.CatalogItem{}, false, err
	}
	if out.Product == nil {
		return channel.CatalogItem{}, false, nil
	}
	return toItem(*out.Product)
}

func toItem(n productNode) (channel.CatalogItem, bool, error) {
	if n.IsGiftCard {
		return channel.CatalogItem{}, false, nil
	}
	item := channel.CatalogItem{ExternalID: n.ID, Title: n.Title, Description: n.DescriptionHTML}
	switch n.Status {
	case "ACTIVE":
		item.Status = channel.CatalogActive
	case "DRAFT":
		item.Status = channel.CatalogDraft
	default: // ARCHIVED，以及以后新加的状态：按不在卖处理
		item.Status = channel.CatalogArchived
	}
	for _, m := range n.Media.Nodes {
		if m.Image != nil && m.Image.URL != "" {
			item.ImageURLs = append(item.ImageURLs, m.Image.URL)
		}
	}
	for _, v := range n.Variants.Nodes {
		price, err := parsePriceCents(v.Price)
		if err != nil {
			return channel.CatalogItem{}, false, fmt.Errorf("Shopify 变体 %s 的价格 %q：%w", v.ID, v.Price, err)
		}
		cv := channel.CatalogVariant{ExternalID: v.ID, PriceCents: price, Options: map[string]string{}}
		if v.SKU != nil {
			cv.SKUCode = strings.TrimSpace(*v.SKU)
		}
		// 只有一个默认规格的商品，Shopify 给的是 Title = Default Title：不当规格。
		if !(len(v.SelectedOptions) == 1 && v.SelectedOptions[0].Name == "Title" && v.SelectedOptions[0].Value == "Default Title") {
			for _, o := range v.SelectedOptions {
				cv.Options[o.Name] = o.Value
			}
		}
		if v.Image != nil {
			cv.ImageURL = v.Image.URL
		}
		tracked := v.InventoryItem.Tracked
		cv.Extra, _ = json.Marshal(variantExtra{InventoryItemID: v.InventoryItem.ID, ProductID: n.ID, Tracked: &tracked})
		for _, l := range v.InventoryItem.InventoryLevels.Nodes {
			if len(l.Quantities) == 0 {
				continue
			}
			cv.Levels = append(cv.Levels, channel.StockLevel{ExternalStoreID: l.Location.ID, Qty: l.Quantities[0].Quantity})
		}
		item.Variants = append(item.Variants, cv)
	}
	return item, true, nil
}

// parsePriceCents 把 Shopify 的十进制价格串（"949.95"）精确换成分，不走浮点。超过两位小数、负数、空串都拒。
func parsePriceCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, fmt.Errorf("不是非负的十进制金额")
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		// 尾部的 0 可以去掉（"1.500"），真有第三位小数的拒。
		trimmed := strings.TrimRight(frac, "0")
		if len(trimmed) > 2 {
			return 0, fmt.Errorf("超过两位小数")
		}
		frac = trimmed
	}
	for len(frac) < 2 {
		frac += "0"
	}
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("整数部分认不出来")
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("小数部分认不出来")
	}
	if w > (1<<62)/100 {
		return 0, fmt.Errorf("金额太大")
	}
	return w*100 + f, nil
}

// formatCents 把分写成 Shopify 要的两位小数串。
func formatCents(c int64) string {
	return fmt.Sprintf("%d.%02d", c/100, c%100)
}
