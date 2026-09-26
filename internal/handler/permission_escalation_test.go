package handler_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 防提权：每一条单独一条测试（internal/service/authz.go 的 authorizeStaffWrite）。
//
// 矩阵（permission_test.go）已经把「谁能调 POST / PATCH /admin/staff」敲过一遍，
// 但它每一格只发一种请求体。提权的花样全在请求体里 —— 同一个大区管理员、
// 同一条接口，建门店管理员是对的，建管理员就是提权。所以这几条逐一把
// 「对的那一种」与「提权的那一种」并排放着：先打对的（阳性对照，证明这个人
// 确实有权做这类事、前提确实建起来了），再打提权的，断言 403 与 type。
//
// 每一条都做过变异（把对应的那一段判据改成放行），红的是本条的断言，
// 不是别处先炸 —— 变异记录在提交信息里。

// wantForbidden 断言 403 且 type 是指定的那一个，返回 detail。
func wantForbidden(t *testing.T, w *httptest.ResponseRecorder, want denyType, what string) {
	t.Helper()
	typ, detail := problemTypeOf(w)
	if w.Code != http.StatusForbidden || typ != "https://keel.dev/problems/"+string(want) {
		t.Fatalf("%s：期望 403 %s，实际 %d %s %s —— 这是一次提权",
			what, want, w.Code, typ, w.Body.String())
	}
	if detail == "" && (want == roleForbidden || want == outOfScope) {
		t.Errorf("%s：403 的 Problem 没有 detail —— 界面要把服务端的原话显示给被拒的人", what)
	}
}

func patchStaff(t *testing.T, fx *permFixture, token string, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	return patchAs(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/staff/%d", id), body, token)
}

// 大区管理员建不出管理员、操作员，也建不出和自己平级的大区管理员；
// 也不能把手下的门店管理员改成这几种角色。
func TestRegionManagerCannotCreateMerchantAdminOrOperator(t *testing.T) {
	fx := newPermFixture(t)
	tok := fx.tokens[roleRegion]

	// 阳性对照：他确实能建门店管理员（本大区的店）。
	fx.createStaff(t, tok, 4, nil, []int64{fx.N1})

	for _, c := range []struct {
		what string
		body string
	}{
		{"建商家管理员", staffBody("esc-a"+fx.next()+"@keel.test", 1, nil, nil)},
		{"建操作员", staffBody("esc-o"+fx.next()+"@keel.test", 2, nil, nil)},
		{"建大区管理员（本大区）", staffBody("esc-r"+fx.next()+"@keel.test", 3, []int64{fx.North}, nil)},
	} {
		wantForbidden(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff", c.body, tok),
			roleForbidden, "大区管理员"+c.what)
	}

	// 手下的门店管理员：状态能改（阳性对照），角色改不成 1 / 2。
	mine := fx.createStaff(t, tok, 4, nil, []int64{fx.N2})
	wantStatus(t, patchStaff(t, fx, tok, mine, `{"status":2}`), http.StatusOK, "大区管理员停用手下的店长")
	wantForbidden(t, patchStaff(t, fx, tok, mine, `{"role":1}`), roleForbidden, "大区管理员把店长提成管理员")
	wantForbidden(t, patchStaff(t, fx, tok, mine, `{"role":2}`), roleForbidden, "大区管理员把店长提成操作员")

	if n := adminQueryInt64(t, `SELECT count(*) FROM staff WHERE merchant_id = $1 AND role IN (1, 2)
		AND email LIKE 'esc-%'`, fx.sh.MerchantID); n != 0 {
		t.Fatalf("库里多出了 %d 个由大区管理员建出来的管理员 / 操作员", n)
	}
}

// 大区管理员给门店管理员分配的每一家店都必须在他的大区里 ——
// 建的时候、改的时候都一样；目标本人管着别的大区的店时也改不了。
func TestRegionManagerCannotAssignStoresOutsideTheirRegion(t *testing.T) {
	fx := newPermFixture(t)
	tok := fx.tokens[roleRegion]

	// 阳性对照：本大区的两家店一起给，能建出来。
	mine := fx.createStaff(t, tok, 4, nil, []int64{fx.N1, fx.N2})

	wantForbidden(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff",
		staffBody("esc-e"+fx.next()+"@keel.test", 4, nil, []int64{fx.E1}), tok),
		outOfScope, "大区管理员建一个管华东门店的店长")
	wantForbidden(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff",
		staffBody("esc-ne"+fx.next()+"@keel.test", 4, nil, []int64{fx.N1, fx.E1}), tok),
		outOfScope, "大区管理员建一个一半华北一半华东的店长")

	// 改：阳性对照是换成本大区的另一家店。
	wantStatus(t, patchStaff(t, fx, tok, mine, fmt.Sprintf(`{"store_ids":[%d]}`, fx.N2)),
		http.StatusOK, "大区管理员把店长换到本大区另一家店")
	wantForbidden(t, patchStaff(t, fx, tok, mine, fmt.Sprintf(`{"store_ids":[%d,%d]}`, fx.N2, fx.E1)),
		outOfScope, "大区管理员给店长加一家华东的店")

	// 目标本人管着华东的店（由商家管理员建的）：他不归华北的大区管理员管。
	theirs := fx.createStaff(t, fx.sh.Token, 4, nil, []int64{fx.E1})
	wantForbidden(t, patchStaff(t, fx, tok, theirs, fmt.Sprintf(`{"store_ids":[%d]}`, fx.N1)),
		outOfScope, "大区管理员把华东的店长拉到华北")

	var st api.Staff
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/me", staffSession(t, fx.sh.Host, mine).Token),
		http.StatusOK, "店长读自己", &st)
	if len(st.StoreIds) != 1 || st.StoreIds[0] != fx.N2 {
		t.Fatalf("被拒的那次改动之后店长管的是 %v，期望还是 [%d]", st.StoreIds, fx.N2)
	}
}

// 大区管理员不能扩大自己的范围：改自己不行，建一个新大区（建出来就是他的
// 「下一块地」的前奏）也不行。
func TestRegionManagerCannotWidenTheirOwnScope(t *testing.T) {
	fx := newPermFixture(t)
	tok := fx.tokens[roleRegion]

	// 阳性对照：他对华北有权（能改华北的名字）。
	wantStatus(t, patchAs(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/regions/%d", fx.North),
		`{"name":"华北"}`, tok), http.StatusOK, "大区管理员改自己的大区")

	wantForbidden(t, patchStaff(t, fx, tok, fx.RegionMgrID,
		fmt.Sprintf(`{"region_ids":[%d,%d]}`, fx.North, fx.East)),
		roleForbidden, "大区管理员给自己加华东")
	wantForbidden(t, reqAs(t, http.MethodPost, fx.sh.Host, "/api/v1/admin/regions",
		fmt.Sprintf(`{"code":"grab-%s","name":"自己建的大区"}`, fx.next()), tok),
		roleForbidden, "大区管理员建新大区")

	// 被拒之后他对华东仍然无权（范围真的没变 —— 下一个请求从库里重读）。
	wantForbidden(t, patchAs(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/regions/%d", fx.East),
		`{"name":"华东"}`, tok), outOfScope, "被拒之后大区管理员改华东")
}

// 门店管理员不能建员工，也不能改任何员工（包括自己）。
func TestStoreManagerCannotCreateStaff(t *testing.T) {
	fx := newPermFixture(t)
	tok := fx.tokens[roleStore]

	// 阳性对照：同一个请求体，商家管理员建得出来 —— 请求体本身是对的。
	body := func() string { return staffBody("esc-s"+fx.next()+"@keel.test", 4, nil, []int64{fx.N1}) }
	wantStatus(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff", body(), fx.sh.Token),
		http.StatusCreated, "商家管理员建店长")

	wantForbidden(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff", body(), tok),
		staffForbidden, "门店管理员建店长")
	wantForbidden(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff",
		staffBody("esc-sa"+fx.next()+"@keel.test", 1, nil, nil), tok),
		staffForbidden, "门店管理员建管理员")
	wantForbidden(t, patchStaff(t, fx, tok, fx.StoreMgrID, fmt.Sprintf(`{"store_ids":[%d,%d]}`, fx.N1, fx.N2)),
		staffForbidden, "门店管理员给自己加一家店")
}

// 任何人都不能改自己的角色（契约 PATCH /admin/staff/{staff_id}）。
func TestNobodyCanChangeTheirOwnRole(t *testing.T) {
	fx := newPermFixture(t)

	// 商家管理员：店里还有第二个管理员，所以「最后一个管理员」那条 409 拦不住，
	// 拦住它的只能是「不能改自己」这一条。
	second := fx.createStaff(t, fx.sh.Token, 1, nil, nil)
	wantForbidden(t, patchStaff(t, fx, fx.sh.Token, fx.sh.StaffID, `{"role":2}`),
		roleForbidden, "商家管理员把自己降成操作员")
	wantForbidden(t, patchStaff(t, fx, fx.sh.Token, fx.sh.StaffID,
		fmt.Sprintf(`{"role":3,"region_ids":[%d]}`, fx.North)),
		roleForbidden, "商家管理员把自己改成大区管理员")

	// 大区管理员把自己改成门店管理员（换一种身份拿到 N1 之外的店）。
	wantForbidden(t, patchStaff(t, fx, fx.tokens[roleRegion], fx.RegionMgrID,
		fmt.Sprintf(`{"role":4,"store_ids":[%d]}`, fx.N1)),
		roleForbidden, "大区管理员把自己改成门店管理员")

	// 操作员、门店管理员本来就不能管员工，改自己也一样。
	wantForbidden(t, patchStaff(t, fx, fx.tokens[roleOperator], fx.OperatorID, `{"role":1}`),
		staffForbidden, "操作员把自己提成管理员")
	wantForbidden(t, patchStaff(t, fx, fx.tokens[roleStore], fx.StoreMgrID, `{"role":1}`),
		staffForbidden, "门店管理员把自己提成管理员")

	// 阳性对照：同一个改动由**另一个**管理员来做就通过 —— 拒绝的理由是
	// 「改的是自己」，不是「这个改动本身不合法」。
	secondTok := staffSession(t, fx.sh.Host, second).Token
	st := staffOf(t, patchStaff(t, fx, secondTok, fx.sh.StaffID, `{"role":2}`), http.StatusOK)
	if st.Role != 2 {
		t.Fatalf("另一个管理员降级之后 role 是 %d，期望 2", st.Role)
	}
	// 原样带回自己的角色（界面整张表单提交的形状）不算「改」。
	st = staffOf(t, patchStaff(t, fx, secondTok, second, `{"role":1,"status":1}`), http.StatusOK)
	if st.Role != 1 {
		t.Fatalf("原样提交自己的角色之后 role 是 %d，期望 1", st.Role)
	}
}

// 范围每个请求从库里重读：收回一个大区之后，同一串会话的下一个请求就失去它。
func TestStaffScopeIsReloadedOnEveryRequest(t *testing.T) {
	fx := newPermFixture(t)
	tok := fx.tokens[roleRegion]
	price := func(store int64) *httptest.ResponseRecorder {
		return putAs(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", store, fx.SKUID),
			`{"price_cents":700}`, tok)
	}

	wantStatus(t, price(fx.N1), http.StatusOK, "大区管理员改华北门店的价")
	wantForbidden(t, price(fx.E1), outOfScope, "大区管理员改华东门店的价")

	// 商家管理员把他从华北调到华东。
	wantStatus(t, patchStaff(t, fx, fx.sh.Token, fx.RegionMgrID, fmt.Sprintf(`{"region_ids":[%d]}`, fx.East)),
		http.StatusOK, "把大区管理员调到华东")

	wantForbidden(t, price(fx.N1), outOfScope, "调走之后同一串会话改华北门店的价")
	wantStatus(t, price(fx.E1), http.StatusOK, "调走之后同一串会话改华东门店的价")

	// 降成操作员：范围行被清空，角色变成全店范围。
	st := staffOf(t, patchStaff(t, fx, fx.sh.Token, fx.RegionMgrID, `{"role":2}`), http.StatusOK)
	if len(st.RegionIds) != 0 || len(st.StoreIds) != 0 {
		t.Fatalf("降成操作员之后范围是 %v / %v，期望都清空", st.RegionIds, st.StoreIds)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM staff_scopes WHERE staff_id = $1`, fx.RegionMgrID); n != 0 {
		t.Fatalf("降成操作员之后 staff_scopes 里还有 %d 行", n)
	}
}

// 列表类接口对大区 / 门店管理员只返回范围内的。
func TestScopedListsOnlyShowWhatYouManage(t *testing.T) {
	fx := newPermFixture(t)

	storeIDs := func(tok string) []int64 {
		var page struct {
			Items []api.AdminStore `json:"items"`
		}
		decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/stores?page_size=100", tok), http.StatusOK, "列门店", &page)
		out := []int64{}
		for _, s := range page.Items {
			out = append(out, s.Id)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	regionIDs := func(tok string) []int64 {
		var page struct {
			Items []api.AdminRegion `json:"items"`
		}
		decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/regions?page_size=100", tok), http.StatusOK, "列大区", &page)
		out := []int64{}
		for _, r := range page.Items {
			out = append(out, r.Id)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	staffIDs := func(tok string) []int64 {
		var page struct {
			Items []api.Staff `json:"items"`
		}
		decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/staff?page_size=100", tok), http.StatusOK, "列员工", &page)
		out := []int64{}
		for _, s := range page.Items {
			out = append(out, s.Id)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eq := func(what string, got, want []int64) {
		t.Helper()
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s：得到 %v，期望 %v", what, got, want)
		}
	}

	// 阳性对照：商家管理员看得见全部四家店、三个大区。
	eq("商家管理员的门店列表", storeIDs(fx.sh.Token), []int64{fx.sh.StoreID, fx.N1, fx.N2, fx.E1})
	if got := regionIDs(fx.sh.Token); len(got) != 3 {
		t.Errorf("商家管理员的大区列表有 %d 个，期望 3", len(got))
	}

	eq("大区管理员的门店列表", storeIDs(fx.tokens[roleRegion]), []int64{fx.N1, fx.N2})
	eq("大区管理员的大区列表", regionIDs(fx.tokens[roleRegion]), []int64{fx.North})
	eq("门店管理员的门店列表", storeIDs(fx.tokens[roleStore]), []int64{fx.N1})
	eq("门店管理员的大区列表", regionIDs(fx.tokens[roleStore]), []int64{fx.North})

	// 员工列表：大区管理员看见自己 + 只管华北门店的店长；
	// 一个一半华北一半华东的店长不在里面（他的另一半归别人管）。
	mixed := fx.createStaff(t, fx.sh.Token, 4, nil, []int64{fx.N2, fx.E1})
	eastOnly := fx.createStaff(t, fx.sh.Token, 4, nil, []int64{fx.E1})
	_, _ = mixed, eastOnly
	eq("大区管理员的员工列表", staffIDs(fx.tokens[roleRegion]), []int64{fx.RegionMgrID, fx.StoreMgrID})
	eq("门店管理员的员工列表", staffIDs(fx.tokens[roleStore]), []int64{fx.StoreMgrID})
}

// 角色与范围必须配套（契约 POST /admin/staff），不配套是 422 而不是 500。
func TestStaffRoleAndScopesMustMatch(t *testing.T) {
	fx := newPermFixture(t)
	for _, c := range []struct {
		what string
		body string
	}{
		{"大区管理员不带大区", staffBody("bad1"+fx.next()+"@keel.test", 3, nil, nil)},
		{"大区管理员带了门店", staffBody("bad2"+fx.next()+"@keel.test", 3, []int64{fx.North}, []int64{fx.N1})},
		{"门店管理员不带门店", staffBody("bad3"+fx.next()+"@keel.test", 4, nil, nil)},
		{"操作员带了大区", staffBody("bad4"+fx.next()+"@keel.test", 2, []int64{fx.North}, nil)},
		{"引用不存在的门店", staffBody("bad5"+fx.next()+"@keel.test", 4, nil, []int64{999999999})},
		{"角色不在枚举里", staffBody("bad6"+fx.next()+"@keel.test", 5, nil, nil)},
	} {
		wantStatus(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff", c.body, fx.sh.Token),
			http.StatusUnprocessableEntity, c.what)
	}

	// 阳性对照 + 响应形状：带范围建出来，响应里原样带回（升序、去重）。
	var st api.Staff
	decodeInto(t, postIdem(t, fx.sh.Host, "/api/v1/admin/staff",
		staffBody("ok"+fx.next()+"@keel.test", 4, nil, []int64{fx.N2, fx.N1, fx.N2}), fx.sh.Token),
		http.StatusCreated, "建一个管两家店的店长", &st)
	want := []int64{fx.N1, fx.N2}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if fmt.Sprint(st.StoreIds) != fmt.Sprint(want) || len(st.RegionIds) != 0 {
		t.Fatalf("响应里的范围是 region=%v store=%v，期望 store=%v", st.RegionIds, st.StoreIds, want)
	}
}
