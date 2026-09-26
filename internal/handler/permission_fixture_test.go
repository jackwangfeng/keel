package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
)

// 分级权限那一组测试的夹具（v0.1.0，00025，internal/service/authz.go）。
//
// 数据一律走真实的后台接口建，员工也是 —— 用 POST /admin/staff 建出大区管理员
// 与门店管理员，本身就是在验「管理员能分配角色与范围」这条主路径。会话走真实的
// POST /admin/auth/session（staffSession），理由与 newAdminShop 一字不差。

// permRole 是矩阵的一个维度。平台级不在里面：平台级员工对一家店是全店范围，
// 与商家管理员同一个判据（auth.StaffIdentity.MerchantWide），开店那一条单独有
// admin_merchant_test.go 守着。
type permRole int

const (
	roleAdmin permRole = iota
	roleOperator
	roleRegion
	roleStore
)

func (r permRole) String() string {
	return [...]string{"商家管理员", "操作员", "大区管理员", "门店管理员"}[r]
}

var allPermRoles = []permRole{roleAdmin, roleOperator, roleRegion, roleStore}

// permFixture 是一家连锁：两个大区、三家门店、一件已上架的商品，外加四种角色
// 各一个会话。
//
//	华北（North）：N1、N2        大区管理员管华北；门店管理员管 N1
//	华东（East）： E1
//	默认大区：     S0（默认门店，newAdminShop 播的）
//
// 「范围内」的目标一律是华北 / N1，「范围外」的一律是华东 / E1 ——
// 对大区管理员与门店管理员同时成立，所以矩阵里一个 in 布尔就够。
type permFixture struct {
	sh adminShop

	North, East int64
	N1, N2, E1  int64
	CategoryID  int64
	ProductID   int64
	SKUID       int64
	OperatorID  int64
	RegionMgrID int64
	StoreMgrID  int64
	tokens      map[permRole]string
	seq         atomic.Int64
	solo        *soloFixture
}

// newPermFixture 建一家连锁并给四种角色各签一个会话。
func newPermFixture(t *testing.T) *permFixture {
	t.Helper()
	sh := newAdminShop(t)
	fx := &permFixture{sh: sh, tokens: map[permRole]string{roleAdmin: sh.Token}}

	fx.North = createRegion(t, sh, "north", "华北")
	fx.East = createRegion(t, sh, "east", "华东")
	fx.N1 = createStore(t, sh, fx.North, "n1", "华北一店", 116.40, 39.90)
	fx.N2 = createStore(t, sh, fx.North, "n2", "华北二店", 116.50, 39.95)
	fx.E1 = createStore(t, sh, fx.East, "e1", "华东一店", 121.47, 31.23)
	fx.CategoryID = createCategory(t, sh, "权限矩阵类目")
	fx.ProductID, fx.SKUID = createPublishedSKU(t, sh, fx.CategoryID, "权限矩阵商品", 1000)

	fx.OperatorID = fx.createStaff(t, sh.Token, 2, nil, nil)
	fx.RegionMgrID = fx.createStaff(t, sh.Token, 3, []int64{fx.North}, nil)
	fx.StoreMgrID = fx.createStaff(t, sh.Token, 4, nil, []int64{fx.N1})
	fx.tokens[roleOperator] = staffSession(t, sh.Host, fx.OperatorID).Token
	fx.tokens[roleRegion] = staffSession(t, sh.Host, fx.RegionMgrID).Token
	fx.tokens[roleStore] = staffSession(t, sh.Host, fx.StoreMgrID).Token
	return fx
}

// next 给这家店里的新对象一个不撞车的编号。
func (fx *permFixture) next() string {
	return fmt.Sprintf("%s-%d", fx.sh.Suffix, fx.seq.Add(1))
}

// createStaff 以 token 的身份建一个员工，断言 201，返回 id。
func (fx *permFixture) createStaff(t *testing.T, token string, role int, regions, stores []int64) int64 {
	t.Helper()
	var st api.Staff
	decodeInto(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff",
		staffBody(fmt.Sprintf("p%s@keel.test", fx.next()), role, regions, stores), token),
		http.StatusCreated, "建员工", &st)
	return st.Id
}

// staffBody 拼 POST /admin/staff 的请求体。nil 的范围不出现在请求体里。
func staffBody(email string, role int, regions, stores []int64) string {
	m := map[string]any{"email": email, "role": role}
	if regions != nil {
		m["region_ids"] = regions
	}
	if stores != nil {
		m["store_ids"] = stores
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// freshStore 以管理员身份在给定大区里建一家新门店。给会被删掉的那几格用，
// 免得一格删掉的门店让下一格变成 404。
func (fx *permFixture) freshStore(t *testing.T, region int64) int64 {
	t.Helper()
	return createStore(t, fx.sh, region, "tmp"+fx.next(), "临时门店", 116.3, 39.8)
}

// freshDraftProduct 建一件草稿商品（带一个 SKU），给删商品 / 删 SKU 用 ——
// 已上架的删不掉（409），那会让「放行」那一格看起来像被拒。
func (fx *permFixture) freshDraftProduct(t *testing.T) (productID, skuID int64) {
	t.Helper()
	var p api.AdminProduct
	decodeInto(t, postIdem(t, fx.sh.Host, "/api/v1/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":"草稿 %s"}`, fx.CategoryID, fx.next()), fx.sh.Token),
		http.StatusCreated, "建草稿商品", &p)
	var sku api.AdminSku
	decodeInto(t, postIdem(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", p.Id),
		fmt.Sprintf(`{"sku_code":"D-%s","price_cents":100}`, fx.next()), fx.sh.Token),
		http.StatusCreated, "给草稿商品建 SKU", &sku)
	return p.Id, sku.Id
}

// freshPublishedProduct 建一件已上架商品，给「下架」那一格用。
func (fx *permFixture) freshPublishedProduct(t *testing.T) int64 {
	t.Helper()
	// createPublishedSKU 的货号由价格拼出来，所以价格每次都得不一样。
	p, _ := createPublishedSKU(t, fx.sh, fx.CategoryID, "上架 "+fx.next(), 5000+fx.seq.Add(1))
	return p
}

// freshCategory 建一个根类目，给删类目用。
func (fx *permFixture) freshCategory(t *testing.T) int64 {
	t.Helper()
	var cat api.AdminCategory
	decodeInto(t, postIdem(t, fx.sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":"临时类目 %s","sort_order":1}`, fx.next()), fx.sh.Token),
		http.StatusCreated, "建类目", &cat)
	return cat.Id
}

// storeQty 以管理员身份读这家店这个 SKU 的当前水位（缺行是 0）。
// 改库存是 CAS，expected 必须是当前值，否则放行的那一格会以 409 失败。
func (fx *permFixture) storeQty(t *testing.T, storeID, skuID int64) int {
	t.Helper()
	var page struct {
		Items []api.AdminInventory `json:"items"`
	}
	decodeInto(t, getAs(t, fx.sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/inventories?page_size=100", storeID), fx.sh.Token),
		http.StatusOK, "读门店库存", &page)
	for _, in := range page.Items {
		if in.SkuId == skuID {
			return in.AvailableQty
		}
	}
	t.Fatalf("门店 %d 的库存清单里没有 sku %d", storeID, skuID)
	return 0
}

// soloFixture 是「只有一家未软删门店」的另一家店，给
// PUT /admin/skus/{sku_id}/inventory 那条捷径用：主夹具有四家店，
// 在那里打这条路径对谁都是 409 store-ambiguous，矩阵就什么也没验。
//
// 范围内 / 范围外由**人**区分，不由目标区分（目标只有那一家）：
//
//	大区管理员：范围内管默认大区；范围外管一个空的大区 X
//	门店管理员：范围内管 S0；范围外管一家建完就软删掉的门店 T
type soloFixture struct {
	sh     adminShop
	SKUID  int64
	tokens map[permCase]string
}

// permCase 是矩阵的一格：哪个角色、目标在不在范围内。
type permCase struct {
	Role permRole
	In   bool
}

func (c permCase) String() string {
	if c.In {
		return c.Role.String() + "/范围内"
	}
	return c.Role.String() + "/范围外"
}

func (fx *permFixture) soloShop(t *testing.T) *soloFixture {
	t.Helper()
	if fx.solo != nil {
		return fx.solo
	}
	sh := newAdminShop(t)
	so := &soloFixture{sh: sh, tokens: map[permCase]string{}}
	cat := createCategory(t, sh, "单店类目")
	_, so.SKUID = createPublishedSKU(t, sh, cat, "单店商品", 800)

	sub := &permFixture{sh: sh}
	defaultRegion := adminQueryInt64(t, `SELECT region_id FROM stores WHERE id = $1`, sh.StoreID)
	emptyRegion := createRegion(t, sh, "x", "空大区")
	gone := createStore(t, sh, emptyRegion, "t", "要软删的店", 116.1, 39.1)

	op := sub.createStaff(t, sh.Token, 2, nil, nil)
	rIn := sub.createStaff(t, sh.Token, 3, []int64{defaultRegion}, nil)
	rOut := sub.createStaff(t, sh.Token, 3, []int64{emptyRegion}, nil)
	sIn := sub.createStaff(t, sh.Token, 4, nil, []int64{sh.StoreID})
	sOut := sub.createStaff(t, sh.Token, 4, nil, []int64{gone})
	wantStatus(t, deleteAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", gone), sh.Token),
		http.StatusNoContent, "软删门店 T，让这家店回到只有一家门店")

	opTok := staffSession(t, sh.Host, op).Token
	so.tokens[permCase{roleAdmin, true}] = sh.Token
	so.tokens[permCase{roleAdmin, false}] = sh.Token
	so.tokens[permCase{roleOperator, true}] = opTok
	so.tokens[permCase{roleOperator, false}] = opTok
	so.tokens[permCase{roleRegion, true}] = staffSession(t, sh.Host, rIn).Token
	so.tokens[permCase{roleRegion, false}] = staffSession(t, sh.Host, rOut).Token
	so.tokens[permCase{roleStore, true}] = staffSession(t, sh.Host, sIn).Token
	so.tokens[permCase{roleStore, false}] = staffSession(t, sh.Host, sOut).Token
	fx.solo = so
	return so
}

// sendAs 发一个请求。POST 一律带新鲜的 Idempotency-Key（reqAs 已经这么做）。
func sendAs(t *testing.T, host string, r permReq, token string) *httptest.ResponseRecorder {
	t.Helper()
	if r.Upload {
		return uploadImage(t, adminShop{Host: host, Token: token}, "image/png", []byte("\x89PNG permission "+time.Now().String()))
	}
	if r.Import != nil {
		key := ""
		if r.Path == v1+"/admin/product-imports" {
			key = freshIdemKey()
		}
		return importReq(t, host, token, r.Path, r.Import, r.ImportCategories, key)
	}
	return reqAs(t, r.Method, host, r.Path, r.Body, token)
}

// problemTypeOf 读 Problem 的 type 与 detail；不是 Problem 就返回空串。
func problemTypeOf(w *httptest.ResponseRecorder) (string, string) {
	var p struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
		return "", ""
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return p.Type, p.Detail
}
