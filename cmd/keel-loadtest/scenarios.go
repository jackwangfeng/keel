package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"time"
)

type scenario struct {
	desc      string
	needUsers bool
	needAdmin bool
	step      func(w *worker)
	setup     func(cfg *config) // 非空表示这是一次性准备动作，不压测
}

// 搜索词：常见词（大量命中）、长尾（几条到几十条命中）、无结果。比例 6 : 3 : 1。
var (
	commonWords = []string{"连衣裙", "蓝牙耳机", "充电宝", "四件套", "咖啡豆", "面膜", "瑜伽垫", "保温杯", "口红", "卫衣",
		"巧克力", "跑步鞋", "电饭煲", "抱枕", "纸尿裤", "绿茶", "衬衫", "帐篷", "洗发水", "机械键盘"}
	tailWords = []string{"栖木复古地毯", "云朵静音蓝牙耳机", "白鹭纯棉四件套", "北屿不锈钢保温杯", "麦田有机绿茶",
		"海盐迷你精华液", "星河防水登山包", "鹿角手工砧板", "远山透气跑步鞋", "松果进口坚果礼盒", "初见陶瓷炒锅", "素野竹制收纳盒"}
	noneWords = []string{"量子计算机", "火箭发动机", "钻石项链", "二手挖掘机", "直升机"}
)

func (w *worker) store() *storeFix {
	if w.user != nil {
		if s := w.cfg.fx.storeByID[w.user.StoreID]; s != nil {
			return s
		}
	}
	return &w.cfg.fx.Stores[w.rng.IntN(len(w.cfg.fx.Stores))]
}

func (w *worker) pick(ids []int64) int64 { return ids[w.rng.IntN(len(ids))] }

func (w *worker) randPID() int64 {
	fx := w.cfg.fx
	return fx.ProductIDMin + w.rng.Int64N(fx.ProductIDMax-fx.ProductIDMin+1)
}

func (w *worker) idem() string {
	w.n++
	return fmt.Sprintf("lt-%d-%d-%d", time.Now().UnixNano(), w.id, w.n)
}

func (w *worker) items(st *storeFix, max int) string {
	k := 1 + w.rng.IntN(max)
	seen := map[int64]bool{}
	s := "["
	for i := 0; i < k; i++ {
		id := w.pick(st.SKUs)
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(seen) > 1 {
			s += ","
		}
		s += fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, id)
	}
	return s + "]"
}

// placeOrder 下单，cfg.pay 时接着发起沙箱支付并把签好名的回调投回去。
func (w *worker) placeOrder(prefix string, storeID int64, items string) {
	body := fmt.Sprintf(`{"items":%s,"address_id":%d,"store_id":%d}`, items, w.user.AddressID, storeID)
	st, b := w.call(prefix+"POST /orders", "POST", "/orders", body, "user", map[string]string{"Idempotency-Key": w.idem()})
	if st != 201 || !w.cfg.pay {
		return
	}
	var o struct {
		OrderNo string `json:"order_no"`
	}
	if json.Unmarshal(b, &o) != nil || o.OrderNo == "" {
		return
	}
	st, b = w.call(prefix+"POST /payments", "POST", "/orders/"+o.OrderNo+"/payments", `{"channel":"wechat"}`, "user",
		map[string]string{"Idempotency-Key": w.idem()})
	if st != 201 {
		return
	}
	var pi struct {
		Payload struct {
			Settle struct {
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
			} `json:"settle"`
		} `json:"payload"`
	}
	if json.Unmarshal(b, &pi) != nil || pi.Payload.Settle.URL == "" {
		return
	}
	w.call(prefix+"POST /webhooks/pay", "POST", pi.Payload.Settle.URL, pi.Payload.Settle.Body, "", pi.Payload.Settle.Headers)
}

func listQuery(w *worker, extra string) {
	st := w.store()
	page := 1 + w.rng.IntN(3)
	w.call("GET /products"+extra, "GET", fmt.Sprintf("/products?store_id=%d&page=%d%s", st.ID, page, extra), "", "", nil)
}

var scenarios = map[string]scenario{
	"list-default": {desc: "商品列表默认排序（前 3 页）", needUsers: true, step: func(w *worker) { listQuery(w, "") }},
	"list-category": {desc: "商品列表按类目（顶层 / 叶子各半）", needUsers: true, step: func(w *worker) {
		fx := w.cfg.fx
		c := w.pick(fx.TopCategoryIDs)
		label := "GET /products?category(top)"
		if w.rng.IntN(2) == 0 {
			c = w.pick(fx.LeafCategories)
			label = "GET /products?category(leaf)"
		}
		w.call(label, "GET", fmt.Sprintf("/products?store_id=%d&category_id=%d&page=%d", w.store().ID, c, 1+w.rng.IntN(3)), "", "", nil)
	}},
	"list-instock": {desc: "商品列表只看有货", needUsers: true, step: func(w *worker) { listQuery(w, "&in_stock_only=true") }},
	"list-deep": {desc: "商品列表深分页 page=50", needUsers: true, step: func(w *worker) {
		w.call("GET /products?page=50", "GET", fmt.Sprintf("/products?store_id=%d&page=50", w.store().ID), "", "", nil)
	}},
	"detail": {desc: "商品详情（随机商品，带门店）", needUsers: true, step: func(w *worker) {
		w.call("GET /products/{id}", "GET", fmt.Sprintf("/products/%d?store_id=%d", w.randPID(), w.store().ID), "", "", nil)
	}},
	"resolve": {desc: "门店解析（买家地址坐标，5% 在围栏外）", needUsers: true, step: func(w *worker) {
		w.call("GET /stores/resolve", "GET", fmt.Sprintf("/stores/resolve?lat=%f&lng=%f", w.user.Lat, w.user.Lng), "", "", nil)
	}},
	"addresses": {desc: "地址簿（带 store_id 判配送范围）", needUsers: true, step: func(w *worker) {
		w.call("GET /addresses?store_id", "GET", fmt.Sprintf("/addresses?store_id=%d", w.store().ID), "", "user", nil)
	}},
	"search": {desc: "关键词搜索：常见 / 长尾 / 无结果 = 6:3:1", needUsers: true, step: func(w *worker) {
		r := w.rng.IntN(10)
		q, label := w.pickWord(commonWords), "POST /search(常见)"
		switch {
		case r >= 9:
			q, label = w.pickWord(noneWords), "POST /search(无结果)"
		case r >= 6:
			q, label = w.pickWord(tailWords), "POST /search(长尾)"
		}
		w.call(label, "POST", "/search", fmt.Sprintf(`{"query":%q,"store_id":%d}`, q, w.store().ID), "", nil)
	}},
	"cart": {desc: "购物车：加 → 改数量 → 看 → 删", needUsers: true, step: func(w *worker) {
		st := w.store()
		sku := w.pick(st.SKUs)
		q := fmt.Sprintf("?store_id=%d&address_id=%d", st.ID, w.user.AddressID)
		code, b := w.call("POST /cart/items", "POST", "/cart/items"+q, fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, sku), "user",
			map[string]string{"Idempotency-Key": w.idem()})
		if code != 200 {
			return
		}
		var cart struct {
			Items []struct {
				ID    int64 `json:"id"`
				SkuID int64 `json:"sku_id"`
			} `json:"items"`
		}
		_ = json.Unmarshal(b, &cart)
		var item int64
		for _, it := range cart.Items {
			if it.SkuID == sku {
				item = it.ID
			}
		}
		if item == 0 {
			return
		}
		w.call("PATCH /cart/items/{id}", "PATCH", fmt.Sprintf("/cart/items/%d%s", item, q), `{"quantity":2}`, "user", nil)
		w.call("GET /cart", "GET", "/cart"+q, "", "user", nil)
		w.call("DELETE /cart/items/{id}", "DELETE", fmt.Sprintf("/cart/items/%d", item), "", "user", nil)
	}},
	"preview": {desc: "结算试算（1–3 个有货 SKU）", needUsers: true, step: func(w *worker) {
		st := w.store()
		w.call("POST /orders/preview", "POST", "/orders/preview",
			fmt.Sprintf(`{"items":%s,"address_id":%d,"store_id":%d}`, w.items(st, 3), w.user.AddressID, st.ID), "user", nil)
	}},
	"order": {desc: "下单（分散 SKU，1–2 件）+ 沙箱支付回调", needUsers: true, step: func(w *worker) {
		st := w.store()
		w.placeOrder("", st.ID, w.items(st, 2))
	}},
	"order-hot": {desc: "热点：所有人抢默认店同一个 SKU（hot_sku）", needUsers: true, step: func(w *worker) {
		w.placeOrder("hot ", w.cfg.fx.DefaultStoreID, fmt.Sprintf(`[{"sku_id":%d,"quantity":1}]`, w.cfg.fx.HotSKU))
	}},
	"order-flash": {desc: "秒杀：所有人抢同一个秒杀活动的 SKU（先跑 setup-flash）", needUsers: true, step: func(w *worker) {
		sku := w.cfg.flashSKU
		if sku == 0 {
			sku = w.cfg.fx.HotSKU
		}
		w.placeOrder("flash ", w.cfg.fx.DefaultStoreID, fmt.Sprintf(`[{"sku_id":%d,"quantity":1}]`, sku))
	}},
	"setup-flash": {desc: "（准备）建一个秒杀活动：hot_sku，¥1，配额 5 万件，不限购", needAdmin: true, setup: setupFlash},
	"admin-orders": {desc: "后台订单列表（前 20 页）", needAdmin: true, step: func(w *worker) {
		w.call("GET /admin/orders", "GET", fmt.Sprintf("/admin/orders?page=%d&page_size=20", 1+w.rng.IntN(20)), "", "admin", nil)
	}},
	"admin-orders-search": {desc: "后台订单按手机号 / 单号查（各半）", needAdmin: true, step: func(w *worker) {
		fx := w.cfg.fx
		i := w.rng.IntN(len(fx.OrderNos))
		if w.rng.IntN(2) == 0 {
			w.call("GET /admin/orders?phone", "GET", "/admin/orders?phone="+url.QueryEscape(fx.Phones[i]), "", "admin", nil)
		} else {
			w.call("GET /admin/orders?order_no", "GET", "/admin/orders?order_no="+fx.OrderNos[i], "", "admin", nil)
		}
	}},
	"admin-products": {desc: "后台商品列表（前 50 页）", needAdmin: true, step: func(w *worker) {
		w.call("GET /admin/products", "GET", fmt.Sprintf("/admin/products?page=%d&page_size=20", 1+w.rng.IntN(50)), "", "admin", nil)
	}},
	"admin-stock-same": {desc: "后台并发调整同一个 SKU 的库存（±1 交替）", needAdmin: true, step: func(w *worker) {
		st := w.cfg.fx.Stores[0]
		adjust(w, "adjust(同一 SKU)", st.ID, st.SKUs[0])
	}},
	"admin-stock-diff": {desc: "后台并发调整不同 SKU 的库存（随机店随机 SKU，±1 交替）", needAdmin: true, step: func(w *worker) {
		st := w.cfg.fx.Stores[w.rng.IntN(len(w.cfg.fx.Stores))]
		adjust(w, "adjust(不同 SKU)", st.ID, w.pick(st.SKUs))
	}},
}

// mix 在 init 里登记：它的步骤要引用 scenarios 本身，写在字面量里是初始化环。
func init() {
	scenarios["mix"] = scenario{desc: "混合：70% 读 / 20% 搜索+购物车+试算 / 10% 下单（含支付）", needUsers: true, step: mixStep}
}

func (w *worker) pickWord(ws []string) string { return ws[w.rng.IntN(len(ws))] }

func adjust(w *worker, label string, store, sku int64) {
	delta := 1
	if w.n%2 == 1 {
		delta = -1
	}
	w.call("POST "+label, "POST", fmt.Sprintf("/admin/stores/%d/skus/%d/inventory/adjustments", store, sku),
		fmt.Sprintf(`{"delta":%d,"reason":"压测"}`, delta), "admin", map[string]string{"Idempotency-Key": w.idem()})
}

// mixStep 按权重挑一个动作。权重是「一次动作」的比例，购物车一次动作含 4 个请求、下单含 3 个。
func mixStep(w *worker) {
	r := w.rng.IntN(100)
	switch {
	case r < 25:
		scenarios["list-default"].step(w)
	case r < 35:
		scenarios["list-category"].step(w)
	case r < 40:
		scenarios["list-instock"].step(w)
	case r < 42:
		scenarios["list-deep"].step(w)
	case r < 62:
		scenarios["detail"].step(w)
	case r < 66:
		scenarios["resolve"].step(w)
	case r < 70:
		scenarios["addresses"].step(w)
	case r < 80:
		scenarios["search"].step(w)
	case r < 85:
		scenarios["cart"].step(w)
	case r < 90:
		scenarios["preview"].step(w)
	default:
		scenarios["order"].step(w)
	}
}

var flashQuota = 50000

func setupFlash(cfg *config) {
	sku := cfg.fx.HotSKU
	body := fmt.Sprintf(`{"name":"压测秒杀","promotion_type":4,"starts_at":%q,"ends_at":%q,
		"skus":[{"sku_id":%d,"promo_price_cents":100,"per_user_limit":0,"stock_qty":%d}]}`,
		time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339), sku, flashQuota)
	st, b, _, err := doReq(cfg.client, "POST", cfg.base+"/api/v1/admin/promotions", body, map[string]string{
		"Authorization": "Bearer " + cfg.adminToken, "Idempotency-Key": "lt-flash-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	if err != nil || st != 201 {
		log.Fatalf("建秒杀活动失败：%d %v %.500s", st, err, b)
	}
	// 建出来是草稿（status 0），要再上线一次才生效。
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(b, &created); err != nil || created.ID == 0 {
		log.Fatalf("建秒杀活动的响应里没有 id：%.300s", b)
	}
	st, b, _, err = doReq(cfg.client, "PATCH", fmt.Sprintf("%s/api/v1/admin/promotions/%d", cfg.base, created.ID), `{"status":1}`,
		map[string]string{"Authorization": "Bearer " + cfg.adminToken, "Idempotency-Key": "lt-flash-on-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	if err != nil || st != 200 {
		log.Fatalf("上线秒杀活动失败：%d %v %.500s", st, err, b)
	}
	fmt.Printf("秒杀活动已上线：id=%d sku=%d 配额 %d\n", created.ID, sku, flashQuota)
}
