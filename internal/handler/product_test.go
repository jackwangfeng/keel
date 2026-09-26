package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// listResp 只声明断言要用到的字段，但 total / page / page_size 一个都不能少：
// 它们是契约 200 响应里 PageMeta 的必填字段。
type listResp struct {
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Total    int64      `json:"total"`
	Items    []respItem `json:"items"`
}

type respItem struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	MinPriceCents int64  `json:"min_price_cents"`
	Status        int16  `json:"status"`
}

// 种子里两家店的商品件数。刻意不同 —— 相同的话，「两边都返回 N 件」
// 区分不开「各看各的」和「都看到了全部」。
const (
	wantA = 3
	wantB = 2
)

func get(t *testing.T, host, path string) (*httptest.ResponseRecorder, listResp) {
	t.Helper()
	w := do(t, host, path)
	var body listResp
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是预期结构: %v\n%s", err, w.Body.String())
		}
	}
	return w, body
}

// 两个租户各自只看得到自己的商品：件数对得上，且 id 集合无交集。
//
// 这一条是整套多租户设计唯一真正要命的地方，而它在单租户测试数据下完全看不
// 出来——加不加 WHERE merchant_id 结果一模一样。
//
// 断言具体条数而不是「非空」「无交集」：把 RLS 策略删掉之后，两家店都会看到
// 全部 5 件，此时 id 集合**确实有交集**，所以无交集那条也会红；但如果哪天
// 两家店的商品变成一样多，只剩无交集这一条时，「两边各返回一半」这种半失效
// 就漏过去了。件数 + 无交集 + 标题归属，三条各挡一类失效。
func TestTenantsSeeOnlyTheirOwnProducts(t *testing.T) {
	wa, a := get(t, "shop-a."+baseDomain, "/api/v1/products")
	wb, b := get(t, "shop-b."+baseDomain, "/api/v1/products")
	if wa.Code != http.StatusOK || wb.Code != http.StatusOK {
		t.Fatalf("状态码 a=%d b=%d\na=%s\nb=%s",
			wa.Code, wb.Code, wa.Body.String(), wb.Body.String())
	}

	if len(a.Items) != wantA || len(b.Items) != wantB {
		t.Fatalf("件数不对：shop-a 得 %d 件（期望 %d），shop-b 得 %d 件（期望 %d）。"+
			"两边都变成 %d 件的话，就是租户过滤整个失效了；"+
			"件数比种子多则是种子不幂等（重复加载在累积重复行）",
			len(a.Items), wantA, len(b.Items), wantB, wantA+wantB)
	}

	// total 是契约里客户端拿来算总页数的那个数，它同样必须是**本租户的**总数。
	// 只断言 items 的话，一个「列表走 RLS、计数不走」的实现能安静地混过去，
	// 症状是客户端一直翻到一页空的。
	if a.Total != wantA || b.Total != wantB {
		t.Fatalf("total 不对：shop-a=%d（期望 %d），shop-b=%d（期望 %d）",
			a.Total, wantA, b.Total, wantB)
	}

	seen := map[int64]bool{}
	for _, it := range a.Items {
		seen[it.ID] = true
	}
	for _, it := range b.Items {
		if seen[it.ID] {
			t.Fatalf("商品 %d 同时出现在两个租户的结果里 —— RLS 没生效", it.ID)
		}
	}

	// 标题里带着店铺 code。它挡的是「件数对、id 也不重叠，但两边拿错了对方那批」
	// 这种交叉——上面两条对它都是绿的。
	for _, tc := range []struct {
		code  string
		items []respItem
	}{{"shop-a", a.Items}, {"shop-b", b.Items}} {
		for _, it := range tc.items {
			if !strings.HasPrefix(it.Title, tc.code+" ") {
				t.Fatalf("%s 的结果里出现了 %q —— 它不属于这家店", tc.code, it.Title)
			}
		}
	}
}

// 分页参数越界必须被钳制，而且钳制的结果要能从响应里看出来。
//
// 只断言「状态码是 200 或 400」是测不出钳制的：不钳制也一样返回 200。
// 断言回显的 page / page_size 是钳制后的值，删掉 service 里的钳制就会红。
func TestPaginationIsClamped(t *testing.T) {
	for _, tc := range []struct {
		query          string
		page, pageSize int
	}{
		{"", 1, 20},
		{"?page=0", 1, 20},
		{"?page=-1", 1, 20},
		{"?page_size=0", 1, 20},
		{"?page_size=-5", 1, 20},
		{"?page_size=100000", 1, 100},
		{"?page_size=101", 1, 100},
		{"?page=abc", 1, 20},
		{"?page=2&page_size=2", 2, 2},
	} {
		w, body := get(t, "shop-a."+baseDomain, "/api/v1/products"+tc.query)
		if w.Code != http.StatusOK {
			t.Fatalf("%q 期望 200，实得 %d：%s", tc.query, w.Code, w.Body.String())
		}
		if body.Page != tc.page || body.PageSize != tc.pageSize {
			t.Fatalf("%q 回显 page=%d page_size=%d，期望 %d / %d —— "+
				"越界的分页参数没有被钳制，它们会原样进 SQL",
				tc.query, body.Page, body.PageSize, tc.page, tc.pageSize)
		}
		if len(body.Items) > tc.pageSize {
			t.Fatalf("%q 返回了 %d 件，超过 page_size=%d", tc.query, len(body.Items), tc.pageSize)
		}
	}
}

// 巨大的页码不能变成一个负的 OFFSET。
//
// (page-1)*pageSize 在 int32 里会绕回负数，而负的 OFFSET 让 Postgres 报错 ——
// 一个 500，错误信息里只字不提「页码太大」。sqlc 生成的参数正是 int32。
func TestHugePageDoesNotOverflow(t *testing.T) {
	for _, q := range []string{"?page=999999999", "?page=9223372036854775807"} {
		w, body := get(t, "shop-a."+baseDomain, "/api/v1/products"+q)
		if w.Code != http.StatusOK {
			t.Fatalf("%q 期望 200，实得 %d：%s", q, w.Code, w.Body.String())
		}
		if len(body.Items) != 0 {
			t.Fatalf("%q 期望空页，实得 %d 件", q, len(body.Items))
		}
	}
}

// 分页真的在翻页：两页的 id 不重复，合起来正好是全部。
//
// 没有这条的话，一个把 OFFSET 恒当成 0 的实现在上面所有断言下都是绿的。
func TestPagingWalksThroughTheWholeList(t *testing.T) {
	var ids []int64
	for page := 1; page <= wantA; page++ {
		_, body := get(t, "shop-a."+baseDomain,
			"/api/v1/products?page_size=1&page="+strconv.Itoa(page))
		if len(body.Items) != 1 {
			t.Fatalf("第 %d 页期望 1 件，实得 %d 件", page, len(body.Items))
		}
		// total 是**全部**的条数，不是本页的条数。
		//
		// 这条断言必须在 page_size=1 上做：默认 page_size=20 而种子只有 3 件时
		// len(items) 恰好等于 total，把 handler 改成 Total: len(items) 全部测试
		// 照样绿——实测过。db/queries/products.sql 花一整段论证 total 必须由
		// 数据库 COUNT(*) OVER() 给出而不是应用层现编，那段论证在默认页长下
		// 没有任何东西守着。
		if body.Total != wantA {
			t.Fatalf("第 %d 页回显 total=%d，期望 %d —— total 是全部条数，"+
				"不是本页条数（page_size=1 时两者必然不同）", page, body.Total, wantA)
		}
		ids = append(ids, body.Items[0].ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			t.Fatalf("翻页翻出了重复的商品 %d —— OFFSET 可能没生效：%v", ids[i], ids)
		}
	}

	_, past := get(t, "shop-a."+baseDomain, "/api/v1/products?page_size=1&page="+strconv.Itoa(wantA+1))
	if len(past.Items) != 0 {
		t.Fatalf("越过最后一页还有 %d 件", len(past.Items))
	}
	// 越界页仍要回显真实的 total —— 前端靠它画页码。这里 items 为空，
	// 所以 Total: len(items) 那种实现会给 0。
	if past.Total != wantA {
		t.Fatalf("越过最后一页回显 total=%d，期望 %d", past.Total, wantA)
	}
}

// healthz 在租户中间件之外：它回答「进程还活着吗」，不该因为 Host 没配对而变红。
func TestHealthzIgnoresHost(t *testing.T) {
	w := do(t, "whatever.invalid", "/healthz")
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("healthz: 状态 %d，正文 %q", w.Code, w.Body.String())
	}
}

// 响应的形状必须是契约里的 PageMeta + items，而不是 {code,message,data} 信封。
func TestResponseShapeMatchesContract(t *testing.T) {
	w := do(t, "shop-a."+baseDomain, "/api/v1/products")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v\n%s", err, w.Body.String())
	}
	// store 是 00020 加的第五个必填顶层字段：本次结果按哪家门店算的。
	// 它**必返**，而不是「有门店时才有」—— 契约把 match_type = none
	// （不在服务范围）也定义成一个正常结果，缺席会让客户端无法分辨
	// 「这家店什么都不卖」与「我们不送到你那儿」。
	for _, k := range []string{"page", "page_size", "total", "items", "store"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("响应缺少契约里的必填字段 %q：%s", k, w.Body.String())
		}
	}
	if len(raw) != 5 {
		t.Fatalf("响应多出了契约里没有的顶层字段：%s", w.Body.String())
	}

	// store 里 match_type 必返；store_id / region_id 在 none 那一支缺席。
	var store map[string]json.RawMessage
	if err := json.Unmarshal(raw["store"], &store); err != nil {
		t.Fatal(err)
	}
	if _, ok := store["match_type"]; !ok {
		t.Fatalf("store 里没有 match_type：%s", raw["store"])
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw["items"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("种子数据不足，无法检查 items 的形状")
	}
	// ProductSummary 的 required：id / title / min_price_cents / status。
	// status 尤其不能带 omitempty —— 0 在契约里是「草稿」这个有意义的取值。
	for _, k := range []string{"id", "title", "min_price_cents", "status"} {
		if _, ok := items[0][k]; !ok {
			t.Fatalf("items[0] 缺少契约里的必填字段 %q：%s", k, raw["items"])
		}
	}
}

// 草稿（status = 0）与软删（deleted_at 非空）的商品既不出现在列表里，
// 也不计进 total。
//
// 这条测试守的是 ListProducts 与 CountProducts 里那两个谓词。没有它 ——
// 更准确地说，没有种子里那两件反例商品 —— 把 `AND status = 1` 从 CountProducts
// 里删掉，全部测试照样绿：在架商品的 status 恒为 1，那个谓词永远筛不掉任何东西，
// 于是它可被删除而无症状。`db/queries/products.sql` 的注释写着「条件必须与
// ListProducts 逐字一致」，这条测试是那句话唯一的执行者。
func TestDraftAndDeletedProductsAreInvisible(t *testing.T) {
	visible, all := rawProductCount(t, "shop-a")

	// 阳性对照，必须排在断言前面：库里没有不可见的行时，下面两条断言在
	// 「谓词被删掉」和「谓词还在」两种情况下的结果一模一样。
	if all <= visible {
		t.Fatalf("shop-a 在库里有 %d 行，其中 %d 行可见 —— 种子里没有草稿或软删的"+
			"商品，这条测试证明不了任何事。db/seed/dev.sql 的反例商品还在吗？",
			all, visible)
	}
	if visible != wantA {
		t.Fatalf("shop-a 可见商品 %d 件，期望 %d 件", visible, wantA)
	}

	_, body := get(t, "shop-a."+baseDomain, "/api/v1/products?page_size=100")
	if len(body.Items) != visible {
		t.Fatalf("接口返回 %d 件，库里可见的只有 %d 件（全部 %d 行）—— "+
			"草稿或软删的商品漏出来了", len(body.Items), visible, all)
	}
	if body.Total != int64(visible) {
		t.Fatalf("total=%d，库里可见的只有 %d 件（全部 %d 行）—— "+
			"CountProducts 的过滤条件和 ListProducts 对不上了",
			body.Total, visible, all)
	}
	for _, it := range body.Items {
		if strings.Contains(it.Title, "草稿") || strings.Contains(it.Title, "已删") {
			t.Fatalf("不该露面的商品出现在列表里：%q", it.Title)
		}
	}
}

// 解析不到商家时的 404 必须带 RFC 9457 的响应体。
//
// 契约里 /products 的响应集合只有 200 和 default（Problem）。一个
// Content-Length: 0 的 404 不在这个集合里 —— 按契约生成的客户端会拿到一个
// 解析不出来的响应，而 404 恰恰是它最需要读懂的那个（「这家店不存在」
// 和「服务挂了」得分得开）。
func TestUnknownHostGetsProblemJSON(t *testing.T) {
	for _, host := range []string{"nobody." + baseDomain, "shop-a.attacker.example.org"} {
		w := do(t, host, "/api/v1/products")
		if w.Code != http.StatusNotFound {
			t.Fatalf("Host %q 期望 404，实得 %d：%s", host, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
			t.Fatalf("Host %q 的 404 Content-Type 是 %q，契约要求 application/problem+json",
				host, ct)
		}
		var p struct {
			Type   string `json:"type"`
			Title  string `json:"title"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("Host %q 的 404 响应体不是 Problem: %v（%q）",
				host, err, w.Body.String())
		}
		// Problem 的 required 是 type / title / status，一个都不能缺。
		if p.Type == "" || p.Title == "" || p.Status != http.StatusNotFound {
			t.Fatalf("Host %q 的 Problem 不完整：type=%q title=%q status=%d",
				host, p.Type, p.Title, p.Status)
		}
	}
}

// assertProblem 断言一个响应是契约里的 Problem：Content-Type 对，
// 三个必填字段齐，status 与 HTTP 状态码一致。
func assertProblem(t *testing.T, w *httptest.ResponseRecorder, want int, what string) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("%s 期望 %d，实得 %d：%s", what, want, w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("%s 的 Content-Type 是 %q，契约要求 application/problem+json（正文：%q）",
			what, ct, w.Body.String())
	}
	var p struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("%s 的响应体不是 Problem: %v（%q）", what, err, w.Body.String())
	}
	// Problem 的 required 是 type / title / status，一个都不能缺。
	if p.Type == "" || p.Title == "" || p.Status != want {
		t.Fatalf("%s 的 Problem 不完整：type=%q title=%q status=%d",
			what, p.Type, p.Title, p.Status)
	}
}

// 没匹配上的路径回 Problem，不是 gin 默认的 text/plain "404 page not found"。
//
// 契约里每个接口的响应集合都是 200 加 default: Problem。一个 text/plain 的 404
// 两头都不沾，而路由拼错、版本前缀漏掉恰恰是客户端最常撞上的那类错误。
func TestUnknownPathGetsProblemJSON(t *testing.T) {
	for _, path := range []string{
		"/api/v1/nonexistent",
		"/api/v2/products", // 版本前缀写错
		"/products",        // 漏掉版本前缀
		"/",
	} {
		w := do(t, "shop-a."+baseDomain, path)
		assertProblem(t, w, http.StatusNotFound, "GET "+path)
	}
}

// 方法不匹配回 405 + Problem，而不是掉进 NoRoute 变成 404。
//
// 404 和 405 对调用方是两件事：「没这个接口」和「接口在，但不收这个方法」。
// gin 的 HandleMethodNotAllowed 默认是 false，不显式打开的话这个区别就没了。
func TestWrongMethodGetsProblemJSON(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		req := httptest.NewRequest(method, "/api/v1/products", nil)
		req.Host = "shop-a." + baseDomain
		w := httptest.NewRecorder()
		testEngine.ServeHTTP(w, req)
		assertProblem(t, w, http.StatusMethodNotAllowed, method+" /api/v1/products")
	}
}
