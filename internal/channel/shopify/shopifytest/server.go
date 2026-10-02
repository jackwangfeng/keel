// Package shopifytest 是进程内的 Shopify 模拟平台：适配器用到的 Admin GraphQL 子集 + client credentials 换 token
// + 商品图。行为照 2026-10-02 在开发店上实测的结果（第二期计划「已实测的 Shopify 事实」）：
//
//   - inventorySetQuantities：changeFromQuantity 对不上回 CHANGE_FROM_QUANTITY_STALE（field 带下标），
//     一批里有一条出错整批不生效；changeFromQuantity 为 null 跳过比对；同一个 @idempotent 键只生效一次。
//   - 每次 GraphQL 响应带 extensions.cost.throttleStatus。
//
// 订单、fulfillment order、fulfillmentCreate 与订单回调见 orders.go（操作名 Order / FulfillmentCreate）。
//
// 不解析 GraphQL：按请求里的 operationName 分发，变量按固定形状解。适配器的每条操作都带名字。
// 不 import 适配器包（适配器的测试要 import 这里）。
package shopifytest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Product / Variant 是往模拟店里放的商品。
type Product struct {
	Title, Description string
	Status             string // ACTIVE / DRAFT / ARCHIVED，空 = ACTIVE
	GiftCard           bool
	Images             []string // 图片 URL（可用 Server.ImageURL 造一个本地能下载的）
	Variants           []Variant
}

type Variant struct {
	SKU     string // 空 = null
	Price   string // "12.34"
	Options map[string]string
	Tracked bool
	Levels  map[string]int32 // location gid → available
}

// WebhookSub 是装在模拟店上的一条订阅。
type WebhookSub struct{ Topic, URI string }

type product struct {
	gid      string
	p        Product
	variants []string
}

type variant struct {
	gid, productGID, itemGID string
	v                        Variant
}

type invItem struct {
	tracked bool
	levels  map[string]int32
}

type Server struct {
	*httptest.Server
	Shop string

	mu        sync.Mutex
	clientID  string
	secret    string
	tokens    map[string]bool
	nextID    int64
	order     []string
	products  map[string]*product
	variants  map[string]*variant
	items     map[string]*invItem
	locations []string
	idem      map[string]bool
	idemSeen  []string
	calls     map[string]int
	throttle  int
	webhooks  []WebhookSub
	images    map[string][]byte
	imageHits int

	orders   map[string]*simOrder // 订单部分见 orders.go
	orderSeq []string
	clock    time.Time
	fail     map[string][]bool // operationName → 排着的 503（值：是否先落地）
}

// New 起一个模拟店。shop 是店铺域名（binding 的 external_account），clientID / clientSecret 是应用凭据。
func New(t testing.TB, shop, clientID, clientSecret string) *Server {
	s := &Server{Shop: shop, clientID: clientID, secret: clientSecret, tokens: map[string]bool{}, nextID: 1000,
		products: map[string]*product{}, variants: map[string]*variant{}, items: map[string]*invItem{},
		idem: map[string]bool{}, calls: map[string]int{}, images: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/oauth/access_token", s.handleToken)
	mux.HandleFunc("POST /admin/api/2026-10/graphql.json", s.handleGraphQL)
	mux.HandleFunc("GET /images/{name}", s.handleImage)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// BaseURL 给适配器的 Options.BaseURL 用。
func (s *Server) BaseURL(string) string { return s.URL }

func (s *Server) id(kind string) string {
	s.nextID++
	return fmt.Sprintf("gid://shopify/%s/%d", kind, s.nextID)
}

// AddLocation 加一个 location，返回 gid。
func (s *Server) AddLocation(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.id("Location")
	s.locations = append(s.locations, g)
	return g
}

// AddProduct 放一件商品，返回商品 gid。
func (s *Server) AddProduct(p Product) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := &product{gid: s.id("Product"), p: p}
	for _, v := range p.Variants {
		s.addVariantLocked(pr, v)
	}
	s.products[pr.gid] = pr
	s.order = append(s.order, pr.gid)
	return pr.gid
}

func (s *Server) addVariantLocked(pr *product, v Variant) string {
	vg := s.id("ProductVariant")
	ig := s.id("InventoryItem")
	lv := map[string]int32{}
	for k, q := range v.Levels {
		lv[k] = q
	}
	s.items[ig] = &invItem{tracked: v.Tracked, levels: lv}
	s.variants[vg] = &variant{gid: vg, productGID: pr.gid, itemGID: ig, v: v}
	pr.variants = append(pr.variants, vg)
	return vg
}

// AddVariant 给已有商品加一个变体，返回变体 gid。
func (s *Server) AddVariant(productGID string, v Variant) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addVariantLocked(s.products[productGID], v)
}

// RemoveVariant 删一个变体。
func (s *Server) RemoveVariant(variantGID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.variants[variantGID]
	pr := s.products[v.productGID]
	for i, g := range pr.variants {
		if g == variantGID {
			pr.variants = append(pr.variants[:i], pr.variants[i+1:]...)
			break
		}
	}
	delete(s.variants, variantGID)
}

// SetTitle 改商品标题。
func (s *Server) SetTitle(productGID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.products[productGID].p.Title = title
}

// SetImages 换商品图。
func (s *Server) SetImages(productGID string, urls []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.products[productGID].p.Images = urls
}

// DeleteProduct 删商品。
func (s *Server) DeleteProduct(gid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.products[gid]
	if pr == nil {
		return
	}
	for _, v := range pr.variants {
		delete(s.variants, v)
	}
	delete(s.products, gid)
	for i, g := range s.order {
		if g == gid {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// VariantIDs 是一件商品的变体 gid（按加入顺序）。
func (s *Server) VariantIDs(productGID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.products[productGID].variants...)
}

// InventoryItem 是变体的 inventory item gid。
func (s *Server) InventoryItem(variantGID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.variants[variantGID].itemGID
}

// Available 是 (inventory item, location) 的 available。
func (s *Server) Available(itemGID, locationGID string) (int32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.items[itemGID]
	if it == nil {
		return 0, false
	}
	q, ok := it.levels[locationGID]
	return q, ok
}

// SetAvailable 模拟店员在 Shopify 后台改数。
func (s *Server) SetAvailable(itemGID, locationGID string, q int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[itemGID].levels[locationGID] = q
}

// Price 是变体的价格串。
func (s *Server) Price(variantGID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.variants[variantGID].v.Price
}

// ThrottleNext 让之后 n 次 GraphQL 调用回 THROTTLED。
func (s *Server) ThrottleNext(n int) { s.mu.Lock(); s.throttle = n; s.mu.Unlock() }

// ExpireTokens 让已发出的 token 全部失效（下一次调用 401）。
func (s *Server) ExpireTokens() { s.mu.Lock(); s.tokens = map[string]bool{}; s.mu.Unlock() }

// RotateSecret 换 client secret：旧 secret 换 token 回 400，已发出的 token 一并作废。
func (s *Server) RotateSecret(newSecret string) {
	s.mu.Lock()
	s.secret, s.tokens = newSecret, map[string]bool{}
	s.mu.Unlock()
}

// Webhooks 是已装的订阅。
func (s *Server) Webhooks() []WebhookSub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]WebhookSub(nil), s.webhooks...)
}

// AddWebhook 预先装一条订阅（测「已有不同地址」）。
func (s *Server) AddWebhook(w WebhookSub) {
	s.mu.Lock()
	s.webhooks = append(s.webhooks, w)
	s.mu.Unlock()
}

// Calls 是某个操作被调用的次数（GraphQL 按 operationName；换 token 是 "tokens"）。
func (s *Server) Calls(op string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls[op] }

// IdemKeys 是 SetQty 收到过的幂等键（按到达顺序）。
func (s *Server) IdemKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.idemSeen...)
}

// Sign 用当前 secret 算回调签名。
func (s *Server) Sign(body []byte) string {
	s.mu.Lock()
	sec := s.secret
	s.mu.Unlock()
	m := hmac.New(sha256.New, []byte(sec))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// WebhookRequest 造一条签好名的回调请求（目标 url）。
func (s *Server) WebhookRequest(url, topic, eventID string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	r.Header.Set("X-Shopify-Hmac-Sha256", s.Sign(body))
	r.Header.Set("X-Shopify-Topic", topic)
	r.Header.Set("X-Shopify-Shop-Domain", s.Shop)
	r.Header.Set("X-Shopify-Event-Id", eventID)
	r.Header.Set("Content-Type", "application/json")
	return r
}

// ImageURL 登记一张本地能下载的图（PNG 字节），返回它的 URL。
func (s *Server) ImageURL(name string, png []byte) string {
	s.mu.Lock()
	s.images[name] = png
	s.mu.Unlock()
	return s.URL + "/images/" + name
}

// ImageHits 是图片被下载的次数。
func (s *Server) ImageHits() int { s.mu.Lock(); defer s.mu.Unlock(); return s.imageHits }

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	b, ok := s.images[r.PathValue("name")]
	s.imageHits++
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(b)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GrantType    string `json:"grant_type"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls["tokens"]++
	if in.GrantType != "client_credentials" || in.ClientID != s.clientID || in.ClientSecret != s.secret {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
		return
	}
	s.nextID++
	tok := fmt.Sprintf("shpat_sim_%d", s.nextID)
	s.tokens[tok] = true
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "scope": "write_products,write_inventory,write_orders,write_assigned_fulfillment_orders", "expires_in": 86399})
}

type gqlReq struct {
	Query         string          `json:"query"`
	OperationName string          `json:"operationName"`
	Variables     json.RawMessage `json:"variables"`
}

func cost(available float64) map[string]any {
	return map[string]any{"cost": map[string]any{"requestedQueryCost": 10, "actualQueryCost": 10,
		"throttleStatus": map[string]any{"maximumAvailable": 4000.0, "currentlyAvailable": available, "restoreRate": 200.0}}}
}

func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var req gqlReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tokens[r.Header.Get("X-Shopify-Access-Token")] {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":"[API] Invalid API key or access token (unrecognized login or wrong password)"}`)
		return
	}
	s.calls[req.OperationName]++
	w.Header().Set("Content-Type", "application/json")
	if s.throttle > 0 {
		s.throttle--
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors":     []any{map[string]any{"message": "Throttled", "extensions": map[string]any{"code": "THROTTLED"}}},
			"extensions": cost(0)})
		return
	}
	if q := s.fail[req.OperationName]; len(q) > 0 {
		afterApply := q[0]
		s.fail[req.OperationName] = q[1:]
		if afterApply {
			_, _ = s.dispatch(req)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"errors":"Service Unavailable"}`)
		return
	}
	data, err := s.dispatch(req)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": err.Error()}}, "extensions": cost(3990)})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "extensions": cost(3990)})
}

func (s *Server) dispatch(req gqlReq) (any, error) {
	switch req.OperationName {
	case "Products":
		var v struct {
			First int     `json:"first"`
			After *string `json:"after"`
		}
		_ = json.Unmarshal(req.Variables, &v)
		start := 0
		if v.After != nil {
			start, _ = strconv.Atoi(*v.After)
		}
		end := min(start+v.First, len(s.order))
		nodes := []any{}
		for _, g := range s.order[start:end] {
			nodes = append(nodes, s.productJSON(s.products[g]))
		}
		return map[string]any{"products": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end < len(s.order), "endCursor": strconv.Itoa(end)}, "nodes": nodes}}, nil
	case "Product":
		var v struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req.Variables, &v)
		pr := s.products[v.ID]
		if pr == nil {
			return map[string]any{"product": nil}, nil
		}
		return map[string]any{"product": s.productJSON(pr)}, nil
	case "Levels":
		var v struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req.Variables, &v)
		it := s.items[v.ID]
		if it == nil {
			return map[string]any{"inventoryItem": nil}, nil
		}
		return map[string]any{"inventoryItem": map[string]any{"inventoryLevels": map[string]any{"nodes": levelsJSON(it)}}}, nil
	case "ItemLevels":
		var v struct {
			IDs []string `json:"ids"`
		}
		_ = json.Unmarshal(req.Variables, &v)
		if len(v.IDs) > 250 {
			return nil, fmt.Errorf("nodes(ids:) 最多 250 个")
		}
		nodes := []any{}
		for _, id := range v.IDs {
			it := s.items[id]
			if it == nil {
				nodes = append(nodes, nil)
				continue
			}
			nodes = append(nodes, map[string]any{"id": id, "inventoryLevels": map[string]any{"nodes": levelsJSON(it)}})
		}
		return map[string]any{"nodes": nodes}, nil
	case "SetQty":
		return s.setQty(req.Variables)
	case "SetPrices":
		return s.setPrices(req.Variables)
	case "Order":
		var v struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req.Variables, &v)
		o := s.orders[v.ID]
		if o == nil {
			return map[string]any{"order": nil}, nil
		}
		return map[string]any{"order": s.orderJSON(o)}, nil
	case "FulfillmentCreate":
		return s.fulfillmentCreate(req.Variables)
	case "Webhooks":
		nodes := []any{}
		for _, w := range s.webhooks {
			nodes = append(nodes, map[string]any{"topic": w.Topic, "uri": w.URI})
		}
		return map[string]any{"webhookSubscriptions": map[string]any{"nodes": nodes}}, nil
	case "CreateWebhook":
		var v struct {
			T string `json:"t"`
			U string `json:"u"`
		}
		_ = json.Unmarshal(req.Variables, &v)
		s.webhooks = append(s.webhooks, WebhookSub{Topic: v.T, URI: v.U})
		return map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}}, nil
	}
	return nil, fmt.Errorf("模拟平台不认识操作 %q", req.OperationName)
}

func levelsJSON(it *invItem) []any {
	locs := make([]string, 0, len(it.levels))
	for l := range it.levels {
		locs = append(locs, l)
	}
	sort.Strings(locs)
	out := []any{}
	for _, l := range locs {
		out = append(out, map[string]any{"location": map[string]any{"id": l},
			"quantities": []any{map[string]any{"quantity": it.levels[l]}}})
	}
	return out
}

func (s *Server) productJSON(pr *product) map[string]any {
	status := pr.p.Status
	if status == "" {
		status = "ACTIVE"
	}
	media := []any{}
	for _, u := range pr.p.Images {
		media = append(media, map[string]any{"image": map[string]any{"url": u}})
	}
	vs := []any{}
	for _, g := range pr.variants {
		v := s.variants[g]
		var sku any
		if v.v.SKU != "" {
			sku = v.v.SKU
		}
		opts := []any{}
		if len(v.v.Options) == 0 {
			opts = append(opts, map[string]any{"name": "Title", "value": "Default Title"})
		}
		names := make([]string, 0, len(v.v.Options))
		for n := range v.v.Options {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			opts = append(opts, map[string]any{"name": n, "value": v.v.Options[n]})
		}
		it := s.items[v.itemGID]
		vs = append(vs, map[string]any{"id": g, "sku": sku, "price": v.v.Price, "selectedOptions": opts, "image": nil,
			"inventoryItem": map[string]any{"id": v.itemGID, "tracked": it.tracked}})
	}
	return map[string]any{"id": pr.gid, "title": pr.p.Title, "descriptionHtml": pr.p.Description, "status": status,
		"isGiftCard": pr.p.GiftCard, "media": map[string]any{"nodes": media}, "variants": map[string]any{"nodes": vs}}
}

func (s *Server) setQty(raw json.RawMessage) (any, error) {
	var v struct {
		In struct {
			Name       string `json:"name"`
			Quantities []struct {
				InventoryItemID    string `json:"inventoryItemId"`
				LocationID         string `json:"locationId"`
				Quantity           int32  `json:"quantity"`
				ChangeFromQuantity *int32 `json:"changeFromQuantity"`
			} `json:"quantities"`
		} `json:"in"`
		K string `json:"k"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	s.idemSeen = append(s.idemSeen, v.K)
	ok := map[string]any{"inventorySetQuantities": map[string]any{"inventoryAdjustmentGroup": map[string]any{"id": "gid://shopify/InventoryAdjustmentGroup/1"}, "userErrors": []any{}}}
	if s.idem[v.K] {
		return ok, nil
	}
	var errs []any
	for i, q := range v.In.Quantities {
		it := s.items[q.InventoryItemID]
		if it == nil {
			errs = append(errs, map[string]any{"field": []string{"input", "quantities", strconv.Itoa(i), "inventoryItemId"},
				"message": "The specified inventory item could not be found.", "code": "INVALID_INVENTORY_ITEM"})
			continue
		}
		cur, stocked := it.levels[q.LocationID]
		if !stocked {
			errs = append(errs, map[string]any{"field": []string{"input", "quantities", strconv.Itoa(i), "locationId"},
				"message": "The specified inventory item is not stocked at the location.", "code": "ITEM_NOT_STOCKED_AT_LOCATION"})
			continue
		}
		if q.ChangeFromQuantity != nil && *q.ChangeFromQuantity != cur {
			errs = append(errs, map[string]any{"field": []string{"input", "quantities", strconv.Itoa(i), "changeFromQuantity"},
				"message": "The changeFromQuantity argument no longer matches the persisted quantity.", "code": "CHANGE_FROM_QUANTITY_STALE"})
		}
	}
	if len(errs) > 0 {
		return map[string]any{"inventorySetQuantities": map[string]any{"inventoryAdjustmentGroup": nil, "userErrors": errs}}, nil
	}
	for _, q := range v.In.Quantities {
		s.items[q.InventoryItemID].levels[q.LocationID] = q.Quantity
	}
	s.idem[v.K] = true
	return ok, nil
}

func (s *Server) setPrices(raw json.RawMessage) (any, error) {
	var v struct {
		PID string `json:"pid"`
		VS  []struct {
			ID    string `json:"id"`
			Price string `json:"price"`
		} `json:"vs"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var errs []any
	for i, x := range v.VS {
		vv := s.variants[x.ID]
		if vv == nil || vv.productGID != v.PID {
			errs = append(errs, map[string]any{"field": []string{"variants", strconv.Itoa(i), "id"}, "message": "Product variant does not exist"})
		}
	}
	if len(errs) == 0 {
		for _, x := range v.VS {
			s.variants[x.ID].v.Price = x.Price
		}
	}
	if errs == nil {
		errs = []any{}
	}
	return map[string]any{"productVariantsBulkUpdate": map[string]any{"userErrors": errs}}, nil
}
