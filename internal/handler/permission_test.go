package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// ===========================================================================
// 后台权限矩阵：每一条 /admin/ 路由 × 每一种角色 × 范围内 / 范围外
// ===========================================================================
//
// 这是分级权限（v0.1.0，00025）的执行者。判据全在 internal/service/authz.go，
// 这张表把它们从外面逐格敲一遍：
//
//   - 应当放行的格子，断言**具体的成功状态码**（不是「不是 403」）—— 一个把
//     请求体写错了的格子会以 422 失败，而不是悄悄算作「放行」；
//   - 应当拒绝的格子，断言 403 **且** Problem 的 type 是指定的那一个
//     （role-forbidden / out-of-scope / staff-forbidden / platform-only）。
//
// ### 两个方向都锁（照 contract_test.go 的写法）
//
// TestEveryAdminRouteIsInThePermissionMatrix：路由表里每一条 /admin/ 路由都必须
// 在 permMatrix 里有一行（或在 permExempt 里写明为什么不在）；反过来，表里的每一行
// 都必须真的注册了。**以后有人新加一条后台接口却忘了做权限判断，这里当场红** ——
// 他得先在这张表里写下「谁能调」，而写下来的那一刻，没实现的判据就会让矩阵红。

// denyType 是拒绝那一格期望的 Problem type 后缀；空串表示放行。
type denyType string

const (
	allow          denyType = ""
	roleForbidden  denyType = "role-forbidden"
	outOfScope     denyType = "out-of-scope"
	staffForbidden denyType = "staff-forbidden"
	platformOnly   denyType = "platform-only"
)

// permReq 是一格要发的请求。Token 为空时用这个角色在主夹具里的会话。
type permReq struct {
	Method, Path, Body string
	Upload             bool
	Host, Token        string
	// OK 是放行时期望的状态码。
	OK int
}

// permRoute 是矩阵的一行。
type permRoute struct {
	Method, Path string // gin 的路由形状，与 testEngine.Routes() 逐字对得上
	Expect       func(c permCase) denyType
	Build        func(t *testing.T, fx *permFixture, c permCase) permReq
}

// ---------------------------------------------------------------------------
// 期望：与契约 StaffRole 描述里那张矩阵逐行对应
// ---------------------------------------------------------------------------

// everyone：读商品目录、看自己、看（收窄过的）列表。
func everyone(permCase) denyType { return allow }

// merchantWide：商品 / SKU / 基准价 / 类目 / 上传的写，建大区。
func merchantWide(c permCase) denyType {
	if c.Role == roleAdmin || c.Role == roleOperator {
		return allow
	}
	return roleForbidden
}

// adminOnly：设默认门店。
func adminOnly(c permCase) denyType {
	if c.Role == roleAdmin {
		return allow
	}
	return roleForbidden
}

// regionScoped：大区的改 / 删 / 价 / 上下架 / 商品列表。
func regionScoped(c permCase) denyType {
	switch c.Role {
	case roleAdmin, roleOperator:
		return allow
	case roleRegion:
		if c.In {
			return allow
		}
		return outOfScope
	default:
		return roleForbidden
	}
}

// storeManage：门店本身的建 / 改 / 删 / 围栏。
func storeManage(c permCase) denyType { return regionScoped(c) }

// storeOperate：门店价 / 上下架 / 库存，以及门店详情与门店维度的读。
func storeOperate(c permCase) denyType {
	switch c.Role {
	case roleAdmin, roleOperator:
		return allow
	default:
		if c.In {
			return allow
		}
		return outOfScope
	}
}

// staffWrite：建 / 改员工。
func staffWrite(c permCase) denyType {
	switch c.Role {
	case roleAdmin:
		return allow
	case roleRegion:
		if c.In {
			return allow
		}
		return outOfScope
	default:
		return staffForbidden
	}
}

// platformOnlyRoute：开店。商家级的四种角色一律 403 platform-only。
func platformOnlyRoute(permCase) denyType { return platformOnly }

// ---------------------------------------------------------------------------
// 目标选择：范围内一律华北 / N1，范围外一律华东 / E1
// ---------------------------------------------------------------------------

func (fx *permFixture) region(c permCase) int64 {
	if c.In {
		return fx.North
	}
	return fx.East
}

func (fx *permFixture) store(c permCase) int64 {
	if c.In {
		return fx.N1
	}
	return fx.E1
}

func permGet(path string) permReq {
	return permReq{Method: http.MethodGet, Path: path, OK: http.StatusOK}
}

const v1 = "/api/v1"

// permMatrix 是全部 /admin/ 路由（除 permExempt 里那三条未认证的）。
var permMatrix = []permRoute{
	// —— 身份与员工
	{"GET", v1 + "/admin/me", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/me")
	}},
	{"GET", v1 + "/admin/staff", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/staff")
	}},
	{"POST", v1 + "/admin/staff", staffWrite, func(t *testing.T, fx *permFixture, c permCase) permReq {
		// 建一个门店管理员：范围内管 N1，范围外管 E1。
		return permReq{Method: "POST", Path: v1 + "/admin/staff", OK: http.StatusCreated,
			Body: staffBody("m"+fx.next()+"@keel.test", 4, nil, []int64{fx.store(c)})}
	}},
	{"PATCH", v1 + "/admin/staff/:staff_id", staffWrite, func(t *testing.T, fx *permFixture, c permCase) permReq {
		// 目标是一个新建的门店管理员（由管理员建）：范围内的管 N1，范围外的管 E1。
		// 改的是 status（原样的 1）：一个不会把目标弄坏、下一格还能再用的改动。
		target := fx.createStaff(t, fx.sh.Token, 4, nil, []int64{fx.store(c)})
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/staff/%d", target),
			Body: `{"status":1}`, OK: http.StatusOK}
	}},
	{"POST", v1 + "/admin/staff/:staff_id/login-token", staffWrite, func(t *testing.T, fx *permFixture, c permCase) permReq {
		// 重签登录 token：判据与 PATCH 同一个（authorizeStaffWrite），目标也同一个 ——
		// 一个新建的门店管理员，范围内管 N1，范围外管 E1。
		target := fx.createStaff(t, fx.sh.Token, 4, nil, []int64{fx.store(c)})
		return permReq{Method: "POST", Path: fmt.Sprintf(v1+"/admin/staff/%d/login-token", target),
			OK: http.StatusCreated}
	}},
	{"POST", v1 + "/admin/merchants", platformOnlyRoute, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: v1 + "/admin/merchants", OK: http.StatusCreated,
			Body: fmt.Sprintf(`{"code":"pm%d","name":"不该建出来的店","admin_email":"x%s@keel.test"}`,
				fx.seq.Add(1), fx.next())}
	}},
	// 商家目录的读与改（租户管理那一支加的）。与开店同一个判据：**只有平台级**。
	// 这张表只有商家级的四种角色，所以四格全拒；平台级能调由 tenant_switch_test.go 证明。
	// 目标用本店自己的商家 id —— 连自己店的名字都改不了，才说明拦的是角色而不是目标。
	{"GET", v1 + "/admin/merchants", platformOnlyRoute, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/merchants")
	}},
	{"GET", v1 + "/admin/merchants/:merchant_id", platformOnlyRoute, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(fmt.Sprintf(v1+"/admin/merchants/%d", fx.sh.MerchantID))
	}},
	{"PATCH", v1 + "/admin/merchants/:merchant_id", platformOnlyRoute, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/merchants/%d", fx.sh.MerchantID),
			Body: `{"name":"不该被改的店名"}`, OK: http.StatusOK}
	}},

	// —— 优惠券（合并时登记）。券直接决定实付，与商品、基准价同一个判据：全店范围。
	// 大区 / 门店管理员不行：券的适用范围可以跨大区，一个只管华北的人不该能发一张
	// 全国通用的券。每格现场建一张新模板（没发出过，券面与范围还能改）。
	{"GET", v1 + "/admin/coupon-templates", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/coupon-templates")
	}},
	{"POST", v1 + "/admin/coupon-templates", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: v1 + "/admin/coupon-templates", Body: permCouponBody(fx), OK: http.StatusCreated}
	}},
	{"GET", v1 + "/admin/coupon-templates/:template_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(fmt.Sprintf(v1+"/admin/coupon-templates/%d", permCouponTemplate(t, fx)))
	}},
	{"PATCH", v1 + "/admin/coupon-templates/:template_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/coupon-templates/%d", permCouponTemplate(t, fx)),
			Body: `{"name":"改个名字"}`, OK: http.StatusOK}
	}},
	{"PUT", v1 + "/admin/coupon-templates/:template_id/scopes", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/coupon-templates/%d/scopes", permCouponTemplate(t, fx)),
			Body: `{"scopes":[{"scope_type":1,"include":true}]}`, OK: http.StatusOK}
	}},
	{"POST", v1 + "/admin/coupon-templates/:template_id/grants", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: fmt.Sprintf(v1+"/admin/coupon-templates/%d/grants", permCouponTemplate(t, fx)),
			Body: fmt.Sprintf(`{"phones":[%q]}`, permBuyerPhone(t, fx)), OK: http.StatusCreated}
	}},

	// —— 营销活动（00044）。与券同一行：活动直接决定实付，判据是全店范围。
	// 每格现场建一个新活动（下线状态，规则还能改）。
	{"GET", v1 + "/admin/promotions", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/promotions")
	}},
	{"POST", v1 + "/admin/promotions", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		permCleanupPromotions(t, fx)
		return permReq{Method: "POST", Path: v1 + "/admin/promotions", Body: permPromotionBody(fx), OK: http.StatusCreated}
	}},
	{"GET", v1 + "/admin/promotions/:promotion_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(fmt.Sprintf(v1+"/admin/promotions/%d", permPromotion(t, fx)))
	}},
	{"PATCH", v1 + "/admin/promotions/:promotion_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/promotions/%d", permPromotion(t, fx)),
			Body: `{"name":"改个名字"}`, OK: http.StatusOK}
	}},

	// —— 上传与商品目录
	{"POST", v1 + "/admin/uploads", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: v1 + "/admin/uploads", Upload: true, OK: http.StatusCreated}
	}},
	{"GET", v1 + "/admin/products", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/products")
	}},
	{"POST", v1 + "/admin/products", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: v1 + "/admin/products", OK: http.StatusCreated,
			Body: fmt.Sprintf(`{"category_id":%d,"title":"新品 %s"}`, fx.CategoryID, fx.next())}
	}},
	{"GET", v1 + "/admin/products/:product_id", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(fmt.Sprintf(v1+"/admin/products/%d", fx.ProductID))
	}},
	{"PATCH", v1 + "/admin/products/:product_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/products/%d", fx.ProductID),
			Body: fmt.Sprintf(`{"subtitle":"副标题 %s"}`, fx.next()), OK: http.StatusOK}
	}},
	{"DELETE", v1 + "/admin/products/:product_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		p, _ := fx.freshDraftProduct(t)
		return permReq{Method: "DELETE", Path: fmt.Sprintf(v1+"/admin/products/%d", p), OK: http.StatusNoContent}
	}},
	{"POST", v1 + "/admin/products/:product_id/publication", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		p := fx.freshPublishedProduct(t)
		return permReq{Method: "POST", Path: fmt.Sprintf(v1+"/admin/products/%d/publication", p),
			Body: `{"action":"unpublish"}`, OK: http.StatusOK}
	}},
	{"PUT", v1 + "/admin/products/:product_id/images", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/products/%d/images", fx.ProductID),
			Body: `{"images":[]}`, OK: http.StatusOK}
	}},
	{"POST", v1 + "/admin/products/:product_id/skus", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: fmt.Sprintf(v1+"/admin/products/%d/skus", fx.ProductID),
			Body: fmt.Sprintf(`{"sku_code":"M-%s","price_cents":300}`, fx.next()), OK: http.StatusCreated}
	}},
	{"PATCH", v1 + "/admin/skus/:sku_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/skus/%d", fx.SKUID),
			Body: `{"price_cents":1000}`, OK: http.StatusOK}
	}},
	{"DELETE", v1 + "/admin/skus/:sku_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		_, sku := fx.freshDraftProduct(t)
		return permReq{Method: "DELETE", Path: fmt.Sprintf(v1+"/admin/skus/%d", sku), OK: http.StatusNoContent}
	}},
	{"PUT", v1 + "/admin/skus/:sku_id/inventory", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		// 捷径只在「恰好一家门店」时可用，所以这一行打的是另一家单店的店；
		// 范围内外由人区分（soloFixture 的注释）。
		so := fx.soloShop(t)
		cur := (&permFixture{sh: so.sh}).storeQty(t, so.sh.StoreID, so.SKUID)
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/skus/%d/inventory", so.SKUID),
			Body: fmt.Sprintf(`{"expected_available_qty":%d,"available_qty":%d}`, cur, cur+1),
			Host: so.sh.Host, Token: so.tokens[c], OK: http.StatusOK}
	}},
	{"GET", v1 + "/admin/categories", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/categories")
	}},
	{"POST", v1 + "/admin/categories", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: v1 + "/admin/categories", OK: http.StatusCreated,
			Body: fmt.Sprintf(`{"name":"类目 %s","sort_order":1}`, fx.next())}
	}},
	{"PATCH", v1 + "/admin/categories/:category_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/categories/%d", fx.CategoryID),
			Body: `{"sort_order":2}`, OK: http.StatusOK}
	}},
	{"DELETE", v1 + "/admin/categories/:category_id", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "DELETE", Path: fmt.Sprintf(v1+"/admin/categories/%d", fx.freshCategory(t)),
			OK: http.StatusNoContent}
	}},

	// —— 大区
	{"GET", v1 + "/admin/regions", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/regions")
	}},
	{"POST", v1 + "/admin/regions", merchantWide, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: v1 + "/admin/regions", OK: http.StatusCreated,
			Body: fmt.Sprintf(`{"code":"r-%s","name":"新大区"}`, fx.next())}
	}},
	{"PATCH", v1 + "/admin/regions/:region_id", regionScoped, func(t *testing.T, fx *permFixture, c permCase) permReq {
		name := "华东"
		if c.In {
			name = "华北"
		}
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/regions/%d", fx.region(c)),
			Body: fmt.Sprintf(`{"name":%q}`, name), OK: http.StatusOK}
	}},
	{"DELETE", v1 + "/admin/regions/:region_id", regionScoped, func(t *testing.T, fx *permFixture, c permCase) permReq {
		// 两个大区名下都有门店，放行的那一格是 409 region-has-stores —— 那说明
		// 判权已经过了、走到了业务规则上。用它而不是真删一个大区：真删的话
		// 下一格就是 404，而矩阵的每一格必须互不影响。
		return permReq{Method: "DELETE", Path: fmt.Sprintf(v1+"/admin/regions/%d", fx.region(c)),
			OK: http.StatusConflict}
	}},
	{"GET", v1 + "/admin/regions/:region_id/products", regionScoped, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(fmt.Sprintf(v1+"/admin/regions/%d/products", fx.region(c)))
	}},
	{"PUT", v1 + "/admin/regions/:region_id/products/:product_id/listing", regionScoped, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/regions/%d/products/%d/listing",
			fx.region(c), fx.ProductID), Body: `{"listed":true}`, OK: http.StatusOK}
	}},
	{"PUT", v1 + "/admin/regions/:region_id/skus/:sku_id/price", regionScoped, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/regions/%d/skus/%d/price",
			fx.region(c), fx.SKUID), Body: `{"price_cents":900}`, OK: http.StatusOK}
	}},
	{"DELETE", v1 + "/admin/regions/:region_id/skus/:sku_id/price", regionScoped, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "DELETE", Path: fmt.Sprintf(v1+"/admin/regions/%d/skus/%d/price",
			fx.region(c), fx.SKUID), OK: http.StatusNoContent}
	}},

	// —— 门店
	{"GET", v1 + "/admin/stores", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/stores")
	}},
	{"POST", v1 + "/admin/stores", storeManage, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "POST", Path: v1 + "/admin/stores", OK: http.StatusCreated,
			Body: fmt.Sprintf(`{"region_id":%d,"code":"s-%s","name":"新门店"}`, fx.region(c), fx.next())}
	}},
	{"GET", v1 + "/admin/stores/:store_id", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(fmt.Sprintf(v1+"/admin/stores/%d", fx.store(c)))
	}},
	{"PATCH", v1 + "/admin/stores/:store_id", storeManage, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PATCH", Path: fmt.Sprintf(v1+"/admin/stores/%d", fx.store(c)),
			Body: `{"phone":"010-1234"}`, OK: http.StatusOK}
	}},
	{"DELETE", v1 + "/admin/stores/:store_id", storeManage, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "DELETE", Path: fmt.Sprintf(v1+"/admin/stores/%d", fx.freshStore(t, fx.region(c))),
			OK: http.StatusNoContent}
	}},
	{"PUT", v1 + "/admin/stores/:store_id/fence", storeManage, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/stores/%d/fence", fx.store(c)),
			Body: `{"fence":{"type":"Polygon","coordinates":[[[116,39],[117,39],[117,40],[116,40],[116,39]]]}}`,
			OK:   http.StatusOK}
	}},
	{"PUT", v1 + "/admin/stores/:store_id/default", adminOnly, func(t *testing.T, fx *permFixture, c permCase) permReq {
		// 目标一律是现在的默认门店 S0：把它再设一次默认是幂等的，
		// 不会让矩阵的后面几格换一个默认店。
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/stores/%d/default", fx.sh.StoreID),
			OK: http.StatusOK}
	}},
	{"GET", v1 + "/admin/stores/:store_id/products", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(fmt.Sprintf(v1+"/admin/stores/%d/products", fx.store(c)))
	}},
	{"PUT", v1 + "/admin/stores/:store_id/products/:product_id/listing", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/stores/%d/products/%d/listing",
			fx.store(c), fx.ProductID), Body: `{"listed":true}`, OK: http.StatusOK}
	}},
	{"PUT", v1 + "/admin/stores/:store_id/skus/:sku_id/price", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/stores/%d/skus/%d/price",
			fx.store(c), fx.SKUID), Body: `{"price_cents":800}`, OK: http.StatusOK}
	}},
	{"DELETE", v1 + "/admin/stores/:store_id/skus/:sku_id/price", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: "DELETE", Path: fmt.Sprintf(v1+"/admin/stores/%d/skus/%d/price",
			fx.store(c), fx.SKUID), OK: http.StatusNoContent}
	}},
	{"GET", v1 + "/admin/stores/:store_id/inventories", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(fmt.Sprintf(v1+"/admin/stores/%d/inventories", fx.store(c)))
	}},
	{"PUT", v1 + "/admin/stores/:store_id/skus/:sku_id/inventory", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		cur := fx.storeQty(t, fx.store(c), fx.SKUID)
		return permReq{Method: "PUT", Path: fmt.Sprintf(v1+"/admin/stores/%d/skus/%d/inventory",
			fx.store(c), fx.SKUID),
			Body: fmt.Sprintf(`{"expected_available_qty":%d,"available_qty":%d}`, cur, cur+1),
			OK:   http.StatusOK}
	}},

	// —— 订单后半程（00033）。契约的 StaffRole 矩阵里没有「发货」这一行，
	// 判据取「门店库存」那一行（storeOperate）：货从哪家店出，就由管那家店库存的人发。
	// 理由写在 service/order_fulfillment.go 的 Ship 上。每格现场造一笔新的已支付订单，
	// 挂在范围内（N1）或范围外（E1）的门店上 —— 共用一笔的话第二格就是 409 已发过货。
	{"POST", v1 + "/admin/orders/:order_no/shipments", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		no := permPaidOrder(t, fx, fx.store(c))
		return permReq{Method: "POST", Path: v1 + "/admin/orders/" + no + "/shipments",
			Body: fmt.Sprintf(`{"carrier_code":"sf","tracking_no":"SF%s"}`, fx.next()), OK: http.StatusCreated}
	}},
	// 退款审核与确认收到退货（00034）：与发货同一个判据，按订单的履约门店。
	// 审核用驳回（带理由）——通过会进 30 并触发沙箱渠道，与权限无关的副作用越少越好。
	{"POST", v1 + "/admin/refunds/:refund_no/audit", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		no := permRefund(t, fx, fx.store(c), 10)
		return permReq{Method: "POST", Path: v1 + "/admin/refunds/" + no + "/audit",
			Body: `{"action":"reject","reject_reason":"权限矩阵驳回"}`, OK: http.StatusOK}
	}},
	{"POST", v1 + "/admin/refunds/:refund_no/receipt", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		no := permRefund(t, fx, fx.store(c), 20)
		return permReq{Method: "POST", Path: v1 + "/admin/refunds/" + no + "/receipt", OK: http.StatusOK}
	}},
	// 后台订单与退款单的列表 / 详情（00035）。契约 StaffRole 矩阵「订单与售后」那一行，
	// 与发货、审核同一个判据。列表对谁都是 200 —— 范围只收窄、不拒绝，
	// 「200 里装的是哪些单」由 admin_order_test.go 的 TestAdminOrderAndRefundListsAreScopedByRole
	// 逐角色比对单号；详情按订单的履约门店判，范围外 403 out-of-scope。
	{"GET", v1 + "/admin/orders", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/orders")
	}},
	{"GET", v1 + "/admin/orders/:order_no", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/orders/" + permPaidOrder(t, fx, fx.store(c)))
	}},
	{"GET", v1 + "/admin/refunds", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/refunds")
	}},
	{"GET", v1 + "/admin/refunds/:refund_no", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/refunds/" + permRefund(t, fx, fx.store(c), 10))
	}},
	// 后台读文件（售后链路补齐那一轮）。退款凭证按**引用它的退款单**判权，与后台退款单
	// 详情同一个判据；放行时是 302（跳限时地址），不是 200。商品图 / 头像对全体员工放行、
	// 没被引用的凭证一律 403 upload-forbidden，这两条由 upload_test.go 单独钉住。
	// 后台待办提醒（00053，数据模型 §16）。四条对谁都放行：列表与未读数只收窄、不拒绝；
	// 标已读在范围外回的是 404（看不见的不承认它存在）而不是 403，所以这一格只造在
	// 每个角色都管得着的 N1 上 —— 「范围外 404、每个人看到的是哪几条、已读各算各的」
	// 由 notification_test.go 的 TestAdminNotificationsAreScopedAndReadPerStaff 逐角色钉住。
	{"GET", v1 + "/admin/notifications", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/notifications")
	}},
	{"GET", v1 + "/admin/notifications/unread-count", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permGet(v1 + "/admin/notifications/unread-count")
	}},
	{"POST", v1 + "/admin/notifications/read-all", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: http.MethodPost, Path: v1 + "/admin/notifications/read-all", OK: http.StatusOK}
	}},
	{"POST", v1 + "/admin/notifications/:notification_id/read", everyone, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: http.MethodPost, OK: http.StatusOK,
			Path: fmt.Sprintf(v1+"/admin/notifications/%d/read", permNotification(t, fx, fx.N1))}
	}},
	{"GET", v1 + "/admin/uploads/:upload_id", storeOperate, func(t *testing.T, fx *permFixture, c permCase) permReq {
		return permReq{Method: http.MethodGet, OK: http.StatusFound,
			Path: fmt.Sprintf(v1+"/admin/uploads/%d", permEvidence(t, fx, fx.store(c)))}
	}},
}

// permEvidence 在 storeID 这家门店上造一张待审核退款单，给它挂一张退款凭证
// （purpose 3、上传者是退款单的买家），返回凭证的 upload id。直接插库，理由同 permRefund。
// 字节不落盘：矩阵只打第一跳（判权 → 302），不跟到第二跳。
func permEvidence(t *testing.T, fx *permFixture, storeID int64) int64 {
	t.Helper()
	refundNo := permRefund(t, fx, storeID, 10)
	userID := adminQueryInt64(t, `SELECT user_id FROM refunds WHERE refund_no = $1`, refundNo)
	uploadID := adminQueryInt64(t, `
		INSERT INTO uploads (merchant_id, user_id, purpose, driver, storage_key,
		                     content_type, size_bytes, sha256, referenced)
		VALUES ($1, $2, 3, 1, $3, 'image/png', 3, 'perm', TRUE) RETURNING id`,
		fx.sh.MerchantID, userID, fmt.Sprintf("%d/perm/evidence-%s.png", fx.sh.MerchantID, fx.next()))
	// 排在 permCleanupOrders 之后注册，于是先于它执行（t.Cleanup 后进先出）：
	// uploads.user_id 指向那个下单人，要先删凭证才删得掉人。删的是**这家店全部**矩阵凭证
	// 而不只是这一张：同一个子测试里几格各注册一对清理，后一格的 permCleanupOrders 会
	// 连前一格的下单人一起删，那时前一格的凭证必须已经没了。
	t.Cleanup(func() {
		adminExec(t, `DELETE FROM uploads WHERE merchant_id = $1 AND sha256 = 'perm'`, fx.sh.MerchantID)
	})
	adminExec(t, `UPDATE refunds SET evidence_urls = ARRAY[$2::text] WHERE refund_no = $1`,
		refundNo, fmt.Sprintf("/api/v1/uploads/%d", uploadID))
	return uploadID
}

// permNotification 在 storeID 这家门店上造一条商家通知（直接插库，理由同 permPaidOrder），返回 id。
func permNotification(t *testing.T, fx *permFixture, storeID int64) int64 {
	t.Helper()
	t.Cleanup(func() {
		adminExec(t, `DELETE FROM notifications WHERE merchant_id = $1 AND dedupe_key LIKE 'perm:%'`, fx.sh.MerchantID)
	})
	return adminQueryInt64(t, `
		INSERT INTO notifications (merchant_id, audience, store_id, kind, title, body,
		                           target_type, order_no, dedupe_key)
		VALUES ($1, 2, $2, 'merchant_order_paid', '新订单待发货', '权限矩阵', 'order', $3, $4)
		RETURNING id`, fx.sh.MerchantID, storeID, "PERM"+fx.next(), "perm:"+fx.next())
}

// permExempt 是刻意不在矩阵里的 /admin/ 路由，每条写明理由。
var permExempt = map[string]string{
	"POST " + v1 + "/admin/auth/bootstrap":  "未认证接口（契约 security: []），还没有身份，谈不上角色",
	"POST " + v1 + "/admin/auth/email-link": "未认证接口（契约 security: []），还没有身份，谈不上角色",
	"POST " + v1 + "/admin/auth/session":    "未认证接口（契约 security: []），还没有身份，谈不上角色",
}

// 两个方向都锁：路由表里的每一条 /admin/ 路由都在矩阵里（或写明了为什么不在），
// 矩阵里的每一行都真的注册了。
func TestEveryAdminRouteIsInThePermissionMatrix(t *testing.T) {
	registered := map[string]bool{}
	for _, ri := range testEngine.Routes() {
		if strings.HasPrefix(ri.Path, v1+"/admin/") {
			registered[ri.Method+" "+ri.Path] = true
		}
	}
	if len(registered) == 0 {
		t.Fatal("路由表里一条 /admin/ 路由都没有 —— 这条测试没在检查任何东西")
	}

	inMatrix := map[string]bool{}
	for _, r := range permMatrix {
		key := r.Method + " " + r.Path
		if inMatrix[key] {
			t.Errorf("权限矩阵里 %s 登记了两次", key)
		}
		inMatrix[key] = true
	}

	var missing []string
	for key := range registered {
		if inMatrix[key] {
			continue
		}
		if _, ok := permExempt[key]; ok {
			continue
		}
		missing = append(missing, key)
	}
	sort.Strings(missing)
	for _, key := range missing {
		t.Errorf("后台路由 %s 注册了，但权限矩阵（permission_test.go 的 permMatrix）里没有它 —— "+
			"先在表里写下「哪些角色能调、范围内外各是什么」，再去 internal/service/authz.go "+
			"给它挑一个判据。一条没人声明过权限的后台接口，默认就是全店员工都能调", key)
	}
	for key := range inMatrix {
		if !registered[key] {
			t.Errorf("权限矩阵里登记了 %s，但它没有被注册 —— 路径改了或接口删了，表烂了", key)
		}
	}
	for key := range permExempt {
		if !registered[key] {
			t.Errorf("permExempt 里挂着 %s，但它没有被注册 —— 请删掉这一行", key)
		}
		if inMatrix[key] {
			t.Errorf("%s 同时在矩阵与 permExempt 里", key)
		}
	}
	t.Logf("后台路由 %d 条：矩阵 %d 条 × %d 格，豁免 %d 条",
		len(registered), len(permMatrix), len(allPermRoles)*2, len(permExempt))
}

// TestAdminPermissionMatrix 逐格敲一遍。
func TestAdminPermissionMatrix(t *testing.T) {
	fx := newPermFixture(t)
	cells := 0
	for _, r := range permMatrix {
		r := r
		t.Run(r.Method+" "+strings.TrimPrefix(r.Path, v1), func(t *testing.T) {
			for _, role := range allPermRoles {
				for _, in := range []bool{true, false} {
					c := permCase{Role: role, In: in}
					cells++
					req := r.Build(t, fx, c)
					host, token := fx.sh.Host, fx.tokens[role]
					if req.Host != "" {
						host = req.Host
					}
					if req.Token != "" {
						token = req.Token
					}
					w := sendAs(t, host, req, token)
					want := r.Expect(c)
					typ, detail := problemTypeOf(w)
					if want == allow {
						if w.Code != req.OK {
							t.Errorf("[%s] %s %s：期望放行（%d），实际 %d %s %s",
								c, req.Method, req.Path, req.OK, w.Code, typ, detail)
						}
						continue
					}
					wantType := "https://keel.dev/problems/" + string(want)
					if w.Code != http.StatusForbidden || typ != wantType {
						t.Errorf("[%s] %s %s：期望 403 %s，实际 %d %s %s",
							c, req.Method, req.Path, want, w.Code, typ, strings.TrimSpace(w.Body.String()))
					}
				}
			}
		})
	}
	t.Logf("权限矩阵：%d 条路由，%d 格", len(permMatrix), cells)
}

// permCouponBody 是一张满 100 减 20、领取后 7 天有效的券。
func permCouponBody(fx *permFixture) string {
	return fmt.Sprintf(`{"name":"权限矩阵券%d","coupon_type":1,"threshold_cents":10000,"discount_cents":2000,`+
		`"valid_mode":2,"valid_days":7,"total_count":0,"per_user_limit":1}`, fx.seq.Add(1))
}

// permCouponTemplate 用商家管理员的令牌建一张新模板，返回 id。
// 每格一张：发出过券的模板券面与范围不能再改（409），共用一张会让后面的格子
// 因为前面格子的发放而失败，而那和权限毫无关系。
func permCouponTemplate(t *testing.T, fx *permFixture) int64 {
	t.Helper()
	permCleanupCoupons(t, fx)
	w := reqAs(t, http.MethodPost, fx.sh.Host, v1+"/admin/coupon-templates", permCouponBody(fx), fx.sh.Token)
	if w.Code != http.StatusCreated {
		t.Fatalf("夹具：建券模板失败 %d %s", w.Code, w.Body.String())
	}
	var tpl struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tpl); err != nil || tpl.ID == 0 {
		t.Fatalf("夹具：券模板响应解不出 id：%v %s", err, w.Body.String())
	}
	return tpl.ID
}

// permPromotionBody 是一个全店满 100 减 10 的活动（新建即下线）。
func permPromotionBody(fx *permFixture) string {
	return fmt.Sprintf(`{"name":"权限矩阵活动%d","promotion_type":1,"threshold_unit":1,`+
		`"starts_at":"2020-01-01T00:00:00Z","ends_at":"2099-01-01T00:00:00Z",`+
		`"tiers":[{"threshold":10000,"discount_cents":1000,"discount_rate":0}]}`, fx.seq.Add(1))
}

// permPromotion 用商家管理员的令牌建一个新活动，返回 id。
func permPromotion(t *testing.T, fx *permFixture) int64 {
	t.Helper()
	permCleanupPromotions(t, fx)
	w := reqAs(t, http.MethodPost, fx.sh.Host, v1+"/admin/promotions", permPromotionBody(fx), fx.sh.Token)
	if w.Code != http.StatusCreated {
		t.Fatalf("夹具：建活动失败 %d %s", w.Code, w.Body.String())
	}
	var p struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.ID == 0 {
		t.Fatalf("夹具：活动响应解不出 id：%v %s", err, w.Body.String())
	}
	return p.ID
}

// permCleanupPromotions 同 permCleanupCoupons：活动挂着指向 merchants 的外键，
// 要排在夹具清理之前删掉。
func permCleanupPromotions(t *testing.T, fx *permFixture) {
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM promotion_tiers WHERE merchant_id = $1`,
			`DELETE FROM promotion_scopes WHERE merchant_id = $1`,
			`DELETE FROM promotion_skus WHERE merchant_id = $1`,
			`DELETE FROM promotions WHERE merchant_id = $1`,
		} {
			adminExec(t, q, fx.sh.MerchantID)
		}
	})
}

// permBuyerPhone 在这家店建一个买家，返回手机号（定向发放按手机号找人）。
func permBuyerPhone(t *testing.T, fx *permFixture) string {
	t.Helper()
	permCleanupCoupons(t, fx)
	phone := fmt.Sprintf("137%08d", fx.seq.Add(1)%100_000_000)
	adminExec(t, `INSERT INTO users (merchant_id, phone, nickname) VALUES ($1, $2, '权限矩阵买家')`,
		fx.sh.MerchantID, phone)
	return phone
}

// permCleanupCoupons 在这一格结束时删掉券与买家。
//
// 夹具本身的清理（newAdminShop 注册的那个）最后要删 merchants 行，而券模板、
// 发出去的券、建出来的买家都挂着指向它的外键 —— 不先删它们，那一步会报 23503，
// 而报错指向的是一个与权限毫无关系的地方。子测试的 Cleanup 先于父测试的执行，
// 所以挂在这里正好排在夹具清理之前。DELETE 可重复执行，每格都挂一次没有代价。
func permCleanupCoupons(t *testing.T, fx *permFixture) {
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM user_coupons WHERE merchant_id = $1`,
			`DELETE FROM coupon_scopes WHERE merchant_id = $1`,
			`DELETE FROM coupon_templates WHERE merchant_id = $1`,
			`DELETE FROM users WHERE merchant_id = $1 AND nickname = '权限矩阵买家'`,
		} {
			adminExec(t, q, fx.sh.MerchantID)
		}
	})
}

// permPaidOrder 在 storeID 这家门店上造一笔**已支付**的订单（直接插库），返回单号。
//
// 走管理员连接直接插，而不是下单 + 沙箱支付：矩阵关心的是「谁能对这一单做什么」，
// 不是下单链路；而一格一笔真实下单要带上库存、地址、SAGA，二十几格下来
// 慢且与权限毫无关系。状态 20 要带 paid_at（00033 的 chk_fulfillment_timestamps）。
func permPaidOrder(t *testing.T, fx *permFixture, storeID int64) string {
	t.Helper()
	permCleanupOrders(t, fx)
	no := "PERM" + fx.next()
	uid := adminQueryInt64(t, `INSERT INTO users (merchant_id, phone, nickname)
	                           VALUES ($1, $2, '权限矩阵下单人') RETURNING id`,
		fx.sh.MerchantID, fmt.Sprintf("136%08d", fx.seq.Add(1)%100_000_000))
	adminExec(t, `
		INSERT INTO orders (merchant_id, order_no, user_id, status, goods_amount_cents, payable_cents,
		                    paid_cents, paid_at, receiver_snapshot, expire_at,
		                    store_id, region_id, store_snapshot)
		SELECT $1, $2, $3, 20, 1000, 1000, 1000, now(), '{}'::jsonb, now() + interval '30 minutes',
		       st.id, st.region_id, '{}'::jsonb
		  FROM stores st WHERE st.id = $4`, fx.sh.MerchantID, no, uid, storeID)
	return no
}

// permRefund 在 storeID 这家门店上造一笔已支付订单（带一行订单项、一笔成功支付）
// 和挂在它上面的一张退款单，退款单停在 status（10 待审核 或 20 待买家退货），返回退款单号。
//
// 同样直接插库，理由同 permPaidOrder。20 要带 audited_at（00034 的 chk_refund_state），
// 而且是退货退款（只有退货退款会停在 20）。
func permRefund(t *testing.T, fx *permFixture, storeID int64, status int) string {
	t.Helper()
	orderNo := permPaidOrder(t, fx, storeID)
	orderID := adminQueryInt64(t, `SELECT id FROM orders WHERE order_no = $1`, orderNo)
	userID := adminQueryInt64(t, `SELECT user_id FROM orders WHERE order_no = $1`, orderNo)
	itemID := adminQueryInt64(t, `
		INSERT INTO order_items (merchant_id, order_id, sku_id, product_id, title_snapshot,
		                         spec_snapshot, price_cents, list_price_cents, quantity, amount_cents)
		VALUES ($1, $2, $3, $4, '权限矩阵商品', '{}'::jsonb, 1000, 1000, 1, 1000) RETURNING id`,
		fx.sh.MerchantID, orderID, fx.SKUID, fx.ProductID)
	paymentID := adminQueryInt64(t, `
		INSERT INTO payments (merchant_id, payment_no, order_id, channel, amount_cents, status,
		                      channel_txn_id, paid_at)
		VALUES ($1, $2, $3, 1, 1000, 1, $2, now()) RETURNING id`,
		fx.sh.MerchantID, "PERMPAY"+fx.next(), orderID)
	refundNo := "PERMRF" + fx.next()
	refundType, audited := 1, "NULL"
	if status == 20 {
		refundType, audited = 2, "now()"
	}
	refundID := adminQueryInt64(t, `
		INSERT INTO refunds (merchant_id, refund_no, order_id, payment_id, user_id, refund_type,
		                     reason_code, goods_amount_cents, amount_cents, status, channel, audited_at)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 1000, 1000, $7, 1, `+audited+`) RETURNING id`,
		fx.sh.MerchantID, refundNo, orderID, paymentID, userID, refundType, status)
	adminExec(t, `INSERT INTO refund_items (merchant_id, refund_id, order_item_id, quantity, amount_cents)
	              VALUES ($1, $2, $3, 1, 1000)`, fx.sh.MerchantID, refundID, itemID)
	adminExec(t, `UPDATE orders SET refund_status = 1 WHERE id = $1`, orderID)
	return refundNo
}

// permCleanupOrders 在这一格结束时删掉矩阵造出来的订单与它们的下游行，
// 理由与 permCleanupCoupons 一样：夹具最后要删 merchants 行，而这些行挂着指向它的外键。
func permCleanupOrders(t *testing.T, fx *permFixture) {
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM shipments WHERE merchant_id = $1`,
			`DELETE FROM refund_items WHERE merchant_id = $1`,
			`DELETE FROM refunds WHERE merchant_id = $1`,
			`DELETE FROM payments WHERE merchant_id = $1`,
			`DELETE FROM order_items WHERE merchant_id = $1`,
			`DELETE FROM orders WHERE merchant_id = $1`,
			`DELETE FROM users WHERE merchant_id = $1 AND nickname = '权限矩阵下单人'`,
		} {
			adminExec(t, q, fx.sh.MerchantID)
		}
	})
}
