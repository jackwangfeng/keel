package service

// 商品进 keel（spec §7.4）：商品源 binding（Shopify）启用时整店首拉（channel.catalog.pull，一页一个任务），
// 之后靠商品回调（EventCatalogChanged → PullItem）跟上。一件商品一个事务：
//
//  1. 找映射：channel_item_links 按外部 ID 反查。没有映射的新商品先**认领**：变体货号和 keel 里已有、还没映射的
//     SKU 一样的，挂到那个 SKU 的商品上（商家先在 keel 建过）；都对不上才新建商品（草稿，挂 binding 的
//     default_category_id）。上架仍是 keel 的决定（上架要过广告法检查），推渠道不看 keel 的上架状态
//     （repository.SKUOffer.Sellable 的 catalogOwned）。
//  2. 标题、详情、规格以商品源为准（变了就改）；价格只在新建 SKU 时取商品源的，之后归 keel（门店价、价格规则）。
//  3. 新映射上的 SKU 把商品源那一刻各门店的可售数记成 channel_listings 的基线；新建的 SKU 再用它作 keel 的初始库存
//     —— 否则第一次推送就会把商品源上的现货推成 keel 的 0（Review Focus 1）。认领的 SKU 库存以 keel 为准，
//     基线是商品源的数，于是随后的重算把 keel 的数推出去覆盖（Review Focus 2）。
//  4. 商品源上删掉的变体：删映射、keel 的 SKU 停售；删掉的商品：删映射、keel 商品下架。
//
// 读写库存在事务之外（与 channel_listing.go 同一条规矩）。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"strings"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// ErrChannelNoDefaultCategory：商品源 binding 没配 default_category_id，新商品没处挂。
var ErrChannelNoDefaultCategory = errors.New("binding 没配 default_category_id，商品拉不进来")

// channelCatalogJob 是 channel.catalog.pull 的载荷：一页。First 是首页（顺带装回调订阅）。
type channelCatalogJob struct {
	BindingID int64  `json:"binding_id"`
	Cursor    string `json:"cursor,omitempty"`
	First     bool   `json:"first,omitempty"`
}

func (s *ChannelService) enqueueCatalogPull(ctx context.Context, tx repository.Tx, bindingID int64, cursor string, first bool) error {
	key := fmt.Sprintf("pull:%d:", bindingID)
	if cursor != "" {
		h := sha1.Sum([]byte(cursor))
		key += hex.EncodeToString(h[:8])
	}
	payload, _ := json.Marshal(channelCatalogJob{BindingID: bindingID, Cursor: cursor, First: first})
	_, err := tx.EnqueueJob(ctx, repository.NewJob{Queue: QueueChannelCatalogPull, JobKey: key, Payload: payload,
		MaxAttempts: channelPushMaxAttempts})
	return err
}

// pullCatalogPage 跑一个拉商品任务。返回 nil 即完成（包括 binding 删了 / 停了 / 不是商品源）。
func (s *ChannelService) pullCatalogPage(ctx context.Context, payload []byte) error {
	var p channelCatalogJob
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("拉商品任务的载荷解不开: %w", err)
	}
	b, ab, err := s.loadBinding(ctx, p.BindingID)
	if errors.Is(err, repository.ErrChannelNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !b.IsActiveCatalogSource() {
		return nil
	}
	a, ok := s.reg.Lookup(b.Channel)
	src, isSrc := a.(channel.CatalogSource)
	if !ok || !isSrc {
		s.log.WarnContext(ctx, "binding 是商品源，但它的适配器拉不了商品", "binding_id", b.ID, "channel", b.Channel)
		return nil
	}
	if p.First {
		s.installWebhooks(ctx, b, ab, a)
	}
	page, err := src.PullCatalog(ctx, ab, p.Cursor)
	if err != nil {
		return channel.RedactError(err, ab.Secrets)
	}
	for _, item := range page.Items {
		if err := s.syncCatalogItem(ctx, b, item); err != nil {
			return channel.RedactError(fmt.Errorf("商品 %s：%w", item.ExternalID, err), ab.Secrets)
		}
	}
	if page.NextCursor == "" {
		return nil
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return s.enqueueCatalogPull(ctx, tx, b.ID, page.NextCursor, false)
	})
}

func (s *ChannelService) loadBinding(ctx context.Context, id int64) (b repository.ChannelBinding, ab channel.Binding, err error) {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return b, ab, err
	}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		if b, e = tx.GetChannelBinding(ctx, id); e != nil {
			return e
		}
		ab, e = adapterBinding(ctx, tx, merchantID, b)
		return e
	})
	return b, ab, err
}

// installWebhooks 按 config.webhook_base_url 装回调订阅；没配就不装。失败只记日志：不挡拉商品，下一次首拉再装。
func (s *ChannelService) installWebhooks(ctx context.Context, b repository.ChannelBinding, ab channel.Binding, a channel.Adapter) {
	base := strings.TrimRight(parseBindingConfig(b.Config).WebhookBaseURL, "/")
	wi, ok := a.(channel.WebhookInstaller)
	if base == "" || !ok {
		return
	}
	url := fmt.Sprintf("%s/api/v1/webhooks/channels/%d", base, b.ID)
	if err := wi.EnsureWebhooks(ctx, ab, url); err != nil {
		s.log.WarnContext(ctx, "装渠道回调订阅失败（商品照拉；停用再启用会重装）", "binding_id", b.ID,
			"err", channel.RedactError(err, ab.Secrets))
	}
}

// catalogChanged 是 EventCatalogChanged 的处理器：按事件里的商品 ID 回读商品源（回调只是提示，见 Caps.OutOfOrderInbound）。
func (s *ChannelService) catalogChanged(ctx context.Context, b repository.ChannelBinding, ev channel.Event) error {
	if !b.IsActiveCatalogSource() {
		return nil
	}
	_, ab, err := s.loadBinding(ctx, b.ID)
	if err != nil {
		return err
	}
	a, _ := s.reg.Lookup(b.Channel)
	src, ok := a.(channel.CatalogItemSource)
	if !ok {
		// 重试也不会有：记一笔、不重试（否则这条回调要耗满 20 次才进死信）。
		s.log.WarnContext(ctx, "商品回调来了，但这个适配器不能按 ID 拉单件商品", "binding_id", b.ID, "channel", b.Channel)
		return nil
	}
	for _, id := range ev.ExternalItemIDs {
		item, found, err := src.PullItem(ctx, ab, id)
		if err != nil {
			return err
		}
		if !found {
			if err := s.catalogItemGone(ctx, b, id); err != nil {
				return err
			}
			continue
		}
		if err := s.syncCatalogItem(ctx, b, item); err != nil {
			return fmt.Errorf("商品 %s：%w", id, err)
		}
	}
	return nil
}

// catalogItemGone：商品源上这件商品没了。删映射（不再推）、keel 商品下架（keel 自己的商品数据留着）。
func (s *ChannelService) catalogItemGone(ctx context.Context, b repository.ChannelBinding, externalID string) error {
	var productID int64
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		link, err := tx.ChannelItemLinkByExternal(ctx, b.ID, repository.ChannelItemProduct, externalID)
		if errors.Is(err, repository.ErrChannelNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		productID = link.KeelID
		skus, err := tx.AdminListProductSKUs(ctx, productID)
		if err != nil {
			return err
		}
		for _, k := range skus {
			if err := tx.DeleteChannelItemLink(ctx, b.ID, repository.ChannelItemSKU, k.ID); err != nil {
				return err
			}
		}
		if err := tx.DeleteChannelItemLink(ctx, b.ID, repository.ChannelItemProduct, productID); err != nil {
			return err
		}
		p, err := tx.AdminFindProduct(ctx, productID)
		if err != nil || p.Status != productStatusPublished {
			return err
		}
		_, err = tx.SetProductPublication(ctx, productID, false)
		return err
	})
	if err == nil && productID != 0 {
		s.ProductChanged(ctx, productID) // 别的销售渠道上它也该推 0 了
	}
	return err
}

// skuCodeFor 是变体在 keel 的货号：商品源给了就用；没给用「渠道名-外部 ID 末段」（Shopify：shopify-67598154137690）。
func skuCodeFor(kind string, v channel.CatalogVariant) string {
	if v.SKUCode != "" {
		return v.SKUCode
	}
	return kind + "-" + path.Base(v.ExternalID)
}

// syncCatalogItem 把商品源上的一件商品落进 keel（文件头 1–4）。
func (s *ChannelService) syncCatalogItem(ctx context.Context, b repository.ChannelBinding, item channel.CatalogItem) error {
	cfg := parseBindingConfig(b.Config)
	var productID int64
	var initRows []inventory.InitRow
	var touched []int64 // 这次映射着的 SKU（重算用）
	var stores []repository.ChannelStoreLink
	created := false
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		productID, initRows, touched, created = 0, nil, nil, false
		var err error
		if stores, err = tx.ListChannelStoreLinks(ctx, b.ID); err != nil {
			return err
		}
		storeOf := map[string]int64{}
		for _, l := range stores {
			storeOf[l.ExternalStoreID] = l.StoreID
		}

		// 1. 商品
		link, err := tx.ChannelItemLinkByExternal(ctx, b.ID, repository.ChannelItemProduct, item.ExternalID)
		switch {
		case err == nil:
			productID = link.KeelID
			p, err := tx.AdminFindProduct(ctx, productID)
			if errors.Is(err, repository.ErrCatalogNotFound) || (err == nil && p.DeletedAt != nil) {
				s.log.WarnContext(ctx, "商品源上的商品在 keel 里已经删了，不再同步", "binding_id", b.ID, "external_id", item.ExternalID)
				productID = 0
				return nil
			}
			if err != nil {
				return err
			}
			if err := syncProductFields(ctx, tx, p, item); err != nil {
				return err
			}
		case errors.Is(err, repository.ErrChannelNotFound):
			if productID, err = s.adoptProduct(ctx, tx, b, item); err != nil {
				return err
			}
			if productID == 0 {
				if cfg.DefaultCategoryID == 0 {
					return ErrChannelNoDefaultCategory
				}
				p, err := tx.CreateProduct(ctx, repository.NewProduct{CategoryID: cfg.DefaultCategoryID, Title: item.Title,
					Description: optString(item.Description)})
				if err != nil {
					return fmt.Errorf("建商品: %w", err)
				}
				productID, created = p.ID, true
			} else if p, err := tx.AdminFindProduct(ctx, productID); err != nil {
				return err
			} else if err := syncProductFields(ctx, tx, p, item); err != nil {
				return err
			}
			if err := tx.UpsertChannelItemLink(ctx, repository.ChannelItemLink{BindingID: b.ID, Kind: repository.ChannelItemProduct,
				KeelID: productID, ExternalID: item.ExternalID}); err != nil {
				return err
			}
		default:
			return err
		}
		if item.Status == channel.CatalogArchived {
			p, err := tx.AdminFindProduct(ctx, productID)
			if err != nil {
				return err
			}
			if p.Status == productStatusPublished {
				if _, err := tx.SetProductPublication(ctx, productID, false); err != nil {
					return err
				}
			}
		}

		// 2. 规格
		skus, err := tx.AdminListProductSKUs(ctx, productID)
		if err != nil {
			return err
		}
		ids := make([]int64, len(skus))
		byID, byCode := map[int64]repository.AdminSKU{}, map[string]repository.AdminSKU{}
		for i, k := range skus {
			ids[i], byID[k.ID], byCode[k.SKUCode] = k.ID, k, k
		}
		links, err := tx.ChannelItemLinks(ctx, b.ID, repository.ChannelItemSKU, ids)
		if err != nil {
			return err
		}
		skuOfExt := map[string]int64{}
		for id, l := range links {
			skuOfExt[l.ExternalID] = id
		}
		seen := map[int64]bool{}
		for _, v := range item.Variants {
			code := skuCodeFor(b.Channel, v)
			spec, err := encodeSpecValues(v.Options)
			if err != nil {
				return err
			}
			skuID, fresh, made := int64(0), false, false
			if id, ok := skuOfExt[v.ExternalID]; ok {
				skuID = id
			} else if l, err := tx.ChannelItemLinkByExternal(ctx, b.ID, repository.ChannelItemSKU, v.ExternalID); err == nil {
				skuID = l.KeelID // 映射在别的 keel 商品下（商家在 keel 里挪过）：照用
			} else if !errors.Is(err, repository.ErrChannelNotFound) {
				return err
			} else if k, ok := byCode[code]; ok {
				if _, linked := links[k.ID]; linked {
					s.log.WarnContext(ctx, "同货号的 SKU 已经映射到另一个变体，跳过这个变体", "binding_id", b.ID,
						"variant", v.ExternalID, "sku_code", code)
					continue
				}
				skuID, fresh = k.ID, true
			} else {
				taken, err := tx.ChannelSKUsByCodes(ctx, []string{code})
				if err != nil {
					return err
				}
				if _, dup := taken[code]; dup {
					s.log.WarnContext(ctx, "变体的货号在 keel 里被别的商品占着（或是删了的 SKU），跳过这个变体", "binding_id", b.ID,
						"variant", v.ExternalID, "sku_code", code)
					continue
				}
				k, err := tx.CreateSKU(ctx, repository.NewSKU{ProductID: productID, SKUCode: code, SpecValues: spec,
					PriceCents: v.PriceCents, Status: 1})
				if err != nil {
					return fmt.Errorf("建 SKU %s: %w", code, err)
				}
				skuID, fresh, made = k.ID, true, true
				created = true
			}
			if k, ok := byID[skuID]; ok && !made && !sameSpec(k.SpecValues, v.Options) {
				if _, err := tx.UpdateSKU(ctx, skuID, repository.SKUPatch{SpecValues: spec}); err != nil {
					return err
				}
			}
			if err := tx.UpsertChannelItemLink(ctx, repository.ChannelItemLink{BindingID: b.ID, Kind: repository.ChannelItemSKU,
				KeelID: skuID, ExternalID: v.ExternalID, Extra: v.Extra}); err != nil {
				return err
			}
			seen[skuID] = true
			touched = append(touched, skuID)
			for _, lv := range v.Levels {
				store, ok := storeOf[lv.ExternalStoreID]
				if !ok {
					continue
				}
				// 初始库存：已有库存行的由库存服务跳过，所以每次都给（上一次提交后 InitSKUs 失败的，这次补上）。
				initRows = append(initRows, inventory.InitRow{SKUID: skuID, StoreID: store, Available: max(lv.Qty, 0)})
				if fresh {
					if _, err := tx.RecordChannelListing(ctx, repository.ChannelListing{BindingID: b.ID, StoreID: store, SKUID: skuID,
						PublishedQty: lv.Qty, PublishedCents: v.PriceCents}); err != nil {
						return err
					}
				}
			}
		}

		// 3. 商品源上删掉的变体
		for id := range links {
			if seen[id] {
				continue
			}
			if err := tx.DeleteChannelItemLink(ctx, b.ID, repository.ChannelItemSKU, id); err != nil {
				return err
			}
			if byID[id].Status == 1 {
				off := int16(0)
				if _, err := tx.UpdateSKU(ctx, id, repository.SKUPatch{Status: &off}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil || productID == 0 {
		return err
	}
	if len(initRows) > 0 {
		if err := s.inv.InitSKUs(ctx, initRows); err != nil {
			return fmt.Errorf("商品已同步，初始库存还没写进库存服务（重试会补上）: %w", err)
		}
	}
	if created {
		seedProductStockFlags(ctx, s.repo, s.inv, []int64{productID})
	}
	if err := s.syncImages(ctx, b, productID, item.ExternalID, item.ImageURLs); err != nil {
		s.log.WarnContext(ctx, "商品图没同步上（商品照常同步，图保持原样；下次同步再试）", "binding_id", b.ID,
			"product_id", productID, "err", err)
	}
	for _, l := range stores {
		if err := s.RecomputeListings(ctx, l.StoreID, touched, b.ID); err != nil {
			return err
		}
	}
	return nil
}

// adoptProduct 给一件还没映射的商品找 keel 里现成的商品：第一个货号对得上、还没映射的 SKU 所在的商品
// （那件商品本身也没映射到这个 binding 的别的外部商品）。找不到返回 0。
func (s *ChannelService) adoptProduct(ctx context.Context, tx repository.Tx, b repository.ChannelBinding, item channel.CatalogItem) (int64, error) {
	codes := make([]string, 0, len(item.Variants))
	for _, v := range item.Variants {
		codes = append(codes, skuCodeFor(b.Channel, v))
	}
	found, err := tx.ChannelSKUsByCodes(ctx, codes)
	if err != nil || len(found) == 0 {
		return 0, err
	}
	for _, c := range codes {
		k, ok := found[c]
		if !ok || k.Deleted {
			continue
		}
		skuLinked, err := tx.ChannelItemLinks(ctx, b.ID, repository.ChannelItemSKU, []int64{k.ID})
		if err != nil {
			return 0, err
		}
		prodLinked, err := tx.ChannelItemLinks(ctx, b.ID, repository.ChannelItemProduct, []int64{k.ProductID})
		if err != nil {
			return 0, err
		}
		if len(skuLinked) == 0 && len(prodLinked) == 0 {
			p, err := tx.AdminFindProduct(ctx, k.ProductID)
			if err != nil {
				return 0, err
			}
			if p.DeletedAt == nil {
				return k.ProductID, nil
			}
		}
	}
	return 0, nil
}

// syncProductFields 把商品源管的字段（标题、详情）同步到 keel 商品，没变就不写。
func syncProductFields(ctx context.Context, tx repository.Tx, p repository.AdminProduct, item channel.CatalogItem) error {
	var patch repository.ProductPatch
	if item.Title != "" && item.Title != p.Title {
		patch.Title = &item.Title
	}
	cur := ""
	if p.Description != nil {
		cur = *p.Description
	}
	if item.Description != cur {
		d := item.Description
		patch.Description = &d
	}
	if patch.Title == nil && patch.Description == nil {
		return nil
	}
	_, err := tx.UpdateProduct(ctx, p.ID, patch)
	return err
}

func sameSpec(raw []byte, want map[string]string) bool {
	var have map[string]string
	if len(raw) > 0 {
		if json.Unmarshal(raw, &have) != nil {
			return false
		}
	}
	if len(have) == 0 && len(want) == 0 {
		return true
	}
	return maps.Equal(have, want)
}
