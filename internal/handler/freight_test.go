package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 运费模板的端到端测试（数据模型 §7「运费模板」「运费怎么算」，00041 / 00042）。
//
// 每条都在 newCouponShop 开的新店里跑（北京门店、广州门店，60 元的连衣裙、50 元的衬衫），
// 模板走后台接口建，买家侧走试算 / 下单 / 购物车：测的是装配好的整条链路。
// 计算口径的逐分边界在 service/freight_calc_test.go，这里验的是「接上了」与「钱对得上」。

// nationwideFreight：全国首件 8 元续件 2 元、满 99 包邮；新疆西藏首件 20 元续件 10 元、
// 不包邮；香港不配送。全店默认模板。
const nationwideFreight = `{"name":"默认运费","charge_mode":1,"is_default":true,
	"rules":[
	  {"region_codes":["650000","540000"],"first_unit":1,"first_fee_cents":2000,"additional_unit":1,
	   "additional_fee_cents":1000,"free_threshold_cents":0,"free_quantity":0},
	  {"region_codes":[],"first_unit":1,"first_fee_cents":800,"additional_unit":1,
	   "additional_fee_cents":200,"free_threshold_cents":9900,"free_quantity":0}],
	"undeliverable_region_codes":["810000"]}`

func (cs couponShop) createFreight(t *testing.T, body string) api.AdminFreightTemplate {
	t.Helper()
	var out api.AdminFreightTemplate
	decodeInto(t, postIdem(t, cs.Host, "/api/v1/admin/freight-templates", body, cs.Token),
		http.StatusCreated, "建运费模板", &out)
	return out
}

// addressIn 给买家再加一条收货地址（省名、可选的区划码），返回 id。
func (cs couponShop) addressIn(t *testing.T, b couponBuyer, province string, regionCode *string) int64 {
	t.Helper()
	return adminQueryInt64(t, `
		INSERT INTO user_addresses (merchant_id, user_id, receiver_name, phone, province, city,
		                            district, street, detail, region_code)
		VALUES ($1, $2, '收件人', $3, $4, '某市', '某区', '某街道', '1 号', $5) RETURNING id`,
		cs.MerchantID, b.UserID, b.Phone, province, regionCode)
}

func orderBodyAt(addressID, storeID int64, lines [][2]int64, couponID *int64) string {
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		parts = append(parts, fmt.Sprintf(`{"sku_id":%d,"quantity":%d}`, l[0], l[1]))
	}
	s := fmt.Sprintf(`{"items":[%s],"address_id":%d,"store_id":%d`, strings.Join(parts, ","), addressID, storeID)
	if couponID != nil {
		s += fmt.Sprintf(`,"user_coupon_id":%d`, *couponID)
	}
	return s + "}"
}

func (cs couponShop) mustPreview(t *testing.T, b couponBuyer, body string) api.OrderPreview {
	t.Helper()
	pv, w := cs.preview(t, b, body)
	if w.Code != http.StatusOK {
		t.Fatalf("试算失败：%d %s", w.Code, w.Body.String())
	}
	return pv
}

// 没配任何运费模板的店：运费是**算出来的 0**，字段必返，明细说清楚是 no_template。
// 这是 00042 之前「整个不出现」那笔挂账的反方向。
func TestPreviewAndOrderCarryComputedFreight(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "nofreight")
	body := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 1}}, nil)

	pw := previewOrder(t, cs.Host, body, b.Token)
	wantStatus(t, pw, http.StatusOK, "试算")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(pw.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"freight_cents", "freight_discount_cents", "freight"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("试算响应里没有 %s（契约定成必返）：%s", k, pw.Body.String())
		}
	}
	var pv api.OrderPreview
	_ = json.Unmarshal(pw.Body.Bytes(), &pv)
	if pv.FreightCents != 0 || pv.PayableCents != 5000 || len(pv.Freight.Groups) != 1 ||
		pv.Freight.Groups[0].FreeReason == nil || *pv.Freight.Groups[0].FreeReason != "no_template" {
		t.Fatalf("没配模板时应是 0 运费、free_reason=no_template，实得 %s", pw.Body.String())
	}
	if pv.Freight.ProvinceCode == nil || *pv.Freight.ProvinceCode != "110000" {
		t.Fatalf("收货地址「北京」应归到 110000，实得 %v", pv.Freight.ProvinceCode)
	}

	var o api.Order
	decodeInto(t, createOrder(t, cs.Host, body, b.Token, "nf-"+uniqueKey()), http.StatusCreated, "下单", &o)
	if o.FreightCents == nil || *o.FreightCents != 0 || o.FreightDiscountCents == nil {
		t.Fatalf("订单上的运费字段应出现且为 0：%+v", o)
	}
	var d api.OrderDetail
	decodeInto(t, getAs(t, cs.Host, "/api/v1/orders/"+o.OrderNo, b.Token), http.StatusOK, "订单详情", &d)
	if d.Freight == nil || len(d.Freight.Groups) != 1 {
		t.Fatalf("订单详情应带运费快照：%+v", d.Freight)
	}
}

// 验收主路径：全店默认模板 → 试算按地址算运费 → 满额包邮按「券后」判 → 下单写进订单 →
// 偏远地区另价 → 不配送地区行级 422。
func TestFreightTemplateEndToEnd(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createFreight(t, nationwideFreight)
	if !tpl.IsDefault || len(tpl.Rules) != 2 || len(tpl.Rules[1].RegionCodes) != 0 {
		t.Fatalf("建出来的模板不对（默认规则应排在最后）：%+v", tpl)
	}
	b := cs.newBuyer(t, "freight")

	// 北京 1 件衬衫：首件 8 元。
	pv := cs.mustPreview(t, b, orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 1}}, nil))
	if pv.FreightCents != 800 || pv.PayableCents != 5800 || *pv.Freight.Groups[0].TemplateId != tpl.Id {
		t.Fatalf("北京 1 件应收 800、应付 5800，实得运费 %d 应付 %d %+v", pv.FreightCents, pv.PayableCents, pv.Freight)
	}

	// 北京 2 件连衣裙 120 元：满 99 包邮。
	pv = cs.mustPreview(t, b, orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.DressSKU, 2}}, nil))
	if pv.FreightCents != 0 || *pv.Freight.Groups[0].FreeReason != "threshold" || pv.PayableCents != 12000 {
		t.Fatalf("120 元应满额包邮，实得运费 %d（%+v）", pv.FreightCents, pv.Freight.Groups[0])
	}

	// 计价顺序：2 件衬衫 100 元用满 100 减 20 → 券后 80 元 < 99，**不包邮**，收 8 + 2 = 10 元。
	coupon := cs.wholeStoreCoupon(t, b)
	body := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 2}}, &coupon.Id)
	pv = cs.mustPreview(t, b, body)
	if *pv.DiscountCents != 2000 || pv.FreightCents != 1000 || pv.PayableCents != 9000 {
		t.Fatalf("满 100 减 20 之后 80 元不满 99，应收运费 1000、应付 9000；实得优惠 %d 运费 %d 应付 %d",
			*pv.DiscountCents, pv.FreightCents, pv.PayableCents)
	}
	withExpect := strings.TrimSuffix(body, "}") + fmt.Sprintf(`,"expected_payable_cents":%d}`, pv.PayableCents)
	var o api.Order
	decodeInto(t, createOrder(t, cs.Host, withExpect, b.Token, "fr-"+uniqueKey()), http.StatusCreated, "下单", &o)
	var freight, fdisc, discount, payable, goods int64
	var snap []byte
	if err := admin(t).QueryRow(context.Background(), `
		SELECT freight_cents, freight_discount_cents, discount_cents, payable_cents, goods_amount_cents,
		       freight_snapshot FROM orders WHERE order_no = $1`, o.OrderNo).
		Scan(&freight, &fdisc, &discount, &payable, &goods, &snap); err != nil {
		t.Fatal(err)
	}
	if freight != 1000 || fdisc != 0 || payable != goods+freight-discount || payable != 9000 {
		t.Fatalf("库里的订单金额不对：运费 %d 抵运费 %d 优惠 %d 应付 %d 商品 %d", freight, fdisc, discount, payable, goods)
	}
	var fs api.FreightBreakdown
	if err := json.Unmarshal(snap, &fs); err != nil || len(fs.Groups) != 1 || fs.Groups[0].TemplateName == nil ||
		*fs.Groups[0].TemplateName != "默认运费" || fs.Groups[0].Rule == nil || fs.Groups[0].Rule.FirstFeeCents != 800 {
		t.Fatalf("运费快照应记下模板名与命中的规则：%s（%v）", snap, err)
	}

	// 改模板不影响已下的单：快照是下单那一刻的。
	wantStatus(t, putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/freight-templates/%d", tpl.Id),
		strings.Replace(nationwideFreight, `"first_fee_cents":800`, `"first_fee_cents":1500`, 1), cs.Token),
		http.StatusOK, "改模板")
	var d api.OrderDetail
	decodeInto(t, getAs(t, cs.Host, "/api/v1/orders/"+o.OrderNo, b.Token), http.StatusOK, "订单详情", &d)
	if d.Freight == nil || d.Freight.Groups[0].Rule.FirstFeeCents != 800 || *d.FreightCents != 1000 {
		t.Fatalf("改模板之后历史订单的运费快照变了：%+v", d.Freight)
	}

	// 新疆（区划码 650102）：偏远地区那条规则，首件 20 元，金额再高也不包邮。
	xj := cs.addressIn(t, b, "新疆维吾尔自治区", ptrStr("650102"))
	pv = cs.mustPreview(t, b, orderBodyAt(xj, cs.NorthStore, [][2]int64{{cs.DressSKU, 2}}, nil))
	if pv.FreightCents != 3000 || pv.Freight.Groups[0].FreeReason != nil {
		t.Fatalf("新疆 2 件应收 2000 + 1000 = 3000 且不包邮，实得 %d %+v", pv.FreightCents, pv.Freight.Groups[0])
	}

	// 香港：不配送，试算与下单都 422，逐行给原因。
	hk := cs.addressIn(t, b, "香港特别行政区", nil)
	hkBody := orderBodyAt(hk, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 1}, {cs.DressSKU, 1}}, nil)
	pw := previewOrder(t, cs.Host, hkBody, b.Token)
	cw := createOrder(t, cs.Host, hkBody, b.Token, "hk-"+uniqueKey())
	for _, w := range []struct {
		what string
		code int
		body []byte
	}{{"试算", pw.Code, pw.Body.Bytes()}, {"下单", cw.Code, cw.Body.Bytes()}} {
		var p api.Problem
		if err := json.Unmarshal(w.body, &p); err != nil || w.code != http.StatusUnprocessableEntity ||
			p.Type != problem.TypeRegionNotDeliverable {
			t.Fatalf("%s寄往香港应 422 region-not-deliverable，实得 %d %s", w.what, w.code, w.body)
		}
		if p.UndeliverableItems == nil || len(*p.UndeliverableItems) != 2 {
			t.Fatalf("%s：两行都送不到，应逐行列出，实得 %s", w.what, w.body)
		}
		for _, it := range *p.UndeliverableItems {
			if it.ReasonCode != "region_excluded" || !strings.Contains(it.Reason, "香港") {
				t.Fatalf("%s：原因应是 region_excluded 且说出省名，实得 %+v", w.what, it)
			}
		}
	}
}

func ptrStr(s string) *string { return &s }

func moneyOf(m *api.Money) int64 {
	if m == nil {
		return -1
	}
	return int64(*m)
}

// 包邮券：抵运费、最多抵到 0、封顶；本单运费为 0 时不可用；「本单可用券」里的抵扣额
// 与带上它试算逐分相等；整单退款退的是实收运费。
func TestFreeShippingCoupon(t *testing.T) {
	cs := newCouponShop(t)
	cs.createFreight(t, nationwideFreight)
	b := cs.newBuyer(t, "freeship")

	tpl := cs.createTemplate(t, `{"name":"包邮券","coupon_type":4,"valid_mode":2,"valid_days":7}`)
	if tpl.CouponType != 4 {
		t.Fatalf("包邮券应能建出来：%+v", tpl)
	}
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true,"per_user_limit":5}`), http.StatusOK, "设为可领")
	c := cs.mustClaim(t, b, tpl.Id)

	// 不带券试算：本单可用券里有它，能抵 800（= 这一单的运费）。
	one := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 1}}, nil)
	pv := cs.mustPreview(t, b, one)
	var listed int64 = -1
	for _, ac := range *pv.ApplicableCoupons {
		if ac.Id == c.Id {
			listed = int64(ac.ApplicableDiscountCents)
		}
	}
	if listed != 800 {
		t.Fatalf("本单可用券里包邮券应能抵 800，实得 %d（%+v）", listed, *pv.ApplicableCoupons)
	}

	// 带上它：运费 800 全抵，优惠 800，应付 = 商品 5000。
	withCoupon := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 1}}, &c.Id)
	pv = cs.mustPreview(t, b, withCoupon)
	if pv.FreightCents != 800 || pv.FreightDiscountCents != 800 || *pv.DiscountCents != 800 ||
		pv.PayableCents != 5000 || pv.Freight.FreightDiscountCents != 800 {
		t.Fatalf("包邮券应把 800 运费抵到 0：运费 %d 抵 %d 优惠 %d 应付 %d",
			pv.FreightCents, pv.FreightDiscountCents, *pv.DiscountCents, pv.PayableCents)
	}
	for _, it := range pv.Items {
		if *it.DiscountCents != 0 {
			t.Fatalf("包邮券不该分摊到行，sku %d 分到了 %d", *it.SkuId, *it.DiscountCents)
		}
	}
	if int64(*pv.DiscountCents) != listed {
		t.Fatalf("本单可用券说抵 %d，带上它试算优惠 %d —— 两边不一致", listed, *pv.DiscountCents)
	}

	// 已满额包邮的单：运费 0，包邮券抵不了钱 → 409，而不是「用掉一张减 0 的券」。
	free := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.DressSKU, 2}}, &c.Id)
	if p := problemOf(t, previewOrder(t, cs.Host, free, b.Token), http.StatusConflict); p.Type != problem.TypeCouponNotApplicable {
		t.Fatalf("运费为 0 时包邮券应 409 coupon-not-applicable，实得 %+v", p)
	}
	pv = cs.mustPreview(t, b, orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.DressSKU, 2}}, nil))
	for _, ac := range *pv.ApplicableCoupons {
		if ac.Id == c.Id {
			t.Fatalf("运费为 0 的单，本单可用券里不该有包邮券：%+v", ac)
		}
	}

	// 下单、付款、未发货整单退：退的是**实收运费** 0，合计 = 实付 5000。
	o := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, &c.Id)
	var fdisc, disc int64
	if err := admin(t).QueryRow(context.Background(),
		`SELECT freight_discount_cents, discount_cents FROM orders WHERE order_no = $1`, o.OrderNo).
		Scan(&fdisc, &disc); err != nil {
		t.Fatal(err)
	}
	if fdisc != 800 || disc != 800 || o.PayableCents != 5000 {
		t.Fatalf("订单上应记抵运费 800、优惠 800、应付 5000，实得 %d / %d / %d", fdisc, disc, o.PayableCents)
	}
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}))
	if moneyOf(r.FreightCents) != 0 || r.AmountCents != 5000 {
		t.Fatalf("包邮券抵掉的运费买家没付过：整单退应退运费 0、合计 5000，实得运费 %d 合计 %d",
			moneyOf(r.FreightCents), r.AmountCents)
	}

	// 封顶：最多抵 5 元 → 运费 8 元抵 5 元，应付 5300。
	capped := cs.createTemplate(t, `{"name":"运费减 5 元","coupon_type":4,"max_discount_cents":500,"valid_mode":2,"valid_days":7}`)
	wantStatus(t, cs.patchTemplate(t, capped.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	cc := cs.mustClaim(t, b, capped.Id)
	pv = cs.mustPreview(t, b, orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 1}}, &cc.Id))
	if pv.FreightDiscountCents != 500 || pv.PayableCents != 5300 {
		t.Fatalf("封顶 500 的包邮券应抵 500、应付 5300，实得抵 %d 应付 %d", pv.FreightDiscountCents, pv.PayableCents)
	}

	// POST /coupons/applicable：不带地址时判不了包邮券，结果里没有它；带了就有。
	for _, ac := range cs.applicable(t, b, cs.NorthStore, cs.ShirtSKU, 1) {
		if ac.CouponType == 4 {
			t.Fatalf("不带 address_id 时不该出现包邮券：%+v", ac)
		}
	}
	var withAddr []api.ApplicableCoupon
	decodeInto(t, post(t, cs.Host, "/api/v1/coupons/applicable",
		fmt.Sprintf(`{"items":[{"sku_id":%d,"quantity":1}],"store_id":%d,"address_id":%d}`,
			cs.ShirtSKU, cs.NorthStore, b.Address), b.Token), http.StatusOK, "本单可用券（带地址）", &withAddr)
	found := false
	for _, ac := range withAddr {
		if ac.Id == cc.Id && ac.ApplicableDiscountCents == 500 {
			found = true
		}
	}
	if !found {
		t.Fatalf("带了 address_id，本单可用券里应有能抵 500 的包邮券：%+v", withAddr)
	}
}

// 未发货整单退：全退实收运费；退货退款审核裁定运费的上限也是实收运费。
func TestRefundFreightUsesTheFreightActuallyPaid(t *testing.T) {
	cs := newCouponShop(t)
	cs.createFreight(t, nationwideFreight)
	b := cs.newBuyer(t, "rffreight")

	o := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	if o.PayableCents != 5800 {
		t.Fatalf("1 件衬衫 + 8 元运费应付 5800，实得 %d", o.PayableCents)
	}
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}))
	if moneyOf(r.FreightCents) != 800 || r.AmountCents != 5800 {
		t.Fatalf("未发货整单退应全退运费 800、合计 5800，实得 %d / %d", moneyOf(r.FreightCents), r.AmountCents)
	}

	// 退货退款：已发货、确认收货，审核裁定退运费超过实收 800 → 422；800 放行。
	o2 := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	wantStatus(t, cs.ship(t, o2.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	_, lines2 := cs.lines(t, b, o2.OrderNo)
	r2 := cs.mustApply(t, b, o2.OrderNo, refundBody(2, [2]int64{lines2[cs.ShirtSKU].Id, 1}))
	if p := problemOf(t, cs.audit(t, r2.RefundNo, `{"action":"approve","freight_cents":801}`),
		http.StatusUnprocessableEntity); p.Type != problem.TypeRefundFreightExceeded {
		t.Fatalf("裁定退运费 801 超过实收 800 应 422，实得 %+v", p)
	}
	var ok api.Refund
	decodeInto(t, cs.audit(t, r2.RefundNo, `{"action":"approve","freight_cents":800}`), http.StatusOK, "裁定退运费 800", &ok)
	if moneyOf(ok.FreightCents) != 800 || ok.AmountCents != 5800 {
		t.Fatalf("裁定退运费 800 之后应退 5800，实得 %+v", ok)
	}
}

// 商品单独挂一个按重量计费的模板、SKU 填重量；门店模板盖过全店默认；
// 商品不能挂门店模板；还挂着商品的模板删不掉、也不能改成门店模板。
func TestFreightTemplateResolutionAndAdminRules(t *testing.T) {
	cs := newCouponShop(t)
	def := cs.createFreight(t, nationwideFreight)
	heavy := cs.createFreight(t, `{"name":"大件按重量","charge_mode":2,
		"rules":[{"region_codes":[],"first_unit":1000,"first_fee_cents":1000,"additional_unit":500,
		          "additional_fee_cents":300,"free_threshold_cents":0,"free_quantity":0}]}`)
	b := cs.newBuyer(t, "resolve")

	// 连衣裙挂按重量的模板，重 800 克。2 件 = 1600 克 → 1000 + ⌈600/500⌉×300 = 1600。
	wantStatus(t, reqAs(t, http.MethodPatch, cs.Host, fmt.Sprintf("/api/v1/admin/skus/%d", cs.DressSKU),
		`{"weight_gram":800}`, cs.Token), http.StatusOK, "填重量")
	var p api.AdminProduct
	decodeInto(t, reqAs(t, http.MethodPatch, cs.Host, fmt.Sprintf("/api/v1/admin/products/%d", cs.DressProduct),
		fmt.Sprintf(`{"freight_template_id":%d}`, heavy.Id), cs.Token), http.StatusOK, "挂模板", &p)
	if p.FreightTemplateId == nil || *p.FreightTemplateId != heavy.Id {
		t.Fatalf("商品应挂上按重量的模板：%+v", p)
	}
	pv := cs.mustPreview(t, b, orderBodyAt(b.Address, cs.NorthStore,
		[][2]int64{{cs.DressSKU, 2}, {cs.ShirtSKU, 1}}, nil))
	// 两组求和：大件 1600 + 衬衫走全店默认（商品 170 元满 99 包邮）0。
	if pv.FreightCents != 1600 || len(pv.Freight.Groups) != 2 || pv.Freight.Groups[0].Units != 1600 {
		t.Fatalf("应是大件 1600 克收 1600 + 默认模板满额包邮，实得 %d %+v", pv.FreightCents, pv.Freight.Groups)
	}

	// 北京门店建门店模板（首件 3 元）：衬衫改走它，大件仍走商品挂的。
	store := cs.createFreight(t, fmt.Sprintf(`{"name":"北京店","store_id":%d,"charge_mode":1,
		"rules":[{"region_codes":[],"first_unit":1,"first_fee_cents":300,"additional_unit":1,
		          "additional_fee_cents":100,"free_threshold_cents":0,"free_quantity":0}]}`, cs.NorthStore))
	pv = cs.mustPreview(t, b, orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 1}}, nil))
	if pv.FreightCents != 300 || *pv.Freight.Groups[0].TemplateId != store.Id {
		t.Fatalf("北京门店的单应走门店模板收 300，实得 %d %+v", pv.FreightCents, pv.Freight.Groups)
	}
	// 广州门店没有门店模板：走全店默认。
	pv = cs.mustPreview(t, b, orderBodyAt(b.Address, cs.SouthStore, [][2]int64{{cs.ShirtSKU, 1}}, nil))
	if pv.FreightCents != 800 || *pv.Freight.Groups[0].TemplateId != def.Id {
		t.Fatalf("广州门店的单应走全店默认收 800，实得 %d", pv.FreightCents)
	}

	// 每店至多一个门店模板：409。
	if pt := problemOf(t, postIdem(t, cs.Host, "/api/v1/admin/freight-templates",
		fmt.Sprintf(`{"name":"又一个","store_id":%d,"charge_mode":1,"rules":[{"region_codes":[],"first_unit":1,
		"first_fee_cents":1,"additional_unit":1,"additional_fee_cents":1,"free_threshold_cents":0,"free_quantity":0}]}`,
			cs.NorthStore), cs.Token), http.StatusConflict); pt.Type != problem.TypeFreightTemplateConflict {
		t.Fatalf("同一家门店第二个门店模板应 409 freight-template-conflict，实得 %+v", pt)
	}
	// 商品不能挂门店模板：422。
	if pt := problemOf(t, reqAs(t, http.MethodPatch, cs.Host, fmt.Sprintf("/api/v1/admin/products/%d", cs.ShirtProduct),
		fmt.Sprintf(`{"freight_template_id":%d}`, store.Id), cs.Token), http.StatusUnprocessableEntity); pt.Type != problem.TypeInvalidRequest {
		t.Fatalf("商品挂门店模板应 422，实得 %+v", pt)
	}
	// 还挂着商品的模板：删不掉，也不能改成门店模板。
	if pt := problemOf(t, deleteAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/freight-templates/%d", heavy.Id), cs.Token),
		http.StatusConflict); pt.Type != problem.TypeFreightTemplateInUse {
		t.Fatalf("删还挂着商品的模板应 409 freight-template-in-use，实得 %+v", pt)
	}
	if pt := problemOf(t, putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/freight-templates/%d", heavy.Id),
		fmt.Sprintf(`{"name":"大件","store_id":%d,"charge_mode":2,"rules":[{"region_codes":[],"first_unit":1000,
		"first_fee_cents":1000,"additional_unit":500,"additional_fee_cents":300,"free_threshold_cents":0,"free_quantity":0}]}`,
			cs.SouthStore), cs.Token), http.StatusConflict); pt.Type != problem.TypeFreightTemplateInUse {
		t.Fatalf("还挂着商品的全店模板改成门店模板应 409，实得 %+v", pt)
	}
	// 解除挂靠（null）之后就能删；删掉之后连衣裙落回全店默认。
	wantStatus(t, reqAs(t, http.MethodPatch, cs.Host, fmt.Sprintf("/api/v1/admin/products/%d", cs.DressProduct),
		`{"freight_template_id":null}`, cs.Token), http.StatusOK, "解除挂靠")
	wantStatus(t, deleteAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/freight-templates/%d", heavy.Id), cs.Token),
		http.StatusNoContent, "删模板")
	wantStatus(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/freight-templates/%d", heavy.Id), cs.Token),
		http.StatusNotFound, "删掉的模板")

	// 模板不成立：同一个省出现两次 → 422，detail 说清楚是哪个省。
	pt := problemOf(t, postIdem(t, cs.Host, "/api/v1/admin/freight-templates", `{"name":"坏","charge_mode":1,
		"rules":[{"region_codes":["440000"],"first_unit":1,"first_fee_cents":1,"additional_unit":1,"additional_fee_cents":1,
		          "free_threshold_cents":0,"free_quantity":0},
		         {"region_codes":[],"first_unit":1,"first_fee_cents":1,"additional_unit":1,"additional_fee_cents":1,
		          "free_threshold_cents":0,"free_quantity":0}],
		"undeliverable_region_codes":["440000"]}`, cs.Token), http.StatusUnprocessableEntity)
	if pt.Detail == nil || !strings.Contains(*pt.Detail, "广东省") {
		t.Fatalf("同一个省既计费又不配送应 422 且说出省名，实得 %+v", pt)
	}

	// 列表：按门店筛。
	var page struct {
		Total int                        `json:"total"`
		Items []api.AdminFreightTemplate `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/freight-templates?store_id=%d", cs.NorthStore), cs.Token),
		http.StatusOK, "按门店筛", &page)
	if page.Total != 1 || page.Items[0].Id != store.Id {
		t.Fatalf("按北京门店筛应只有门店模板，实得 %+v", page)
	}
}

// 新建是幂等的：同一把 Idempotency-Key 重放拿到同一个模板，不建第二个。
func TestFreightTemplateCreateIsIdempotent(t *testing.T) {
	cs := newCouponShop(t)
	key := freshIdemKey()
	var first, second api.AdminFreightTemplate
	decodeInto(t, postWithKey(t, cs.Host, "/api/v1/admin/freight-templates", nationwideFreight, cs.Token, key),
		http.StatusCreated, "首次", &first)
	w := postWithKey(t, cs.Host, "/api/v1/admin/freight-templates", nationwideFreight, cs.Token, key)
	decodeInto(t, w, http.StatusCreated, "重放", &second)
	if w.Header().Get("Idempotency-Replayed") != "true" || second.Id != first.Id {
		t.Fatalf("重放应回首次那个模板并带 Idempotency-Replayed：%d vs %d，头 %q",
			first.Id, second.Id, w.Header().Get("Idempotency-Replayed"))
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM freight_templates WHERE merchant_id = $1`, cs.MerchantID); n != 1 {
		t.Fatalf("重放之后库里应只有 1 个模板，实得 %d", n)
	}
	// 同一把钥匙配不同的请求体：422。
	if p := problemOf(t, postWithKey(t, cs.Host, "/api/v1/admin/freight-templates",
		strings.Replace(nationwideFreight, "默认运费", "别的名字", 1), cs.Token, key),
		http.StatusUnprocessableEntity); p.Type != problem.TypeIdempotencyKeyReused {
		t.Fatalf("同一把钥匙配不同请求体应 422 idempotency-key-reused，实得 %+v", p)
	}
}

// 购物车按收货地址给行打「送不到」、算预估运费；不传 address_id 用默认地址；
// 指名一个不是你的地址 422。
func TestCartShowsFreightAndUndeliverableLines(t *testing.T) {
	cs := newCouponShop(t)
	cs.createFreight(t, nationwideFreight)
	// 车排在 newCouponShop 的清理之前删（t.Cleanup 后进先出）：cart_items 挂着 skus、carts 挂着 users。
	t.Cleanup(func() {
		adminExec(t, `DELETE FROM cart_items WHERE merchant_id = $1`, cs.MerchantID)
		adminExec(t, `DELETE FROM carts WHERE merchant_id = $1`, cs.MerchantID)
	})
	b := cs.newBuyer(t, "cartfreight")
	for _, sku := range []int64{cs.ShirtSKU, cs.DressSKU} {
		wantStatus(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/cart/items?store_id=%d", cs.NorthStore),
			fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, sku), b.Token, freshIdemKey()), http.StatusOK, "加购")
	}

	// 没有默认地址（newBuyer 建的那条不是默认）：freight 整个不出现。
	w := getAs(t, cs.Host, fmt.Sprintf("/api/v1/cart?store_id=%d", cs.NorthStore), b.Token)
	wantStatus(t, w, http.StatusOK, "购物车")
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if _, ok := raw["freight"]; ok {
		t.Fatalf("没有地址时 freight 应整个不出现：%s", w.Body.String())
	}

	// 指名北京地址：两件 110 元满 99 包邮。
	var cart api.Cart
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/cart?store_id=%d&address_id=%d", cs.NorthStore, b.Address), b.Token),
		http.StatusOK, "购物车（北京）", &cart)
	if cart.Freight == nil || cart.Freight.FreightCents != 0 || cart.AddressId == nil || *cart.AddressId != b.Address {
		t.Fatalf("北京 110 元应满额包邮：%+v", cart.Freight)
	}
	// 只勾衬衫（50 元）：不满 99，收 8 元 —— 改勾选的写接口同样按地址算。
	var sel api.Cart
	decodeInto(t, putAs(t, cs.Host, fmt.Sprintf("/api/v1/cart/selection?store_id=%d&address_id=%d", cs.NorthStore, b.Address),
		fmt.Sprintf(`{"selected":false,"item_ids":[%d]}`, cartItemOf(t, cart, cs.DressSKU)), b.Token),
		http.StatusOK, "取消勾选连衣裙", &sel)
	if sel.Freight == nil || sel.Freight.FreightCents != 800 {
		t.Fatalf("只勾 50 元的衬衫应收 800：%+v", sel.Freight)
	}

	// 默认地址设成香港：不传 address_id 就按它算，两行都标「送不到」。
	hk := cs.addressIn(t, b, "香港特别行政区", nil)
	adminExec(t, `UPDATE user_addresses SET is_default = TRUE WHERE id = $1`, hk)
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/cart?store_id=%d", cs.NorthStore), b.Token),
		http.StatusOK, "购物车（默认地址香港）", &cart)
	if cart.AddressId == nil || *cart.AddressId != hk {
		t.Fatalf("不传 address_id 应用默认地址 %d，实得 %v", hk, cart.AddressId)
	}
	for _, it := range cart.Items {
		if it.Undeliverable == nil || it.Undeliverable.ReasonCode != "region_excluded" {
			t.Fatalf("寄香港每一行都该标送不到：%+v", it)
		}
	}
	if cart.Freight == nil || cart.Freight.FreightCents != 0 || len(cart.Freight.Groups) != 0 {
		t.Fatalf("全都送不到时运费明细应为空：%+v", cart.Freight)
	}

	// 别人的地址：422，不静默改用默认地址。
	other := cs.newBuyer(t, "other")
	if p := problemOf(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/cart?store_id=%d&address_id=%d", cs.NorthStore, other.Address), b.Token),
		http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("指名别人的地址应 422，实得 %+v", p)
	}
}

func cartItemOf(t *testing.T, c api.Cart, skuID int64) int64 {
	t.Helper()
	for _, it := range c.Items {
		if it.SkuId == skuID {
			return it.Id
		}
	}
	t.Fatalf("车里没有 sku %d", skuID)
	return 0
}

// 全店模板的写只有管理员与操作员能做（契约 StaffRole 矩阵「运费模板：全店模板」那一行）。
// 权限矩阵里 POST /admin/freight-templates 打的是门店模板（一条路由只能登记一次），
// 这一条把全店模板那一半逐角色敲一遍：大区 / 门店管理员 403 role-forbidden，读照样放行。
func TestFreightMerchantTemplateWritesNeedMerchantWide(t *testing.T) {
	fx := newPermFixture(t)
	body := func() string {
		return fmt.Sprintf(`{"name":"全店 %s","charge_mode":1,"rules":[{"region_codes":[],"first_unit":1,
			"first_fee_cents":800,"additional_unit":1,"additional_fee_cents":200,"free_threshold_cents":0,
			"free_quantity":0}]}`, fx.next())
	}
	var tpl api.AdminFreightTemplate
	decodeInto(t, postIdem(t, fx.sh.Host, v1+"/admin/freight-templates", body(), fx.sh.Token),
		http.StatusCreated, "管理员建全店模板", &tpl)
	for _, role := range allPermRoles {
		tok := fx.tokens[role]
		wantDeny := role == roleRegion || role == roleStore
		cw := postIdem(t, fx.sh.Host, v1+"/admin/freight-templates", body(), tok)
		pw := putAs(t, fx.sh.Host, fmt.Sprintf(v1+"/admin/freight-templates/%d", tpl.Id), body(), tok)
		for what, w := range map[string]int{"建": cw.Code, "改": pw.Code} {
			switch {
			case wantDeny && w != http.StatusForbidden:
				t.Errorf("[%s] %s全店模板应 403，实得 %d", role, what, w)
			case !wantDeny && w != http.StatusCreated && w != http.StatusOK:
				t.Errorf("[%s] %s全店模板应放行，实得 %d", role, what, w)
			}
		}
		if wantDeny {
			if typ, _ := problemTypeOf(cw); typ != problem.TypeRoleForbidden {
				t.Errorf("[%s] 建全店模板应是 role-forbidden，实得 %q", role, typ)
			}
		}
		wantStatus(t, getAs(t, fx.sh.Host, fmt.Sprintf(v1+"/admin/freight-templates/%d", tpl.Id), tok),
			http.StatusOK, fmt.Sprintf("[%s] 读全店模板", role))
	}
	// 门店管理员也不能把门店模板「领」成全店模板（改归属，新旧两边都要过）。
	id := permStoreFreight(t, fx, fx.N1)
	w := putAs(t, fx.sh.Host, fmt.Sprintf(v1+"/admin/freight-templates/%d", id), body(), fx.tokens[roleStore])
	if typ, _ := problemTypeOf(w); w.Code != http.StatusForbidden || typ != problem.TypeRoleForbidden {
		t.Fatalf("门店管理员把自己的门店模板改成全店模板应 403 role-forbidden，实得 %d %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 权限矩阵用的夹具（permission_test.go 的 permMatrix 引用它们）
// ---------------------------------------------------------------------------

func permFreightBody(storeID int64) string {
	return fmt.Sprintf(`{"name":"门店运费 %d","store_id":%d,"charge_mode":1,"rules":[{"region_codes":[],
		"first_unit":1,"first_fee_cents":600,"additional_unit":1,"additional_fee_cents":100,
		"free_threshold_cents":0,"free_quantity":0}]}`, time.Now().UnixNano()%1000, storeID)
}

// permClearStoreFreight 删掉这家门店现有的门店模板（每店至多一个，下一格要建新的）。
func permClearStoreFreight(t *testing.T, fx *permFixture, storeID int64) {
	t.Helper()
	adminExec(t, `DELETE FROM freight_template_rules WHERE template_id IN
	              (SELECT id FROM freight_templates WHERE merchant_id = $1 AND store_id = $2)`, fx.sh.MerchantID, storeID)
	adminExec(t, `DELETE FROM freight_templates WHERE merchant_id = $1 AND store_id = $2`, fx.sh.MerchantID, storeID)
}

// permStoreFreight 以商家管理员身份给这家门店建一个新的门店模板，返回 id。
func permStoreFreight(t *testing.T, fx *permFixture, storeID int64) int64 {
	t.Helper()
	permClearStoreFreight(t, fx, storeID)
	var tpl api.AdminFreightTemplate
	decodeInto(t, postIdem(t, fx.sh.Host, v1+"/admin/freight-templates", permFreightBody(storeID), fx.sh.Token),
		http.StatusCreated, "夹具：建门店运费模板", &tpl)
	return tpl.Id
}
