package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/keel/keel/internal/channel"
)

// 拉商品：分页 products（每件最多 100 个变体），礼品卡滤掉 —— keel 卖的是实物，礼品卡在 Shopify 上也不跟踪库存。
//
// 各门店的可售数单独查：Shopify 单条查询的成本上限是 1000 点，实测（2026-10-02）products 带上 inventoryLevels
// 一页 10 件就要 710 点，不带只要 25 件 308 点；而 nodes(ids:) 一次 50 个 inventory item 连水位只要 15 点。
// 所以一页商品 = 一条 Products + 每 50 个跟踪库存的 item 一条 ItemLevels。每个 item 最多取 10 个 location。

const productFields = `id title descriptionHtml status isGiftCard
  media(first:20){ nodes{ ... on MediaImage { image{ url } } } }
  variants(first:100){ nodes{ id sku price selectedOptions{ name value } image{ url }
    inventoryItem{ id tracked } } }`

const queryProducts = `query Products($first:Int!,$after:String){ products(first:$first, after:$after){
  pageInfo{ hasNextPage endCursor } nodes{ ` + productFields + ` } } }`

const queryProduct = `query Product($id:ID!){ product(id:$id){ ` + productFields + ` } }`

const queryItemLevels = `query ItemLevels($ids:[ID!]!){ nodes(ids:$ids){ ... on InventoryItem { id
  inventoryLevels(first:10){ nodes{ location{ id } quantities(names:["available"]){ quantity } } } } } }`

const levelsChunk = 50

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
		ID      string `json:"id"`
		Tracked bool   `json:"tracked"`
	} `json:"inventoryItem"`
}

type levelNodes struct {
	Nodes []struct {
		Location struct {
			ID string `json:"id"`
		} `json:"location"`
		Quantities []struct {
			Quantity int32 `json:"quantity"`
		} `json:"quantities"`
	} `json:"nodes"`
}

func (l levelNodes) levels() []channel.StockLevel {
	var out []channel.StockLevel
	for _, n := range l.Nodes {
		if len(n.Quantities) > 0 {
			out = append(out, channel.StockLevel{ExternalStoreID: n.Location.ID, Qty: n.Quantities[0].Quantity})
		}
	}
	return out
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
	if err := a.fillLevels(ctx, b, page.Items); err != nil {
		return channel.CatalogPage{}, err
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
	item, ok, err := toItem(*out.Product)
	if err != nil || !ok {
		return channel.CatalogItem{}, false, err
	}
	items := []channel.CatalogItem{item}
	if err := a.fillLevels(ctx, b, items); err != nil {
		return channel.CatalogItem{}, false, err
	}
	return items[0], true, nil
}

// fillLevels 给跟踪库存的变体填各门店的可售数（文件头：单独查、每 50 个 item 一条）。
func (a *Adapter) fillLevels(ctx context.Context, b channel.Binding, items []channel.CatalogItem) error {
	type ref struct{ item, variant int }
	byItem := map[string]ref{}
	var ids []string
	for i := range items {
		for j := range items[i].Variants {
			var ex variantExtra
			_ = json.Unmarshal(items[i].Variants[j].Extra, &ex)
			if ex.InventoryItemID == "" || (ex.Tracked != nil && !*ex.Tracked) {
				continue
			}
			byItem[ex.InventoryItemID] = ref{i, j}
			ids = append(ids, ex.InventoryItemID)
		}
	}
	for start := 0; start < len(ids); start += levelsChunk {
		var out struct {
			Nodes []*struct {
				ID     string     `json:"id"`
				Levels levelNodes `json:"inventoryLevels"`
			} `json:"nodes"`
		}
		if err := a.gql(ctx, b, "ItemLevels", queryItemLevels, map[string]any{"ids": ids[start:min(start+levelsChunk, len(ids))]}, &out); err != nil {
			return err
		}
		for _, n := range out.Nodes {
			if n == nil {
				continue
			}
			if r, ok := byItem[n.ID]; ok {
				items[r.item].Variants[r.variant].Levels = n.Levels.levels()
			}
		}
	}
	return nil
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
