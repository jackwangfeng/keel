package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 优惠券的端到端测试（数据模型 §7）。每条都在一家**新开的店**里跑：
//
//	华北大区 ── 华北门店（有货）
//	华南大区 ── 华南门店（有货）
//	默认大区 ── 默认门店
//	分类「服装」/「服装 / 连衣裙」，连衣裙里一件 60 元的商品，服装里一件 50 元的商品
//
// 券模板、范围、发放全部走后台接口，买家侧全部走买家接口：测的是装配好的那一整条链路，
// 而不是某一层的替身。

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

type couponShop struct {
	adminShop
	NorthRegion, SouthRegion int64
	NorthStore, SouthStore   int64
	ParentCat, ChildCat      int64
	DressProduct, DressSKU   int64 // 60 元，在「服装 / 连衣裙」
	ShirtProduct, ShirtSKU   int64 // 50 元，在「服装」
}

type couponBuyer struct {
	UserID  int64
	Phone   string
	Token   string
	Address int64
}

func newCouponShop(t *testing.T) couponShop {
	t.Helper()
	sh := newAdminShop(t)
	cs := couponShop{adminShop: sh}

	// 清理要排在 newAdminShop 的清理**之前**（t.Cleanup 后进先出）：那边会删 merchants，
	// 而这几张表引用着它。
	t.Cleanup(func() {
		for _, stmt := range []string{
			// orders.user_coupon_id 与 user_coupons.order_id 互相引用；先把订单上的券摘掉
			// （连同优惠，否则 chk_discount_needs_coupon 不让摘），环才解得开。
			`UPDATE orders SET user_coupon_id = NULL, discount_cents = 0,
			        payable_cents = goods_amount_cents + freight_cents WHERE merchant_id = $1`,
			`DELETE FROM user_coupons WHERE merchant_id = $1`,
			`DELETE FROM coupon_scopes WHERE merchant_id = $1`,
			`DELETE FROM coupon_templates WHERE merchant_id = $1`,
			`DELETE FROM payments WHERE merchant_id = $1`,
			`DELETE FROM inventory_logs WHERE merchant_id = $1`,
			`DELETE FROM order_items WHERE merchant_id = $1`,
			`DELETE FROM orders WHERE merchant_id = $1`,
			`DELETE FROM user_addresses WHERE merchant_id = $1`,
			`DELETE FROM user_tokens WHERE merchant_id = $1`,
			`DELETE FROM users WHERE merchant_id = $1`,
			`DELETE FROM shop_settings WHERE merchant_id = $1`,
		} {
			if _, err := admin(t).Exec(context.Background(), stmt, sh.MerchantID); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	// 支付回调要验签，密钥从 shop_settings.extra 读（与种子里 shop-a 的形状一样）。
	adminExec(t, `INSERT INTO shop_settings (merchant_id, extra)
	              VALUES ($1, jsonb_build_object('payment_channels',
	                 jsonb_build_object('wechat', jsonb_build_object('notify_secret', $2::text))))`,
		sh.MerchantID, couponWebhookSecret(sh))

	cs.NorthRegion = createRegion(t, sh, "north", "华北")
	cs.SouthRegion = createRegion(t, sh, "south", "华南")
	cs.NorthStore = createStore(t, sh, cs.NorthRegion, "bj", "北京门店", 116.40, 39.90)
	cs.SouthStore = createStore(t, sh, cs.SouthRegion, "gz", "广州门店", 113.26, 23.13)

	cs.ParentCat = createCategory(t, sh, "服装")
	var child api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":"连衣裙 %s","parent_id":%d}`, sh.Suffix, cs.ParentCat), sh.Token),
		http.StatusCreated, "建子类目", &child)
	cs.ChildCat = child.Id

	cs.DressProduct, cs.DressSKU = createPublishedSKU(t, sh, cs.ChildCat, "连衣裙", 6000)
	cs.ShirtProduct, cs.ShirtSKU = createPublishedSKU(t, sh, cs.ParentCat, "衬衫", 5000)
	for _, store := range []int64{cs.NorthStore, cs.SouthStore} {
		for _, sku := range []int64{cs.DressSKU, cs.ShirtSKU} {
			setStoreStock(t, sh, store, sku, 50)
		}
	}
	return cs
}

func couponWebhookSecret(sh adminShop) string { return "coupon-secret-" + sh.Suffix }

func setStoreStock(t *testing.T, sh adminShop, storeID, skuID int64, qty int) {
	t.Helper()
	w := putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", storeID, skuID),
		fmt.Sprintf(`{"expected_available_qty":%d,"available_qty":%d}`, availableAt(t, storeID, skuID), qty), sh.Token)
	wantStatus(t, w, http.StatusOK, "设门店库存")
}

func availableAt(t *testing.T, storeID, skuID int64) int {
	t.Helper()
	var n int
	err := admin(t).QueryRow(context.Background(),
		`SELECT COALESCE((SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2), 0)`,
		storeID, skuID).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// newBuyer 在这家店建一个买家（带一条收货地址），直接签一张访问令牌。
func (cs couponShop) newBuyer(t *testing.T, tag string) couponBuyer {
	t.Helper()
	phone := fmt.Sprintf("139%08d", time.Now().UnixNano()%100_000_000)
	uid := adminQueryInt64(t,
		`INSERT INTO users (merchant_id, phone, nickname) VALUES ($1, $2, $3) RETURNING id`,
		cs.MerchantID, phone, "买家 "+tag)
	addr := adminQueryInt64(t,
		`INSERT INTO user_addresses (merchant_id, user_id, receiver_name, phone, province, city,
		                             district, street, detail)
		 VALUES ($1, $2, '收件人', $3, '北京', '北京', '朝阳', '某街道', '1 号') RETURNING id`,
		cs.MerchantID, uid, phone)
	tok, err := testSigner.Issue(cs.MerchantID, uid, 0, auth.KindAccess, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return couponBuyer{UserID: uid, Phone: phone, Token: tok, Address: addr}
}

// createTemplate 走后台接口建一张券模板，返回它。
func (cs couponShop) createTemplate(t *testing.T, body string) api.AdminCouponTemplate {
	t.Helper()
	var out api.AdminCouponTemplate
	decodeInto(t, postIdem(t, cs.Host, "/api/v1/admin/coupon-templates", body, cs.Token),
		http.StatusCreated, "建券模板", &out)
	return out
}

func (cs couponShop) setScopes(t *testing.T, tplID int64, scopes string) *httptest.ResponseRecorder {
	t.Helper()
	return putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/coupon-templates/%d/scopes", tplID),
		`{"scopes":`+scopes+`}`, cs.Token)
}

func (cs couponShop) patchTemplate(t *testing.T, tplID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	return reqAs(t, http.MethodPatch, cs.Host, fmt.Sprintf("/api/v1/admin/coupon-templates/%d", tplID),
		body, cs.Token)
}

func (cs couponShop) claim(t *testing.T, b couponBuyer, tplID int64) *httptest.ResponseRecorder {
	t.Helper()
	return postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/coupon-templates/%d/claim", tplID), "", b.Token,
		freshIdemKey())
}

func (cs couponShop) mustClaim(t *testing.T, b couponBuyer, tplID int64) api.UserCoupon {
	t.Helper()
	var out api.UserCoupon
	decodeInto(t, cs.claim(t, b, tplID), http.StatusCreated, "领券", &out)
	return out
}

func (cs couponShop) orderJSON(b couponBuyer, storeID, skuID int64, qty int, couponID *int64) string {
	s := fmt.Sprintf(`{"items":[{"sku_id":%d,"quantity":%d}],"address_id":%d,"store_id":%d`,
		skuID, qty, b.Address, storeID)
	if couponID != nil {
		s += fmt.Sprintf(`,"user_coupon_id":%d`, *couponID)
	}
	return s + "}"
}

func (cs couponShop) preview(t *testing.T, b couponBuyer, body string) (api.OrderPreview, *httptest.ResponseRecorder) {
	t.Helper()
	w := previewOrder(t, cs.Host, body, b.Token)
	var out api.OrderPreview
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out, w
}

func (cs couponShop) applicable(t *testing.T, b couponBuyer, storeID, skuID int64, qty int) []api.ApplicableCoupon {
	t.Helper()
	var out []api.ApplicableCoupon
	decodeInto(t, post(t, cs.Host, "/api/v1/coupons/applicable",
		fmt.Sprintf(`{"items":[{"sku_id":%d,"quantity":%d}],"store_id":%d}`, skuID, qty, storeID), b.Token),
		http.StatusOK, "本单可用券", &out)
	return out
}

func (cs couponShop) pay(t *testing.T, orderNo string, amount int64) {
	t.Helper()
	body := payload(orderNo, "txn-"+orderNo, amount)
	w := notifyPaymentSigned(t, cs.Host, "wechat", body, sign(couponWebhookSecret(cs.adminShop), []byte(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("支付回调失败：%d %s", w.Code, w.Body.String())
	}
}

// couponState 直接读库：这张券现在的状态与占着它的订单号。
func couponState(t *testing.T, couponID int64) (status int16, orderNo *string) {
	t.Helper()
	err := admin(t).QueryRow(context.Background(), `
		SELECT uc.status, o.order_no FROM user_coupons uc LEFT JOIN orders o ON o.id = uc.order_id
		 WHERE uc.id = $1`, couponID).Scan(&status, &orderNo)
	if err != nil {
		t.Fatalf("读券 %d 失败: %v", couponID, err)
	}
	return status, orderNo
}

func ptr64(v int64) *int64 { return &v }

// withTenantConn 以 keel_app 身份、在给定租户上下文里跑一条查询 —— 直接验 RLS，
// 不经过任何一层应用代码。
func withTenantConn(t *testing.T, merchantID int64, sql string, args ...any) pgx.Row {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.merchant_id = '%d'", merchantID)); err != nil {
		t.Fatal(err)
	}
	return tx.QueryRow(ctx, sql, args...)
}

// full100minus20 是验收用例里那张券：满 100 减 20，绝对时间、今天可用。
func full100minus20(total, perUser int) string {
	return fmt.Sprintf(`{"name":"满100减20","coupon_type":1,"threshold_cents":10000,"discount_cents":2000,
		"valid_mode":1,"valid_start_at":%q,"valid_end_at":%q,"total_count":%d,"per_user_limit":%d}`,
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		time.Now().Add(72*time.Hour).UTC().Format(time.RFC3339), total, perUser)
}

// ---------------------------------------------------------------------------
// 验收主路径：限华北的满 100 减 20 → 领 → 华北下单试算减 20 → 下单锁券 → 支付核销；
// 华南不可用。
// ---------------------------------------------------------------------------

func TestCouponNorthRegionEndToEnd(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, full100minus20(0, 2))
	if tpl.Claimable {
		t.Fatal("新建的模板默认不该可领（先配范围，再打开领券中心）")
	}
	wantStatus(t, cs.setScopes(t, tpl.Id, fmt.Sprintf(`[{"scope_type":5,"target_id":%d}]`, cs.NorthRegion)),
		http.StatusOK, "限华北")
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")

	b := cs.newBuyer(t, "north")

	// 领券中心里看得到它，带着范围。
	var center struct {
		Items []api.ClaimableCouponTemplate `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/coupon-templates", b.Token), http.StatusOK, "领券中心", &center)
	if len(center.Items) != 1 || center.Items[0].Id != tpl.Id || !center.Items[0].CanClaim ||
		len(center.Items[0].Scopes) != 1 || center.Items[0].Scopes[0].TargetName == nil ||
		*center.Items[0].Scopes[0].TargetName != "华北" {
		t.Fatalf("领券中心的内容不对：%+v", center.Items)
	}

	coupon := cs.mustClaim(t, b, tpl.Id)
	if coupon.Status != 1 || coupon.Source != 1 {
		t.Fatalf("刚领的券状态 %d 来源 %d，期望 1 / 1", coupon.Status, coupon.Source)
	}

	// 华北门店：两件连衣裙 120 元，满 100 减 20。本单可用券与试算说的是同一个数。
	apps := cs.applicable(t, b, cs.NorthStore, cs.DressSKU, 2)
	if len(apps) != 1 || apps[0].Id != coupon.Id || apps[0].ApplicableDiscountCents != 2000 {
		t.Fatalf("华北门店的本单可用券不对：%+v", apps)
	}
	body := cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 2, &coupon.Id)
	pv, w := cs.preview(t, b, body)
	if w.Code != http.StatusOK {
		t.Fatalf("华北试算失败：%d %s", w.Code, w.Body.String())
	}
	if *pv.DiscountCents != 2000 || pv.PayableCents != 10000 || pv.UserCouponId == nil ||
		*pv.UserCouponId != coupon.Id {
		t.Fatalf("华北试算：优惠 %d 应付 %d 券 %v，期望 2000 / 10000 / %d",
			*pv.DiscountCents, pv.PayableCents, pv.UserCouponId, coupon.Id)
	}
	if pv.StoreId != cs.NorthStore || pv.RegionId == nil || *pv.RegionId != cs.NorthRegion {
		t.Fatalf("试算回显的门店 / 大区是 %d / %v，期望 %d / %d", pv.StoreId, pv.RegionId, cs.NorthStore, cs.NorthRegion)
	}
	if pv.ApplicableCoupons == nil || len(*pv.ApplicableCoupons) != 1 {
		t.Fatalf("试算里的 applicable_coupons 应有 1 张：%+v", pv.ApplicableCoupons)
	}

	// 下单：金额与试算逐分相等，券进入锁定。
	cw := createOrder(t, cs.Host, body, b.Token, "cpn-"+uniqueKey())
	if cw.Code != http.StatusCreated {
		t.Fatalf("下单失败：%d %s", cw.Code, cw.Body.String())
	}
	var order api.Order
	if err := json.Unmarshal(cw.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if order.PayableCents != pv.PayableCents || *order.DiscountCents != *pv.DiscountCents {
		t.Fatalf("下单应付 %d 优惠 %d，与试算 %d / %d 不一致",
			order.PayableCents, *order.DiscountCents, pv.PayableCents, *pv.DiscountCents)
	}
	if order.UserCouponId == nil || *order.UserCouponId != coupon.Id {
		t.Fatalf("订单上的 user_coupon_id 是 %v，期望 %d", order.UserCouponId, coupon.Id)
	}
	if st, no := couponState(t, coupon.Id); st != 2 || no == nil || *no != order.OrderNo {
		t.Fatalf("下单之后券应锁定在 %s 上，实际状态 %d 订单 %v", order.OrderNo, st, no)
	}
	// 订单行的分摊之和 = 订单优惠。
	var sum int64
	if err := admin(t).QueryRow(context.Background(), `
		SELECT COALESCE(sum(oi.discount_cents), 0) FROM order_items oi JOIN orders o ON o.id = oi.order_id
		 WHERE o.order_no = $1`, order.OrderNo).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum != 2000 {
		t.Fatalf("order_items.discount_cents 之和是 %d，期望 2000", sum)
	}
	// 锁定期间「我的券」里按 locked 查得到它。
	var mine struct {
		Items []api.UserCoupon `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/coupons?status=locked", b.Token), http.StatusOK, "我的锁定券", &mine)
	if len(mine.Items) != 1 || mine.Items[0].Id != coupon.Id || mine.Items[0].Status != 2 {
		t.Fatalf("status=locked 应查到这张券：%+v", mine.Items)
	}

	// 支付：券变成已使用。
	cs.pay(t, order.OrderNo, order.PayableCents)
	if st, _ := couponState(t, coupon.Id); st != 3 {
		t.Fatalf("支付之后券状态是 %d，期望 3 已使用", st)
	}
	if got := orderStatusOf(t, order.OrderNo); got != 20 {
		t.Fatalf("支付之后订单状态是 %d，期望 20", got)
	}

	// 华南门店：领第二张（每人限 2），在华南不可用，本单可用券里也没有它。
	second := cs.mustClaim(t, b, tpl.Id)
	if apps := cs.applicable(t, b, cs.SouthStore, cs.DressSKU, 2); len(apps) != 0 {
		t.Fatalf("华南门店不该有可用券：%+v", apps)
	}
	_, w = cs.preview(t, b, cs.orderJSON(b, cs.SouthStore, cs.DressSKU, 2, &second.Id))
	p := problemOf(t, w, http.StatusConflict)
	if p.Type != problem.TypeCouponNotApplicable || p.Detail == nil || !strings.Contains(*p.Detail, "门店") {
		t.Fatalf("华南试算应 409 coupon-not-applicable（门店不在范围），实得 %+v", p)
	}
	cw = createOrder(t, cs.Host, cs.orderJSON(b, cs.SouthStore, cs.DressSKU, 2, &second.Id), b.Token, "cpn-"+uniqueKey())
	if p := problemOf(t, cw, http.StatusConflict); p.Type != problem.TypeCouponNotApplicable {
		t.Fatalf("华南下单应 409 coupon-not-applicable，实得 %+v", p)
	}
	if st, _ := couponState(t, second.Id); st != 1 {
		t.Fatalf("被拒的下单不该动券：状态 %d", st)
	}

	// 后台统计：发出 2、领取 2、已使用 1、未使用 1。
	var view api.AdminCouponTemplate
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/coupon-templates/%d", tpl.Id), cs.Token),
		http.StatusOK, "模板详情", &view)
	if view.Stats.Issued != 2 || view.Stats.Claimed != 2 || view.Stats.Used != 1 || view.Stats.Unused != 1 ||
		view.IssuedCount != 2 || !view.Locked {
		t.Fatalf("统计不对：%+v issued_count=%d locked=%v", view.Stats, view.IssuedCount, view.Locked)
	}
}

// 试算与下单算出同一个数 —— 带券、多行、按门店价（华北门店单独定了价）。
func TestCouponPreviewAndCreateAgree(t *testing.T) {
	cs := newCouponShop(t)
	// 华北门店把连衣裙定成 70.01 元：券要按门店生效价算，不是基准价 60 元。
	wantStatus(t, putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", cs.NorthStore, cs.DressSKU),
		`{"price_cents":7001}`, cs.Token), http.StatusOK, "门店价")
	tpl := cs.createTemplate(t, `{"name":"85 折封顶 30","coupon_type":2,"discount_rate":850,
		"max_discount_cents":3000,"valid_mode":2,"valid_days":7}`)
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	b := cs.newBuyer(t, "agree")
	c := cs.mustClaim(t, b, tpl.Id)

	body := fmt.Sprintf(`{"items":[{"sku_id":%d,"quantity":1},{"sku_id":%d,"quantity":1}],
		"address_id":%d,"store_id":%d,"user_coupon_id":%d}`, cs.DressSKU, cs.ShirtSKU, b.Address, cs.NorthStore, c.Id)
	pv, w := cs.preview(t, b, body)
	if w.Code != http.StatusOK {
		t.Fatalf("试算失败：%d %s", w.Code, w.Body.String())
	}
	// 7001 + 5000 = 12001；× 15% = 1800.15 → 向下取整 1800（没到 3000 封顶）。
	if pv.GoodsAmountCents != 12001 || *pv.DiscountCents != 1800 || pv.PayableCents != 10201 {
		t.Fatalf("试算 商品 %d 优惠 %d 应付 %d，期望 12001 / 1800 / 10201",
			pv.GoodsAmountCents, *pv.DiscountCents, pv.PayableCents)
	}
	withExpect := strings.TrimSuffix(body, "}") + fmt.Sprintf(`,"expected_payable_cents":%d}`, pv.PayableCents)
	cw := createOrder(t, cs.Host, withExpect, b.Token, "agree-"+uniqueKey())
	if cw.Code != http.StatusCreated {
		t.Fatalf("带着试算的应付下单失败：%d %s", cw.Code, cw.Body.String())
	}
	var order api.Order
	if err := json.Unmarshal(cw.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if order.PayableCents != pv.PayableCents || *order.DiscountCents != *pv.DiscountCents {
		t.Fatalf("下单 %d/%d 与试算 %d/%d 不一致", order.PayableCents, *order.DiscountCents,
			pv.PayableCents, *pv.DiscountCents)
	}
	// 每行分摊也要与试算一致。
	rows, err := admin(t).Query(context.Background(), `
		SELECT oi.sku_id, oi.discount_cents FROM order_items oi JOIN orders o ON o.id = oi.order_id
		 WHERE o.order_no = $1`, order.OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]int64{}
	for rows.Next() {
		var sku, d int64
		if err := rows.Scan(&sku, &d); err != nil {
			t.Fatal(err)
		}
		got[sku] = d
	}
	for _, it := range pv.Items {
		if got[*it.SkuId] != int64(*it.DiscountCents) {
			t.Fatalf("sku %d 的分摊：订单 %d，试算 %d", *it.SkuId, got[*it.SkuId], *it.DiscountCents)
		}
	}
}

// 分类含子孙、排除优先 —— 端到端（分类 path 从 ListSKUsForPricing 一路带到计算）。
func TestCouponCategoryScopeEndToEnd(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, `{"name":"服装立减 10","coupon_type":3,"discount_cents":1000,
		"valid_mode":2,"valid_days":3,"per_user_limit":5}`)
	// 限「服装」（含子类「连衣裙」），但衬衫这件商品排除。
	wantStatus(t, cs.setScopes(t, tpl.Id, fmt.Sprintf(`[{"scope_type":2,"target_id":%d},
		{"scope_type":3,"target_id":%d,"include":false}]`, cs.ParentCat, cs.ShirtProduct)), http.StatusOK, "配范围")
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	b := cs.newBuyer(t, "cat")
	c := cs.mustClaim(t, b, tpl.Id)

	// 连衣裙在子类里：可用。
	if apps := cs.applicable(t, b, cs.NorthStore, cs.DressSKU, 1); len(apps) != 1 || apps[0].ApplicableDiscountCents != 1000 {
		t.Fatalf("子类商品应可用立减 1000：%+v", apps)
	}
	// 衬衫被排除：不可用。
	_, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 1, &c.Id))
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeCouponNotApplicable {
		t.Fatalf("被排除的商品应 409，实得 %+v", p)
	}
}

// ---------------------------------------------------------------------------
// SAGA：扣库存失败 → 券解锁；超时关单 → 券退回；锁券是最终判定点。
// ---------------------------------------------------------------------------

func TestCouponUnlockedWhenStockDeductionFails(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, full100minus20(0, 1))
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	b := cs.newBuyer(t, "stock")
	c := cs.mustClaim(t, b, tpl.Id)

	setStoreStock(t, cs.adminShop, cs.NorthStore, cs.DressSKU, 1) // 只剩 1 件，下 2 件
	w := createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 2, &c.Id), b.Token, "stk-"+uniqueKey())
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeInsufficientStock {
		t.Fatalf("库存不足应 409 insufficient-stock，实得 %+v", p)
	}
	if st, no := couponState(t, c.Id); st != 1 || no != nil {
		t.Fatalf("库存分支失败后券应补偿回未使用，实际状态 %d 订单 %v", st, no)
	}
	// 券确实被锁过（补偿是真补偿，不是空回滚）：那一单挂着这张券，且被关到 90。
	var status int16
	if err := admin(t).QueryRow(context.Background(),
		`SELECT status FROM orders WHERE user_coupon_id = $1`, c.Id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != 90 {
		t.Fatalf("失败那一单的状态是 %d，期望 90 已关闭", status)
	}
	// 解锁之后这张券还能用在下一单上。
	setStoreStock(t, cs.adminShop, cs.NorthStore, cs.DressSKU, 10)
	w = createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 2, &c.Id), b.Token, "stk-"+uniqueKey())
	wantStatus(t, w, http.StatusCreated, "补货后用同一张券重新下单")
}

func TestCouponReleasedWhenOrderTimesOut(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, full100minus20(0, 1))
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	b := cs.newBuyer(t, "timeout")
	c := cs.mustClaim(t, b, tpl.Id)

	w := createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 2, &c.Id), b.Token, "to-"+uniqueKey())
	var order api.Order
	decodeInto(t, w, http.StatusCreated, "下单", &order)
	if st, _ := couponState(t, c.Id); st != 2 {
		t.Fatalf("下单后券应锁定，实际 %d", st)
	}

	adminExec(t, `UPDATE orders SET expire_at = now() - interval '1 minute' WHERE order_no = $1`, order.OrderNo)
	if _, err := newSweeper(service.SweepConfig{}).SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := orderStatusOf(t, order.OrderNo); got != 90 {
		t.Fatalf("超时之后订单状态 %d，期望 90", got)
	}
	if st, no := couponState(t, c.Id); st != 1 || no != nil {
		t.Fatalf("超时关单后券应退回未使用，实际状态 %d 订单 %v", st, no)
	}
}

// 两单用同一张券：试算都算得过，锁券那一步只有一单锁得上。这里直接调券分支，
// 模拟「第二单在第一单锁券之后才跑到券分支」那个窗口。
func TestCouponLockIsTheArbiter(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, full100minus20(0, 1))
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	b := cs.newBuyer(t, "arbiter")
	c := cs.mustClaim(t, b, tpl.Id)

	var first api.Order
	decodeInto(t, createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 2, &c.Id), b.Token,
		"arb-"+uniqueKey()), http.StatusCreated, "第一单", &first)

	// 第二单：不带券下单，再在库里把它改成「挂着这张券、还没锁」的样子。
	// 它自己那一次 SAGA 已经用掉了分支号 02（屏障会把重放判成重复），
	// 所以下面用一个没用过的分支号 12 直接调券分支。
	var second api.Order
	decodeInto(t, createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 1, nil), b.Token,
		"arb-"+uniqueKey()), http.StatusCreated, "第二单", &second)
	adminExec(t, `UPDATE orders SET user_coupon_id = $1 WHERE order_no = $2`, c.Id, second.OrderNo)

	gid := gidFor(t, cs.MerchantID, second.OrderNo)
	if got := branchOf(t, service.BranchOrderCoupon)(gid, "12", "action"); got != dtm.Failure {
		t.Fatalf("券已被第一单锁住，第二单的券分支应返回 Failure，实得 %d", got)
	}
	if st, no := couponState(t, c.Id); st != 2 || no == nil || *no != first.OrderNo {
		t.Fatalf("券应仍锁在第一单 %s 上，实际状态 %d 订单 %v", first.OrderNo, st, no)
	}
	// 第二单的补偿不能把第一单的锁解开（按 order_id 解，不按券 id）。
	if got := branchOf(t, service.BranchOrderCouponUndo)(gid, "12", "compensate"); got != dtm.Success {
		t.Fatalf("补偿应返回 Success，实得 %d", got)
	}
	if st, no := couponState(t, c.Id); st != 2 || no == nil || *no != first.OrderNo {
		t.Fatalf("第二单的补偿把第一单的锁解开了：状态 %d 订单 %v", st, no)
	}
}

// ---------------------------------------------------------------------------
// 领券并发：不超发、不超过每人限领。
// ---------------------------------------------------------------------------

func TestClaimDoesNotOverIssueUnderConcurrency(t *testing.T) {
	cs := newCouponShop(t)
	const total, buyers = 5, 40
	tpl := cs.createTemplate(t, full100minus20(total, 1))
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")

	bs := make([]couponBuyer, buyers)
	for i := range bs {
		bs[i] = cs.newBuyer(t, fmt.Sprintf("rush-%d", i))
		time.Sleep(time.Microsecond) // 手机号取自纳秒时钟，错开一点
	}
	codes := make([]int, buyers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range bs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = cs.claim(t, bs[i], tpl.Id).Code
		}(i)
	}
	close(start)
	wg.Wait()

	ok, soldOut := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			ok++
		case http.StatusConflict:
			soldOut++
		default:
			t.Errorf("意外的状态码 %d", c)
		}
	}
	var issued, rows int
	if err := admin(t).QueryRow(context.Background(),
		`SELECT issued_count, (SELECT count(*) FROM user_coupons WHERE template_id = $1)
		   FROM coupon_templates WHERE id = $1`, tpl.Id).Scan(&issued, &rows); err != nil {
		t.Fatal(err)
	}
	if ok != total || soldOut != buyers-total || issued != total || rows != total {
		t.Fatalf("%d 人抢 %d 张：成功 %d、409 %d、issued_count %d、实例 %d 行 —— 超发或少发了",
			buyers, total, ok, soldOut, issued, rows)
	}
}

func TestClaimRespectsPerUserLimitUnderConcurrency(t *testing.T) {
	cs := newCouponShop(t)
	const limit, attempts = 2, 20
	tpl := cs.createTemplate(t, full100minus20(0, limit))
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	b := cs.newBuyer(t, "greedy")

	codes := make([]int, attempts)
	types := make([]string, attempts)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			w := cs.claim(t, b, tpl.Id) // 每次一把新的幂等键：这是 20 个不同的领取请求
			codes[i] = w.Code
			if w.Code != http.StatusCreated {
				var p api.Problem
				_ = json.Unmarshal(w.Body.Bytes(), &p)
				types[i] = p.Type
			}
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for i, c := range codes {
		if c == http.StatusCreated {
			ok++
			continue
		}
		if c != http.StatusConflict || types[i] != problem.TypeCouponClaimLimitReached {
			t.Errorf("第 %d 次：%d %s，期望 409 coupon-claim-limit-reached", i, c, types[i])
		}
	}
	var held, issued int
	if err := admin(t).QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM user_coupons WHERE template_id = $1 AND user_id = $2),
		       (SELECT issued_count FROM coupon_templates WHERE id = $1)`, tpl.Id, b.UserID).
		Scan(&held, &issued); err != nil {
		t.Fatal(err)
	}
	if ok != limit || held != limit || issued != limit {
		t.Fatalf("每人限领 %d：成功 %d、持有 %d、issued_count %d", limit, ok, held, issued)
	}
}

// 同一把幂等键重放领券：返回首次那一张，不多领。
func TestClaimIsIdempotent(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, full100minus20(0, 5))
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	b := cs.newBuyer(t, "idem")
	key := freshIdemKey()
	path := fmt.Sprintf("/api/v1/coupon-templates/%d/claim", tpl.Id)
	var first, again api.UserCoupon
	decodeInto(t, postWithKey(t, cs.Host, path, "", b.Token, key), http.StatusCreated, "首次领取", &first)
	w := postWithKey(t, cs.Host, path, "", b.Token, key)
	decodeInto(t, w, http.StatusCreated, "重放领取", &again)
	if again.Id != first.Id || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重放应返回首次那张券 %d（带 Idempotency-Replayed），实得 %d", first.Id, again.Id)
	}
	var n int
	_ = admin(t).QueryRow(context.Background(), `SELECT count(*) FROM user_coupons WHERE template_id = $1`, tpl.Id).Scan(&n)
	if n != 1 {
		t.Fatalf("同一把钥匙领了 %d 张", n)
	}
}

// ---------------------------------------------------------------------------
// 跨租户：A 店的券在 B 店看不到、领不到、用不了。
// ---------------------------------------------------------------------------

func TestCouponsAreTenantIsolated(t *testing.T) {
	a := newCouponShop(t)
	bShop := newCouponShop(t)
	tpl := a.createTemplate(t, full100minus20(0, 1))
	wantStatus(t, a.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	buyerA := a.newBuyer(t, "a")
	cA := a.mustClaim(t, buyerA, tpl.Id)
	buyerB := bShop.newBuyer(t, "b")

	// B 店的后台看不到 A 店的模板。
	if w := getAs(t, bShop.Host, fmt.Sprintf("/api/v1/admin/coupon-templates/%d", tpl.Id), bShop.Token); w.Code != http.StatusNotFound {
		t.Fatalf("B 店后台读 A 店模板应 404，实得 %d", w.Code)
	}
	var list struct {
		Total int `json:"total"`
	}
	decodeInto(t, getAs(t, bShop.Host, "/api/v1/admin/coupon-templates", bShop.Token), http.StatusOK, "B 店模板列表", &list)
	if list.Total != 0 {
		t.Fatalf("B 店模板列表里有 %d 条（A 店的漏过来了）", list.Total)
	}
	// B 店的领券中心看不到、领不到。
	var center struct {
		Total int `json:"total"`
	}
	decodeInto(t, getAs(t, bShop.Host, "/api/v1/coupon-templates", buyerB.Token), http.StatusOK, "B 店领券中心", &center)
	if center.Total != 0 {
		t.Fatalf("B 店领券中心里有 %d 条", center.Total)
	}
	if w := bShop.claim(t, buyerB, tpl.Id); w.Code != http.StatusNotFound {
		t.Fatalf("在 B 店领 A 店的券应 404，实得 %d %s", w.Code, w.Body.String())
	}
	// B 店买家拿 A 店的券 id 下单：409 不可用（与「不存在」同形）。
	_, w := bShop.preview(t, buyerB, bShop.orderJSON(buyerB, bShop.NorthStore, bShop.DressSKU, 2, &cA.Id))
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeCouponNotApplicable {
		t.Fatalf("B 店用 A 店的券应 409，实得 %+v", p)
	}
	// B 店后台不能把 A 店的门店配进范围（target_id 多态列的应用层校验）。
	btpl := bShop.createTemplate(t, full100minus20(0, 1))
	w = bShop.setScopes(t, btpl.Id, fmt.Sprintf(`[{"scope_type":6,"target_id":%d}]`, a.NorthStore))
	if p := problemOf(t, w, http.StatusUnprocessableEntity); !strings.Contains(*p.Detail, "查不到") {
		t.Fatalf("把别家门店配进范围应 422，实得 %+v", p)
	}
	// 行级安全本身：以 B 店的租户上下文直接查 user_coupons，看不到 A 店那一行。
	var n int
	err := withTenantConn(t, bShop.MerchantID, `SELECT count(*) FROM user_coupons WHERE id = $1`, cA.Id).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("B 店租户上下文里看得到 A 店的券（RLS 失效）")
	}
	err = withTenantConn(t, a.MerchantID, `SELECT count(*) FROM user_coupons WHERE id = $1`, cA.Id).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("阳性对照：A 店自己应看得到这张券，实得 %d %v", n, err)
	}
}

// ---------------------------------------------------------------------------
// 过期
// ---------------------------------------------------------------------------

func TestExpiredCouponIsNotUsable(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, `{"name":"立减 5","coupon_type":3,"discount_cents":500,"valid_mode":2,"valid_days":1}`)
	b := cs.newBuyer(t, "expired")
	// 直接落一张已过期的券（领取窗口是现在，没法经接口领到一张过去的券）。
	id := adminQueryInt64(t, `
		INSERT INTO user_coupons (merchant_id, coupon_code, template_id, user_id, source, valid_start_at, valid_end_at)
		VALUES ($1, $2, $3, $4, 2, now() - interval '3 days', now() - interval '1 second') RETURNING id`,
		cs.MerchantID, "CEXP"+cs.Suffix, tpl.Id, b.UserID)

	_, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, &id))
	p := problemOf(t, w, http.StatusConflict)
	if p.Type != problem.TypeCouponNotApplicable || !strings.Contains(*p.Detail, "过期") {
		t.Fatalf("过期券应 409（已过期），实得 %+v", p)
	}
	if apps := cs.applicable(t, b, cs.NorthStore, cs.DressSKU, 1); len(apps) != 0 {
		t.Fatalf("过期券不该出现在本单可用券里：%+v", apps)
	}
	var mine struct {
		Items []api.UserCoupon `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/coupons?status=expired", b.Token), http.StatusOK, "我的过期券", &mine)
	if len(mine.Items) != 1 || mine.Items[0].Status != 4 {
		t.Fatalf("status=expired 应查到它且状态为 4：%+v", mine.Items)
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/coupons?status=available", b.Token), http.StatusOK, "我的可用券", &mine)
	if len(mine.Items) != 0 {
		t.Fatalf("过期券不该在 available 里：%+v", mine.Items)
	}
	// 就算绕过试算直接落一笔挂着它的订单，锁券分支也锁不上（有效期在数据库的 now() 上再判一次）。
	var order api.Order
	decodeInto(t, createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 1, nil), b.Token,
		"exp-"+uniqueKey()), http.StatusCreated, "不带券下单", &order)
	adminExec(t, `UPDATE orders SET user_coupon_id = $1 WHERE order_no = $2`, id, order.OrderNo)
	if got := branchOf(t, service.BranchOrderCoupon)(gidFor(t, cs.MerchantID, order.OrderNo), "12", "action"); got != dtm.Failure {
		t.Fatalf("过期券的锁券分支应 Failure，实得 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 后台：配置校验、权限、已发出后锁定、定向发放。
// ---------------------------------------------------------------------------

func TestAdminCouponTemplateValidation(t *testing.T) {
	cs := newCouponShop(t)
	for _, tc := range []struct {
		name, body, detail string
	}{
		{"满100减200", `{"name":"x","coupon_type":1,"threshold_cents":10000,"discount_cents":20000,"valid_mode":2,"valid_days":1}`, "配置错误"},
		{"包邮券", `{"name":"x","coupon_type":4,"valid_mode":2,"valid_days":1}`, "不计运费"},
		{"折扣率越界", `{"name":"x","coupon_type":2,"discount_rate":1000,"valid_mode":2,"valid_days":1}`, "千分比"},
		{"立减带门槛", `{"name":"x","coupon_type":3,"discount_cents":100,"threshold_cents":100,"valid_mode":2,"valid_days":1}`, "没有门槛"},
		{"有效期倒挂", `{"name":"x","coupon_type":3,"discount_cents":100,"valid_mode":1,
			"valid_start_at":"2026-10-02T00:00:00Z","valid_end_at":"2026-10-01T00:00:00Z"}`, "晚于"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := problemOf(t, postIdem(t, cs.Host, "/api/v1/admin/coupon-templates", tc.body, cs.Token),
				http.StatusUnprocessableEntity)
			if p.Detail == nil || !strings.Contains(*p.Detail, tc.detail) {
				t.Fatalf("detail 是 %v，期望包含 %q", p.Detail, tc.detail)
			}
		})
	}
	// 数据库的 CHECK 是最后一道：绕过接口直接插「满 100 减 200」与包邮券，被拒。
	for _, sql := range []string{
		`INSERT INTO coupon_templates (merchant_id, name, coupon_type, threshold_cents, discount_cents, valid_mode, valid_days)
		 VALUES ($1, 'x', 1, 10000, 20000, 2, 1)`,
		`INSERT INTO coupon_templates (merchant_id, name, coupon_type, valid_mode, valid_days)
		 VALUES ($1, 'x', 4, 2, 1)`,
	} {
		if _, err := admin(t).Exec(context.Background(), sql, cs.MerchantID); err == nil ||
			!strings.Contains(err.Error(), "chk_coupon_rule") {
			t.Fatalf("chk_coupon_rule 应拒绝这一行，实得 %v", err)
		}
	}
}

func TestAdminCouponRequiresMerchantAdminOrOperator(t *testing.T) {
	cs := newCouponShop(t)
	// 操作员（role 2）可以。
	op := mkStaff(t, cs.Suffix, "op-"+cs.Suffix+"@keel.test", 2, 1)
	opSess := staffSession(t, cs.Host, op)
	wantStatus(t, getAs(t, cs.Host, "/api/v1/admin/coupon-templates", opSess.Token), http.StatusOK, "操作员读券")
	// 其他角色（分级权限那条线会加的 3 / 4）一律 403。
	other := mkStaff(t, cs.Suffix, "region-"+cs.Suffix+"@keel.test", 3, 1)
	otherSess := staffSession(t, cs.Host, other)
	if p := problemOf(t, getAs(t, cs.Host, "/api/v1/admin/coupon-templates", otherSess.Token), http.StatusForbidden); p.Type != problem.TypeStaffForbidden {
		t.Fatalf("role 3 应 403 staff-forbidden，实得 %+v", p)
	}
	if p := problemOf(t, postIdem(t, cs.Host, "/api/v1/admin/coupon-templates", full100minus20(0, 1), otherSess.Token),
		http.StatusForbidden); p.Type != problem.TypeStaffForbidden {
		t.Fatalf("role 3 建券应 403，实得 %+v", p)
	}
}

func TestAdminCouponTemplateLockedAfterIssuance(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, full100minus20(10, 1))
	// 发出之前：券面随便改。
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"discount_cents":3000}`), http.StatusOK, "发出前改券面")
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	cs.mustClaim(t, cs.newBuyer(t, "lock"), tpl.Id)

	if p := problemOf(t, cs.patchTemplate(t, tpl.Id, `{"discount_cents":5000}`), http.StatusConflict); p.Type != problem.TypeCouponTemplateLocked {
		t.Fatalf("发出后改券面应 409 locked，实得 %+v", p)
	}
	if p := problemOf(t, cs.setScopes(t, tpl.Id, fmt.Sprintf(`[{"scope_type":6,"target_id":%d}]`, cs.NorthStore)),
		http.StatusConflict); p.Type != problem.TypeCouponTemplateLocked {
		t.Fatalf("发出后改范围应 409 locked，实得 %+v", p)
	}
	// 原样提交券面（后台表单常这么做）不算改；名字、总量、启停照常能改。
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"discount_cents":3000,"name":"改个名","total_count":20,"status":0}`),
		http.StatusOK, "发出后改名、改总量、停用")
}

func TestAdminGrantCouponsByPhone(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, full100minus20(3, 1))
	b1, b2 := cs.newBuyer(t, "g1"), cs.newBuyer(t, "g2")
	path := fmt.Sprintf("/api/v1/admin/coupon-templates/%d/grants", tpl.Id)

	// 有一个手机号查不到：整批 422、一张不发、detail 列出它。
	w := postIdem(t, cs.Host, path, fmt.Sprintf(`{"phones":[%q,"13000000000"]}`, b1.Phone), cs.Token)
	if p := problemOf(t, w, http.StatusUnprocessableEntity); !strings.Contains(*p.Detail, "13000000000") {
		t.Fatalf("未知手机号应 422 并列出，实得 %+v", p)
	}
	var n int
	_ = admin(t).QueryRow(context.Background(), `SELECT count(*) FROM user_coupons WHERE template_id = $1`, tpl.Id).Scan(&n)
	if n != 0 {
		t.Fatalf("整批失败却发出了 %d 张", n)
	}

	// 定向发放不受每人限领（1）约束：b1 发两张。模板没设为可领也照样能发。
	var res api.CouponGrantResult
	decodeInto(t, postIdem(t, cs.Host, path, fmt.Sprintf(`{"phones":[%q,%q,%q]}`, b1.Phone, b1.Phone, b2.Phone), cs.Token),
		http.StatusCreated, "定向发放", &res)
	if res.Granted != 3 || len(res.Coupons) != 3 {
		t.Fatalf("应发出 3 张：%+v", res)
	}
	var mine struct {
		Items []api.UserCoupon `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/coupons", b1.Token), http.StatusOK, "b1 的券", &mine)
	if len(mine.Items) != 2 || mine.Items[0].Source != 2 {
		t.Fatalf("b1 应有 2 张定向发放的券：%+v", mine.Items)
	}
	// 总量 3 已发完：再发 409 sold-out。
	if p := problemOf(t, postIdem(t, cs.Host, path, fmt.Sprintf(`{"phones":[%q]}`, b2.Phone), cs.Token),
		http.StatusConflict); p.Type != problem.TypeCouponSoldOut {
		t.Fatalf("总量用完应 409 sold-out，实得 %+v", p)
	}
	// 定向发放的券也照常能下单。
	w = previewOrder(t, cs.Host, cs.orderJSON(b2, cs.NorthStore, cs.DressSKU, 2, ptr64(res.Coupons[2].UserCouponId)), b2.Token)
	wantStatus(t, w, http.StatusOK, "定向发放的券试算")
}
