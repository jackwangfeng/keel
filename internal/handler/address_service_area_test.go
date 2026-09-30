package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// GET /addresses?store_id=：每条地址标 in_service_area（2026-09-30，结算页自动选址用）。
//
// 北京门店围栏 116.30–116.50 × 39.80–40.00；广州门店不配围栏；再加默认门店（全国兜底）。
// 同一个买家四条地址：围栏内、围栏外（上海）、恰在围栏边上、没坐标（fixture 建的那条）。
//
// 「边上」取的是围栏**落库之后**的顶点（西南角），不是按 116.30 / 39.80 手写：
// 后台配围栏的契约里坐标是 float32，116.30 存进库是 116.30000305…，手写的「边上」其实在
// 围栏外 0.3 米（实测 ST_Intersects 为 false）。也不取边的中点：geography 的边是大圆弧，
// 纬线那条边并不沿纬线走。只有从库里读回来的顶点是精确在边界上的点。
//
// 后半段是这个特性最要紧的一条：in_service_area 与试算的围栏校验**判据一致**。
// 两边是两条 SQL（addresses.sql 的 ListUserAddressesForStore 与 stores.sql 的
// StoreServesPoint），这里逐条拿地址去试算对照 —— 标 false 的必须 422 address-out-of-range，
// 其余的必须试算得过。哪天有人只改了其中一条，这里当场红。
func TestAddressesInServiceArea(t *testing.T) {
	cs := newCouponShop(t)
	setFence(t, cs.adminShop, cs.NorthStore, 116.30, 39.80, 116.50, 40.00)
	def := adminQueryInt64(t, `SELECT id FROM stores WHERE merchant_id = $1 AND is_default AND deleted_at IS NULL`,
		cs.MerchantID)
	setStoreStock(t, cs.adminShop, def, cs.DressSKU, 10)

	b := cs.newBuyer(t, "area")
	addAddr := func(lat, lng float64) int64 {
		t.Helper()
		return adminQueryInt64(t,
			`INSERT INTO user_addresses (merchant_id, user_id, receiver_name, phone, province, city,
			                             district, street, detail, lat, lng)
			 VALUES ($1, $2, '收件人', $3, '北京', '北京', '朝阳', '某街道', '2 号', $4, $5) RETURNING id`,
			cs.MerchantID, b.UserID, b.Phone, lat, lng)
	}
	inside := addAddr(39.90, 116.40)
	outside := addAddr(31.23, 121.47)
	var edgeLat, edgeLng float64
	if err := admin(t).QueryRow(t.Context(),
		`SELECT ST_Y(ST_PointN(ST_ExteriorRing(fence::geometry), 1)),
		        ST_X(ST_PointN(ST_ExteriorRing(fence::geometry), 1))
		   FROM stores WHERE id = $1`, cs.NorthStore).Scan(&edgeLat, &edgeLng); err != nil {
		t.Fatal(err)
	}
	edge := addAddr(edgeLat, edgeLng)
	noPoint := b.Address

	yes, no := true, false
	want := map[int64]map[int64]*bool{
		cs.NorthStore: {inside: &yes, outside: &no, edge: &yes, noPoint: nil},
		cs.SouthStore: {inside: &yes, outside: &yes, edge: &yes, noPoint: &yes},
		def:           {inside: &yes, outside: &yes, edge: &yes, noPoint: &yes},
	}
	show := func(v *bool) string {
		if v == nil {
			return "null"
		}
		return fmt.Sprint(*v)
	}
	names := map[int64]string{inside: "围栏内", outside: "围栏外", edge: "围栏边上", noPoint: "没坐标"}

	for store, byAddr := range want {
		var list []api.Address
		decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/addresses?store_id=%d", store), b.Token),
			http.StatusOK, "按门店标地址簿", &list)
		if len(list) != len(byAddr) {
			t.Fatalf("门店 %d：地址簿应有 %d 条（只标不滤），实得 %d", store, len(byAddr), len(list))
		}
		for _, a := range list {
			if got, exp := show(a.InServiceArea), show(byAddr[a.Id]); got != exp {
				t.Errorf("门店 %d · %s地址：in_service_area 应为 %s，实得 %s", store, names[a.Id], exp, got)
			}
		}

		// 判据一致：拿每条地址去这家门店试算。
		for addr, flag := range byAddr {
			buyer := b
			buyer.Address = addr
			_, w := cs.preview(t, buyer, cs.orderJSON(buyer, store, cs.DressSKU, 1, nil))
			if flag != nil && !*flag {
				if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Type != problem.TypeAddressOutOfRange {
					t.Errorf("门店 %d · %s地址标了 false，试算却是 %s", store, names[addr], p.Type)
				}
			} else if w.Code != http.StatusOK {
				t.Errorf("门店 %d · %s地址标了 %s，试算却失败：%d %s",
					store, names[addr], show(flag), w.Code, w.Body.String())
			}
		}
	}

	// 不带 store_id：字段整个不出现（契约：缺省）。看原始 JSON，不看解码后的指针 ——
	// 解码分不开「缺省」与「null」，而这里要钉的是前者。
	w := getAs(t, cs.Host, "/api/v1/addresses", b.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("地址簿：%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "in_service_area") {
		t.Fatalf("不带 store_id 时不该出现 in_service_area：%s", w.Body.String())
	}
	var plain []json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &plain); err != nil || len(plain) != 4 {
		t.Fatalf("不带 store_id 的地址簿应有 4 条：%v %s", err, w.Body.String())
	}

	// 别家店的门店与不存在的门店：422 invalid-request，与 GET /cart 的 store_id 同一个约定。
	other := newAdminShop(t)
	otherStore := adminQueryInt64(t,
		`SELECT id FROM stores WHERE merchant_id = $1 AND deleted_at IS NULL ORDER BY id LIMIT 1`, other.MerchantID)
	for what, id := range map[string]int64{"别家店的门店": otherStore, "不存在的门店": 1 << 50} {
		w := getAs(t, cs.Host, fmt.Sprintf("/api/v1/addresses?store_id=%d", id), b.Token)
		if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
			t.Errorf("%s应 422 invalid-request，实得 %s", what, p.Type)
		}
	}
}
