// Command seed 给性能压测造数据（docs/性能压测-2026-10.md，用法见 scripts/loadtest/README.md）。
//
// 规模照 docs/电商系统-总体架构.md「九、性能与容量目标」：单商家 10 万件商品（每件 1–3 个 SKU）、
// 几十家带围栏的门店（同一个城市里）、库存行、几千个买家、若干万历史订单。
//
// **只给一次性的私有压测栈用**：直接以管理员账号（keel，超级用户）写库，绕过 RLS 写入，
// 但每一行都带正确的 merchant_id，应用账号 keel_app 在租户上下文里读得到、改得动。
// 拆分形态（compose.split.yaml）下库存行写进库存库，core 里的「门店 × 商品有没有货」冗余标记
// （product_store_stock）按同一份数量当场算好写进去，与库存服务的判据一致（任一在售 SKU 可售数 > 0）。
// 单体形态把 -inv-dsn 设成与 -core-dsn 相同即可（库存四张表在同一个库里）。
//
// 买家的口令统一是种子买家那一个（keel-demo-2026，同一个 argon2 散列）：短信验证码登录在没接短信服务的栈上
// 是 501，契约里没有别的注册入口，于是买家行直接写库，压测器再按契约 POST /auth/login 换令牌。
//
// 跑完把压测器要用的 ID 写进 -out 指定的 JSON（fixture）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/search"
)

// demoPasswordHash 是 db/seed/single.sql 里种子买家的口令散列（明文 keel-demo-2026）。
const demoPasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$J4kXTFFYK0Ts2p5Co+JeQg$RKPijLxGoS00y5FOKXGh260eD1CC5AT6IlGEo0vC7qY"

// DemoPassword 是上面那个散列的明文，写进 fixture 供压测器登录。
const DemoPassword = "keel-demo-2026"

type catDef struct {
	top    string
	leaves []string
	specK  string
	specV  []string
	priceL int64 // 分
	priceH int64
}

var catalog = []catDef{
	{"女装", []string{"连衣裙", "半身裙", "衬衫", "针织衫", "大衣"}, "尺码", []string{"S", "M", "L"}, 5900, 89900},
	{"男装", []string{"T恤", "夹克", "休闲裤", "卫衣", "西装"}, "尺码", []string{"M", "L", "XL"}, 4900, 99900},
	{"家居", []string{"四件套", "香薰蜡烛", "地毯", "收纳盒", "抱枕"}, "规格", []string{"标准", "加大", "两件装"}, 1900, 129900},
	{"数码配件", []string{"蓝牙耳机", "充电宝", "机械键盘", "数据线", "手机壳"}, "颜色", []string{"黑色", "白色", "灰色"}, 1500, 69900},
	{"食品饮料", []string{"巧克力", "坚果礼盒", "绿茶", "咖啡豆", "果干"}, "规格", []string{"250g", "500g", "1kg"}, 990, 39900},
	{"美妆", []string{"口红", "面霜", "精华液", "防晒霜", "面膜"}, "规格", []string{"小样", "正装", "套装"}, 2900, 59900},
	{"母婴", []string{"纸尿裤", "奶瓶", "婴儿湿巾", "爬行垫", "儿童水杯"}, "规格", []string{"S码", "M码", "L码"}, 1900, 49900},
	{"运动户外", []string{"瑜伽垫", "跑步鞋", "登山包", "帐篷", "运动水壶"}, "颜色", []string{"墨绿", "藏青", "橙色"}, 2900, 159900},
	{"厨具", []string{"炒锅", "刀具套装", "保温杯", "电饭煲", "砧板"}, "规格", []string{"小号", "中号", "大号"}, 2900, 89900},
	{"个护", []string{"电动牙刷", "洗发水", "沐浴露", "剃须刀", "吹风机"}, "规格", []string{"标准装", "家庭装", "旅行装"}, 1900, 79900},
}

var brands = []string{"栖木", "云朵", "山茶", "白鹭", "青禾", "知秋", "半岛", "南风", "北屿", "鹿角",
	"拾光", "素野", "橙子", "木槿", "远山", "初见", "星河", "麦田", "海盐", "松果"}

var adjs = []string{"新款", "经典", "轻奢", "简约", "复古", "北欧", "加厚", "便携", "大容量", "静音",
	"无线", "有机", "进口", "手工", "限定", "高端", "家用", "迷你", "防水", "透气",
	"柔软", "纯棉", "不锈钢", "陶瓷", "竹制", "磨砂", "亮面", "多功能", "智能", "儿童"}

var subtitles = []string{"夏季新款 显瘦", "七天无理由", "包邮 现货速发", "官方正品", "限时特惠", "人气爆款",
	"品质保证 售后无忧", "门店同款", "热销 好评如潮", "新品首发", "经典回归", "日常百搭"}

// 杭州城区一块矩形：门店、买家地址都落在这里。
const (
	latLo, latHi = 30.18, 30.36
	lngLo, lngHi = 120.02, 120.32
)

type storeFix struct {
	ID   int64   `json:"id"`
	Code string  `json:"code"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
	// SKUs 是这家店里有货的 SKU 抽样（下单 / 购物车用）。
	SKUs []int64 `json:"skus"`
}

type userFix struct {
	ID        int64   `json:"id"`
	Phone     string  `json:"phone"`
	AddressID int64   `json:"address_id"`
	StoreID   int64   `json:"store_id"` // 默认地址落在哪家店的围栏里；不在任何围栏里是默认店
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
}

// Fixture 是压测器的输入。
type Fixture struct {
	MerchantID     int64      `json:"merchant_id"`
	DefaultStoreID int64      `json:"default_store_id"`
	Password       string     `json:"password"`
	ProductIDMin   int64      `json:"product_id_min"`
	ProductIDMax   int64      `json:"product_id_max"`
	TopCategoryIDs []int64    `json:"top_category_ids"`
	LeafCategories []int64    `json:"leaf_category_ids"`
	Stores         []storeFix `json:"stores"`
	Users          []userFix  `json:"users"`
	// HotSKU 在每家店都有 1000 万件，热点下单用；HotStoreSKUs 是各店抽出来做「后台改同一个 SKU」的那一行。
	HotSKU   int64    `json:"hot_sku"`
	OrderNos []string `json:"order_nos"` // 历史订单号抽样（后台按单号查）
	Phones   []string `json:"receiver_phones"`
}

func main() {
	coreDSN := flag.String("core-dsn", "", "core 库（管理员账号），如 postgres://keel:keel@192.168.16.3:5432/keel?sslmode=disable")
	invDSN := flag.String("inv-dsn", "", "库存库（管理员账号）；单体形态与 -core-dsn 相同")
	nProducts := flag.Int("products", 100000, "商品数")
	nStores := flag.Int("stores", 40, "门店数（不含默认店）")
	storeShare := flag.Float64("store-share", 0.3, "每家门店铺货的 SKU 比例（默认店铺全部）")
	nUsers := flag.Int("users", 5000, "买家数")
	nOrders := flag.Int("orders", 50000, "历史订单数")
	out := flag.String("out", "loadtest-fixture.json", "fixture 输出路径")
	seed := flag.Uint64("seed", 20261001, "随机种子（同样的参数造同样的数据）")
	force := flag.Bool("force", false, "库里已有大量商品时仍然继续")
	flag.Parse()
	if *coreDSN == "" || *invDSN == "" {
		log.Fatal("要给 -core-dsn 与 -inv-dsn")
	}
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15))

	core, err := pgx.Connect(ctx, *coreDSN)
	must(err)
	defer core.Close(ctx)
	inv, err := pgx.Connect(ctx, *invDSN)
	must(err)
	defer inv.Close(ctx)

	start := time.Now()
	fx := Fixture{Password: DemoPassword}
	var defaultRegion int64
	must(core.QueryRow(ctx, `SELECT m.id, st.id, st.region_id FROM merchants m
		JOIN stores st ON st.merchant_id = m.id AND st.is_default AND st.deleted_at IS NULL
		WHERE m.code = 'demo'`).Scan(&fx.MerchantID, &fx.DefaultStoreID, &defaultRegion))
	mid := fx.MerchantID
	var existing int
	must(core.QueryRow(ctx, `SELECT count(*) FROM products WHERE merchant_id = $1`, mid).Scan(&existing))
	if existing > 1000 && !*force {
		log.Fatalf("库里已经有 %d 件商品，像是造过一次了；确要再造加 -force", existing)
	}

	// ---- 类目 ----
	var leafIDs []int64
	leafOf := map[int64]catDef{}
	leafName := map[int64]string{}
	for i, c := range catalog {
		var top int64
		must(core.QueryRow(ctx, `INSERT INTO categories (merchant_id, name, path, level, sort_order, status)
			VALUES ($1, $2, '', 1, $3, 1) RETURNING id`, mid, "压测-"+c.top, 100+i).Scan(&top))
		_, err := core.Exec(ctx, `UPDATE categories SET path = '/' || id || '/' WHERE id = $1`, top)
		must(err)
		fx.TopCategoryIDs = append(fx.TopCategoryIDs, top)
		for j, l := range c.leaves {
			var id int64
			must(core.QueryRow(ctx, `INSERT INTO categories (merchant_id, parent_id, name, path, level, sort_order, status)
				VALUES ($1, $2, $3, '', 2, $4, 1) RETURNING id`, mid, top, l, j).Scan(&id))
			_, err := core.Exec(ctx, `UPDATE categories SET path = '/' || $2::bigint || '/' || id || '/' WHERE id = $1`, id, top)
			must(err)
			leafIDs = append(leafIDs, id)
			leafOf[id] = c
			leafName[id] = l
		}
	}
	fx.LeafCategories = leafIDs
	logf(start, "类目 %d 个顶层 / %d 个叶子", len(catalog), len(leafIDs))

	// ---- 商品 ----
	type prod struct {
		leaf  int64
		title string
		price int64
		nSKU  int
	}
	prods := make([]prod, *nProducts)
	now := time.Now()
	rows := make([][]any, 0, *nProducts)
	for i := range prods {
		leaf := leafIDs[rng.IntN(len(leafIDs))]
		c := leafOf[leaf]
		title := fmt.Sprintf("%s%s%s %c%d", brands[rng.IntN(len(brands))], adjs[rng.IntN(len(adjs))], leafName[leaf],
			'A'+rune(rng.IntN(26)), 100+rng.IntN(900))
		sub := subtitles[rng.IntN(len(subtitles))]
		price := (c.priceL + rng.Int64N(c.priceH-c.priceL)) / 10 * 10
		prods[i] = prod{leaf, title, price, 1 + rng.IntN(3)}
		pub := now.Add(-time.Duration(rng.Int64N(int64(365 * 24 * time.Hour))))
		st := search.ProductText{Title: title, Subtitle: sub}.SearchText()
		// 销量偏态：多数个位数，少数几千。
		sales := int(math.Floor(math.Pow(rng.Float64(), 4) * 5000))
		rows = append(rows, []any{mid, leaf, title, sub, sales, int16(1), pub, st, pub})
	}
	n, err := core.CopyFrom(ctx, pgx.Identifier{"products"},
		[]string{"merchant_id", "category_id", "title", "subtitle", "sales_count", "status", "published_at", "search_text", "created_at"},
		pgx.CopyFromRows(rows))
	must(err)
	productIDs := idsAfter(ctx, core, `SELECT id FROM products WHERE merchant_id = $1 AND category_id = ANY($2) ORDER BY id`, mid, leafIDs)
	if len(productIDs) != int(n) {
		log.Fatalf("商品回读 %d 行，写入 %d 行", len(productIDs), n)
	}
	fx.ProductIDMin, fx.ProductIDMax = productIDs[0], productIDs[len(productIDs)-1]
	logf(start, "商品 %d 件（id %d–%d）", n, fx.ProductIDMin, fx.ProductIDMax)

	// ---- SKU ----
	type sku struct {
		id, product int64
		price       int64
		title       string
		spec        string
	}
	rows = rows[:0]
	for i, p := range prods {
		c := leafOf[p.leaf]
		for k := 0; k < p.nSKU; k++ {
			spec, _ := json.Marshal(map[string]string{c.specK: c.specV[k]})
			rows = append(rows, []any{mid, productIDs[i], fmt.Sprintf("LT%d-%d", productIDs[i], k+1), string(spec),
				p.price + int64(k)*1000, int16(1), 100 + rng.IntN(2000)})
		}
	}
	n, err = core.CopyFrom(ctx, pgx.Identifier{"skus"},
		[]string{"merchant_id", "product_id", "sku_code", "spec_values", "price_cents", "status", "weight_gram"},
		pgx.CopyFromRows(rows))
	must(err)
	var skus []sku
	r, err := core.Query(ctx, `SELECT s.id, s.product_id, s.price_cents, p.title, s.spec_values::text
		FROM skus s JOIN products p ON p.id = s.product_id
		WHERE s.merchant_id = $1 AND s.product_id BETWEEN $2 AND $3 ORDER BY s.id`, mid, fx.ProductIDMin, fx.ProductIDMax)
	must(err)
	for r.Next() {
		var s sku
		must(r.Scan(&s.id, &s.product, &s.price, &s.title, &s.spec))
		skus = append(skus, s)
	}
	must(r.Err())
	fx.HotSKU = skus[0].id
	logf(start, "SKU %d 个", n)

	// ---- 门店（带围栏）----
	var region int64
	must(core.QueryRow(ctx, `INSERT INTO regions (merchant_id, code, name) VALUES ($1, 'lt-hz', '压测-杭州')
		RETURNING id`, mid).Scan(&region))
	cols := int(math.Ceil(math.Sqrt(float64(*nStores) * 1.6)))
	rowsN := int(math.Ceil(float64(*nStores) / float64(cols)))
	dLat, dLng := (latHi-latLo)/float64(rowsN), (lngHi-lngLo)/float64(cols)
	type storeGeo struct {
		id                   int64
		lat, lng, hLat, hLng float64
	}
	var geos []storeGeo
	for i := 0; i < *nStores; i++ {
		lat := latLo + dLat*(float64(i/cols)+0.5) + (rng.Float64()-0.5)*dLat*0.3
		lng := lngLo + dLng*(float64(i%cols)+0.5) + (rng.Float64()-0.5)*dLng*0.3
		// 围栏是门店周围的一个矩形，比格子大一圈，相邻门店有重叠（重叠区按距离挑）。
		hLat, hLng := dLat*0.65, dLng*0.65
		poly := fmt.Sprintf("POLYGON((%f %f,%f %f,%f %f,%f %f,%f %f))",
			lng-hLng, lat-hLat, lng+hLng, lat-hLat, lng+hLng, lat+hLat, lng-hLng, lat+hLat, lng-hLng, lat-hLat)
		code := fmt.Sprintf("lt-%03d", i+1)
		var id int64
		must(core.QueryRow(ctx, `INSERT INTO stores (merchant_id, region_id, code, name, phone, province, city, district, address,
				location, fence, status)
			VALUES ($1, $2, $3, $4, '0571-88000000', '浙江省', '杭州市', '西湖区', $5,
				ST_GeogFromText($6), ST_GeogFromText($7), 1) RETURNING id`,
			mid, region, code, fmt.Sprintf("压测门店 %03d", i+1), fmt.Sprintf("压测路 %d 号", i+1),
			fmt.Sprintf("SRID=4326;POINT(%f %f)", lng, lat), "SRID=4326;"+poly).Scan(&id))
		geos = append(geos, storeGeo{id, lat, lng, hLat, hLng})
	}
	logf(start, "门店 %d 家（%d×%d 格，围栏有重叠）", len(geos), rowsN, cols)

	// ---- 库存（库存库）与有货标记（core）----
	type key struct{ store, product int64 }
	inStock := map[key]bool{}
	rows = rows[:0]
	storeSKUs := map[int64][]int64{}
	addInv := func(store int64, s sku, qty int) {
		rows = append(rows, []any{s.id, store, mid, qty, 5})
		k := key{store, s.product}
		inStock[k] = inStock[k] || qty > 0
		if qty > 0 {
			storeSKUs[store] = append(storeSKUs[store], s.id)
		}
	}
	for i, s := range skus {
		qty := 50 + rng.IntN(450)
		if rng.IntN(20) == 0 {
			qty = 0
		}
		if i == 0 {
			qty = 10_000_000
		}
		addInv(fx.DefaultStoreID, s, qty)
	}
	for _, g := range geos {
		for i, s := range skus {
			if i != 0 && rng.Float64() >= *storeShare {
				continue
			}
			qty := rng.IntN(100)
			if rng.IntN(10) == 0 {
				qty = 0
			}
			if i == 0 {
				qty = 10_000_000
			}
			addInv(g.id, s, qty)
		}
	}
	n, err = inv.CopyFrom(ctx, pgx.Identifier{"inventories"},
		[]string{"sku_id", "store_id", "merchant_id", "available_qty", "warning_qty"}, pgx.CopyFromRows(rows))
	must(err)
	logf(start, "库存行 %d 行（库存库）", n)
	rows = rows[:0]
	for k, v := range inStock {
		rows = append(rows, []any{k.store, k.product, mid, v})
	}
	n, err = core.CopyFrom(ctx, pgx.Identifier{"product_store_stock"},
		[]string{"store_id", "product_id", "merchant_id", "in_stock"}, pgx.CopyFromRows(rows))
	must(err)
	logf(start, "有货标记 %d 行（core）", n)

	sample := func(ids []int64, k int) []int64 {
		if len(ids) <= k {
			return ids
		}
		outIDs := make([]int64, k)
		for i := range outIDs {
			outIDs[i] = ids[rng.IntN(len(ids))]
		}
		return outIDs
	}
	fx.Stores = append(fx.Stores, storeFix{ID: fx.DefaultStoreID, Code: "default", SKUs: sample(storeSKUs[fx.DefaultStoreID], 3000)})
	for i, g := range geos {
		fx.Stores = append(fx.Stores, storeFix{ID: g.id, Code: fmt.Sprintf("lt-%03d", i+1), Lat: g.lat, Lng: g.lng,
			SKUs: sample(storeSKUs[g.id], 3000)})
	}

	// ---- 买家与地址 ----
	rows = rows[:0]
	phones := make([]string, *nUsers)
	for i := range phones {
		phones[i] = fmt.Sprintf("139%08d", 10000+i)
		rows = append(rows, []any{mid, phones[i], demoPasswordHash, fmt.Sprintf("压测买家%d", i+1), int16(1)})
	}
	_, err = core.CopyFrom(ctx, pgx.Identifier{"users"},
		[]string{"merchant_id", "phone", "password_hash", "nickname", "status"}, pgx.CopyFromRows(rows))
	must(err)
	userIDs := idsAfter(ctx, core, `SELECT id FROM users WHERE merchant_id = $1 AND phone LIKE '139%' ORDER BY id`, mid)
	nearest := func(lat, lng float64) int64 {
		best, bestD := fx.DefaultStoreID, math.MaxFloat64
		for _, g := range geos {
			if math.Abs(lat-g.lat) <= g.hLat && math.Abs(lng-g.lng) <= g.hLng {
				d := (lat-g.lat)*(lat-g.lat) + (lng-g.lng)*(lng-g.lng)
				if d < bestD {
					best, bestD = g.id, d
				}
			}
		}
		return best
	}
	rows = rows[:0]
	type addr struct {
		lat, lng float64
	}
	addrs := make([]addr, len(userIDs))
	districts := [][2]string{{"西湖区", "330106"}, {"上城区", "330102"}, {"拱墅区", "330105"}, {"滨江区", "330108"}, {"余杭区", "330110"}}
	for i, uid := range userIDs {
		// 95% 在城区矩形里，5% 在外面（只能走默认店）。
		lat, lng := latLo+rng.Float64()*(latHi-latLo), lngLo+rng.Float64()*(lngHi-lngLo)
		if rng.IntN(20) == 0 {
			lat, lng = latHi+0.2+rng.Float64(), lngHi+0.2+rng.Float64()
		}
		addrs[i] = addr{lat, lng}
		d := districts[rng.IntN(len(districts))]
		rows = append(rows, []any{mid, uid, fmt.Sprintf("收件人%d", i+1), phones[i], "浙江省", "杭州市", d[0], "压测街",
			fmt.Sprintf("%d 号楼 %d 室", 1+rng.IntN(30), 101+rng.IntN(1500)), d[1], true, lat, lng})
		if rng.IntN(3) == 0 { // 三分之一的人有第二个地址
			rows = append(rows, []any{mid, uid, fmt.Sprintf("收件人%d-2", i+1), phones[i], "浙江省", "杭州市", d[0], "压测街",
				"公司前台", d[1], false, latLo + rng.Float64()*(latHi-latLo), lngLo + rng.Float64()*(lngHi-lngLo)})
		}
	}
	_, err = core.CopyFrom(ctx, pgx.Identifier{"user_addresses"},
		[]string{"merchant_id", "user_id", "receiver_name", "phone", "province", "city", "district", "street", "detail",
			"region_code", "is_default", "lat", "lng"}, pgx.CopyFromRows(rows))
	must(err)
	defAddr := map[int64]int64{}
	r, err = core.Query(ctx, `SELECT user_id, id FROM user_addresses WHERE merchant_id = $1 AND is_default AND user_id = ANY($2)`, mid, userIDs)
	must(err)
	for r.Next() {
		var u, a int64
		must(r.Scan(&u, &a))
		defAddr[u] = a
	}
	must(r.Err())
	for i, uid := range userIDs {
		fx.Users = append(fx.Users, userFix{ID: uid, Phone: phones[i], AddressID: defAddr[uid],
			StoreID: nearest(addrs[i].lat, addrs[i].lng), Lat: addrs[i].lat, Lng: addrs[i].lng})
	}
	logf(start, "买家 %d 个（地址 %d 条）", len(userIDs), len(rows))

	// ---- 历史订单 ----
	storeSnap := map[int64]string{}
	storeRegion := map[int64]int64{fx.DefaultStoreID: defaultRegion}
	r, err = core.Query(ctx, `SELECT st.id, st.region_id, jsonb_build_object('phone', st.phone, 'address', st.address,
			'store_name', st.name, 'region_name', rg.name)::text
		FROM stores st JOIN regions rg ON rg.id = st.region_id WHERE st.merchant_id = $1`, mid)
	must(err)
	for r.Next() {
		var id, rg int64
		var s string
		must(r.Scan(&id, &rg, &s))
		storeSnap[id], storeRegion[id] = s, rg
	}
	must(r.Err())
	type ord struct {
		items []sku
		qty   []int
	}
	ords := make([]ord, *nOrders)
	rows = rows[:0]
	statuses := []int16{40, 40, 40, 40, 40, 40, 90, 90, 30, 20}
	for i := range ords {
		ui := rng.IntN(len(userIDs))
		store := fx.Users[ui].StoreID
		k := 1 + rng.IntN(3)
		var goods int64
		for j := 0; j < k; j++ {
			s := skus[rng.IntN(len(skus))]
			q := 1 + rng.IntN(2)
			ords[i].items = append(ords[i].items, s)
			ords[i].qty = append(ords[i].qty, q)
			goods += s.price * int64(q)
		}
		freight := int64(800)
		if goods >= 9900 {
			freight = 0
		}
		created := now.Add(-time.Duration(rng.Int64N(int64(90 * 24 * time.Hour))))
		st := statuses[rng.IntN(len(statuses))]
		var paidAt, shippedAt, finishedAt any
		paid := goods + freight
		switch st {
		case 90:
			paid = 0
		case 20:
			paidAt = created.Add(time.Minute)
		case 30:
			// 已发货的放在最近 5 天内：更早的会被自动确认收货任务在压测期间成批处理，混进测量里。
			created = now.Add(-time.Duration(rng.Int64N(int64(5 * 24 * time.Hour))))
			paidAt, shippedAt = created.Add(time.Minute), created.Add(20*time.Hour)
		case 40:
			paidAt, shippedAt, finishedAt = created.Add(time.Minute), created.Add(20*time.Hour), created.Add(96*time.Hour)
		}
		recv, _ := json.Marshal(map[string]string{"city": "杭州市", "phone": phones[ui], "detail": "1 号楼 101", "street": "压测街",
			"district": "西湖区", "province": "浙江省", "region_code": "330106", "receiver_name": fmt.Sprintf("收件人%d", ui+1)})
		orderNo := fmt.Sprintf("%s%018x", created.UTC().Format("20060102150405"), rng.Uint64()>>8|uint64(i)<<56)[:32]
		if i%100 == 0 {
			fx.OrderNos = append(fx.OrderNos, orderNo)
			fx.Phones = append(fx.Phones, phones[ui])
		}
		rows = append(rows, []any{mid, orderNo, userIDs[ui], st, goods, freight, int64(0), goods + freight, paid,
			string(recv), created.Add(30 * time.Minute), paidAt, shippedAt, finishedAt, created, created,
			store, storeRegion[store], storeSnap[store], created})
	}
	_, err = core.CopyFrom(ctx, pgx.Identifier{"orders"},
		[]string{"merchant_id", "order_no", "user_id", "status", "goods_amount_cents", "freight_cents", "discount_cents",
			"payable_cents", "paid_cents", "receiver_snapshot", "expire_at", "paid_at", "shipped_at", "finished_at",
			"created_at", "updated_at", "store_id", "region_id", "store_snapshot", "placed_at"}, pgx.CopyFromRows(rows))
	must(err)
	orderIDs := idsAfter(ctx, core, `SELECT id FROM orders WHERE merchant_id = $1 AND order_no <> ALL($2) ORDER BY id`, mid,
		[]string{"-"})
	orderIDs = orderIDs[len(orderIDs)-len(ords):]
	rows = rows[:0]
	for i, o := range ords {
		for j, s := range o.items {
			amt := s.price * int64(o.qty[j])
			rows = append(rows, []any{mid, orderIDs[i], s.id, s.product, s.title, s.spec, s.price, o.qty[j], amt, s.price})
		}
	}
	n, err = core.CopyFrom(ctx, pgx.Identifier{"order_items"},
		[]string{"merchant_id", "order_id", "sku_id", "product_id", "title_snapshot", "spec_snapshot", "price_cents",
			"quantity", "amount_cents", "list_price_cents"}, pgx.CopyFromRows(rows))
	must(err)
	logf(start, "历史订单 %d 单（明细 %d 行）", len(ords), n)

	for _, q := range []string{"ANALYZE"} {
		_, err = core.Exec(ctx, q)
		must(err)
		_, err = inv.Exec(ctx, q)
		must(err)
	}
	b, _ := json.MarshalIndent(fx, "", " ")
	must(os.WriteFile(*out, b, 0o644))
	logf(start, "ANALYZE 完成；fixture 写到 %s", *out)
}

func idsAfter(ctx context.Context, c *pgx.Conn, q string, args ...any) []int64 {
	r, err := c.Query(ctx, q, args...)
	must(err)
	defer r.Close()
	var ids []int64
	for r.Next() {
		var id int64
		must(r.Scan(&id))
		ids = append(ids, id)
	}
	must(r.Err())
	return ids
}

func logf(start time.Time, f string, a ...any) {
	log.Printf("[%5.1fs] %s", time.Since(start).Seconds(), fmt.Sprintf(f, a...))
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
