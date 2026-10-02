package shopify_test

// 对真实开发店的联调（KEEL_SHOPIFY_LIVE=1 才跑）。凭据从 ~/.config/keel/shopify-dev 读
// （SHOPIFY_SHOP / SHOPIFY_CLIENT_ID / SHOPIFY_CLIENT_SECRET），不打印。**不改开发店的任何数据**：
// 推送只做两件事——一次必然冲突的 CAS（不生效）、一次值不变的空操作。

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify"
)

// LiveBinding 读开发店凭据；没开开关就 Skip。
func liveBinding(t *testing.T) channel.Binding {
	t.Helper()
	if os.Getenv("KEEL_SHOPIFY_LIVE") != "1" {
		t.Skip("KEEL_SHOPIFY_LIVE=1 才对真实开发店跑")
	}
	home, _ := os.UserHomeDir()
	f, err := os.Open(filepath.Join(home, ".config/keel/shopify-dev"))
	if err != nil {
		t.Fatalf("读不到开发店凭据：%v", err)
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	sec, _ := json.Marshal(map[string]string{"client_id": kv["SHOPIFY_CLIENT_ID"], "client_secret": kv["SHOPIFY_CLIENT_SECRET"]})
	return channel.Binding{ID: 1, Kind: shopify.Kind, ExternalAccount: kv["SHOPIFY_SHOP"], Secrets: sec,
		Roles: channel.RoleCatalogSource | channel.RoleOutlet}
}

func TestLiveDevStore(t *testing.T) {
	b := liveBinding(t)
	a := shopify.New(shopify.Options{})
	ctx := context.Background()
	var items []channel.CatalogItem
	cursor := ""
	for {
		p, err := a.PullCatalog(ctx, b, cursor)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, p.Items...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	t.Logf("拉到 %d 件商品（礼品卡已滤）", len(items))
	var pick *channel.CatalogVariant
	withLevels := 0
	for i := range items {
		if strings.Contains(strings.ToLower(items[i].Title), "gift card") {
			t.Fatalf("礼品卡没滤掉：%s", items[i].Title)
		}
		for j := range items[i].Variants {
			v := &items[i].Variants[j]
			if len(v.Levels) > 0 {
				withLevels++
				var ex struct{ Tracked *bool }
				_ = json.Unmarshal(v.Extra, &ex)
				if pick == nil && ex.Tracked != nil && *ex.Tracked {
					pick = v
				}
			}
		}
	}
	if withLevels == 0 || pick == nil {
		t.Fatal("没有一个跟踪库存、带水位的变体")
	}
	lvl := pick.Levels[0]
	cur := lvl.Qty
	stale := cur + 7
	l := channel.Listing{StoreID: 1, SKUID: 1, ExternalStoreID: lvl.ExternalStoreID, ExternalSKUID: pick.ExternalID,
		Extra: pick.Extra, Qty: cur, PrevQty: &stale, IdemKey: "live-stale"}
	res, err := a.PushListings(ctx, b, []channel.Listing{l})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Conflict || res[0].ObservedQty == nil || *res[0].ObservedQty != cur {
		t.Fatalf("必然冲突的 CAS = %+v（ObservedQty 期望 %d）", res[0], cur)
	}
	l.PrevQty, l.IdemKey = &cur, "live-noop"
	res, err = a.PushListings(ctx, b, []channel.Listing{l})
	if err != nil || res[0].Err != nil {
		t.Fatalf("值不变的空操作：%v / %v", err, res[0].Err)
	}
	var ex struct {
		InventoryItemID string `json:"inventory_item_id"`
	}
	_ = json.Unmarshal(pick.Extra, &ex)
	if q, ok, err := a.Available(ctx, b, ex.InventoryItemID, lvl.ExternalStoreID); err != nil || !ok || q != cur {
		t.Fatalf("回读 = %d %v %v，期望 %d（开发店的数被改了！）", q, ok, err, cur)
	}
}
