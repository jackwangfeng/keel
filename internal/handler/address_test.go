package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 地址簿（/addresses）的端到端测试。规则来源：契约 User tag 那 6 个操作、
// 数据模型 §9 user_addresses（默认地址至多一个、软删、快照而非外键）。

// 增删改查走一遍，顺带核「默认在首、其余按更新时间倒序」与「软删的不返回」。
func TestAddressBookLifecycle(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "地址簿")
	// newBuyer 用 SQL 落了一条非默认地址（id = b.Address），它也该出现在地址簿里。

	a1 := bs.createAddress(t, b, "张三", false)
	a2 := bs.createAddress(t, b, "李四", true)
	if a1.IsDefault || !a2.IsDefault {
		t.Fatalf("is_default 回显不对：a1=%v a2=%v", a1.IsDefault, a2.IsDefault)
	}
	if a2.Tag == nil || *a2.Tag != 1 || a2.Street == nil || *a2.Street != "林和街道" {
		t.Fatalf("可选字段没有回显：%+v", a2)
	}

	list := bs.listAddresses(t, b)
	if len(list) != 3 {
		t.Fatalf("地址簿应有 3 条，实得 %d：%+v", len(list), list)
	}
	if list[0].Id != a2.Id {
		t.Fatalf("默认地址必须排在首位，实得第一条是 %d（默认是 %d）", list[0].Id, a2.Id)
	}
	// 其余按更新时间倒序：a1 比种下的那条新。
	if list[1].Id != a1.Id || list[2].Id != b.Address {
		t.Fatalf("非默认地址应按更新时间倒序：%d, %d（期望 %d, %d）", list[1].Id, list[2].Id, a1.Id, b.Address)
	}

	// 整体替换：字段全换，is_default 不动（契约：仅在新增时有效）。
	var got api.Address
	decodeInto(t, bs.call(t, http.MethodPut, fmt.Sprintf("/api/v1/addresses/%d", a2.Id),
		`{"receiver_name":"李四改","phone":"13900139000","province":"北京市","city":"北京市",`+
			`"district":"朝阳区","detail":"建国路 2 号"}`, b), http.StatusOK, "整体替换", &got)
	if got.ReceiverName != "李四改" || got.Province != "北京市" || !got.IsDefault {
		t.Fatalf("整体替换结果不对：%+v", got)
	}
	if got.Street == nil || *got.Street != "" || got.Tag == nil || *got.Tag != 0 {
		t.Fatalf("整体替换时没给的可选字段应回到默认值（street 空串、tag 0）：%+v", got)
	}

	decodeInto(t, bs.call(t, http.MethodGet, fmt.Sprintf("/api/v1/addresses/%d", a2.Id), "", b),
		http.StatusOK, "详情", &got)
	if got.ReceiverName != "李四改" {
		t.Fatalf("详情没读到替换后的值：%+v", got)
	}

	// 软删：之后详情 404、地址簿里不出现，但库里那一行还在（写的是 deleted_at）。
	wantStatus(t, bs.call(t, http.MethodDelete, fmt.Sprintf("/api/v1/addresses/%d", a2.Id), "", b),
		http.StatusNoContent, "删除")
	if typ := problemType(t, bs.call(t, http.MethodGet, fmt.Sprintf("/api/v1/addresses/%d", a2.Id), "", b),
		http.StatusNotFound, "删后详情"); typ != problem.TypeNotFound {
		t.Fatalf("删后详情的 type 是 %s", typ)
	}
	if n := len(bs.listAddresses(t, b)); n != 2 {
		t.Fatalf("删掉一条之后地址簿应剩 2 条，实得 %d", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM user_addresses WHERE id = $1 AND deleted_at IS NOT NULL`,
		a2.Id); n != 1 {
		t.Fatal("DELETE 应当是软删（写 deleted_at），库里那一行却不在了")
	}
	// 删掉的默认地址不留一个「幽灵默认」，也不替用户扶一个新默认上来。
	if n := defaultAddressCount(t, b.UserID); n != 0 {
		t.Fatalf("删掉默认地址后应当没有默认地址，实得 %d 条", n)
	}
	// 重复删：它已经不存在了。
	wantStatus(t, bs.call(t, http.MethodDelete, fmt.Sprintf("/api/v1/addresses/%d", a2.Id), "", b),
		http.StatusNotFound, "重复删除")
}

// 默认地址互斥：新增时带 is_default、专用接口切换，任何时刻库里至多一条默认。
func TestDefaultAddressIsExclusive(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "默认")

	a := bs.createAddress(t, b, "甲", true)
	c := bs.createAddress(t, b, "乙", true)
	if n := defaultAddressCount(t, b.UserID); n != 1 {
		t.Fatalf("新增第二条默认地址后，库里应恰好 1 条默认，实得 %d", n)
	}
	var got api.Address
	decodeInto(t, bs.call(t, http.MethodGet, fmt.Sprintf("/api/v1/addresses/%d", a.Id), "", b),
		http.StatusOK, "旧默认", &got)
	if got.IsDefault {
		t.Fatal("新增一条默认地址之后，旧的默认没有被清掉")
	}

	// 切回 a。
	decodeInto(t, bs.call(t, http.MethodPut, fmt.Sprintf("/api/v1/addresses/%d/default", a.Id), "", b),
		http.StatusOK, "设为默认", &got)
	if !got.IsDefault {
		t.Fatal("设为默认之后响应里 is_default 仍是 false")
	}
	if n := defaultAddressCount(t, b.UserID); n != 1 {
		t.Fatalf("切换默认后库里应恰好 1 条默认，实得 %d", n)
	}
	// 幂等：对已是默认的再设一次，200，不报错。
	decodeInto(t, bs.call(t, http.MethodPut, fmt.Sprintf("/api/v1/addresses/%d/default", a.Id), "", b),
		http.StatusOK, "重复设为默认", &got)
	if !got.IsDefault || defaultAddressCount(t, b.UserID) != 1 {
		t.Fatal("对已是默认的地址重复设为默认之后，默认地址不对了")
	}

	// PUT 不许把非默认改成默认（契约 422 use-default-endpoint），对已是默认的则无妨。
	if typ := problemType(t, bs.call(t, http.MethodPut, fmt.Sprintf("/api/v1/addresses/%d", c.Id),
		addressBody("乙", true), b), http.StatusUnprocessableEntity, "PUT 切默认"); typ != problem.TypeUseDefaultEndpoint {
		t.Fatalf("PUT 切默认的 type 是 %s，期望 use-default-endpoint", typ)
	}
	wantStatus(t, bs.call(t, http.MethodPut, fmt.Sprintf("/api/v1/addresses/%d", a.Id),
		addressBody("甲", true), b), http.StatusOK, "PUT 已是默认的地址")
	// PUT 带 false 也不取消默认：is_default 在这条接口上不生效。
	decodeInto(t, bs.call(t, http.MethodPut, fmt.Sprintf("/api/v1/addresses/%d", a.Id),
		addressBody("甲", false), b), http.StatusOK, "PUT 带 false", &got)
	if !got.IsDefault {
		t.Fatal("PUT 带 is_default=false 把默认地址取消了 —— 契约说它只在新增时有效")
	}
}

// 并发切换默认：一个都不能 500，结束时恰好一条默认。
//
// 这条盯的是服务端那把「每个买家一把」的锁（LockUser）。没有它，两个并发的切换
// 各自清旧时看不见对方还没提交的新默认，第二个置新时撞上 uk_user_addresses_default ——
// 客户端看到的是 500，而契约把切换收进专用接口的理由正是「别让并发点两下报 500」。
func TestConcurrentDefaultSwitchNeverFails(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "并发默认")
	const n = 8
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = bs.createAddress(t, b, fmt.Sprintf("收件人%d", i), false).Id
	}
	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		codes := make([]int, n)
		bodies := make([]string, n)
		for i, id := range ids {
			wg.Add(1)
			go func(i int, id int64) {
				defer wg.Done()
				w := bs.call(t, http.MethodPut, fmt.Sprintf("/api/v1/addresses/%d/default", id), "", b)
				codes[i], bodies[i] = w.Code, w.Body.String()
			}(i, id)
		}
		wg.Wait()
		for i, c := range codes {
			if c != http.StatusOK {
				t.Fatalf("第 %d 轮并发设为默认：第 %d 个请求 %d %s", round, i, c, bodies[i])
			}
		}
		if got := defaultAddressCount(t, b.UserID); got != 1 {
			t.Fatalf("第 %d 轮并发切换之后库里有 %d 条默认地址，期望恰好 1 条", round, got)
		}
	}

	// 并发新增默认地址同理。
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = bs.call(t, http.MethodPost, "/api/v1/addresses", addressBody("并发新增", true), b).Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusCreated {
			t.Fatalf("并发新增默认地址：第 %d 个请求 %d", i, c)
		}
	}
	if got := defaultAddressCount(t, b.UserID); got != 1 {
		t.Fatalf("并发新增默认地址之后库里有 %d 条默认，期望恰好 1 条", got)
	}
}

// 越权：同一家店里另一个买家的地址，读、改、删、设默认一律 404（不是 403），
// 而且那条地址原封不动。跨租户（另一家店的买家）同样 404 —— 那一面由 RLS 挡。
func TestAddressesOfOthersAreNotFound(t *testing.T) {
	bs := newBuyerShop(t)
	owner := bs.newBuyer(t, "主人")
	intruder := bs.newBuyer(t, "旁人")
	victim := bs.createAddress(t, owner, "主人的地址", true)

	path := fmt.Sprintf("/api/v1/addresses/%d", victim.Id)
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, path, ""},
		{http.MethodPut, path, addressBody("旁人改的", false)},
		{http.MethodDelete, path, ""},
		{http.MethodPut, path + "/default", ""},
	} {
		w := bs.call(t, tc.method, tc.path, tc.body, intruder)
		if typ := problemType(t, w, http.StatusNotFound, tc.method+" "+tc.path); typ != problem.TypeNotFound {
			t.Fatalf("%s %s：type 是 %s", tc.method, tc.path, typ)
		}
	}
	for _, a := range bs.listAddresses(t, intruder) {
		if a.Id == victim.Id {
			t.Fatal("别人的地址出现在了我的地址簿里")
		}
	}
	// 原封不动：没被改、没被删、仍是主人的默认地址。
	var got api.Address
	decodeInto(t, bs.call(t, http.MethodGet, path, "", owner), http.StatusOK, "主人读自己的地址", &got)
	if got.ReceiverName != "主人的地址" || !got.IsDefault {
		t.Fatalf("旁人的请求改动了主人的地址：%+v", got)
	}
	if defaultAddressCount(t, intruder.UserID) != 0 {
		t.Fatal("旁人对别人地址的「设为默认」在旁人名下留下了一条默认地址")
	}

	// 跨租户：另一家店的买家拿着这个 id 去他自己的店里问。
	other := newBuyerShop(t)
	stranger := other.newBuyer(t, "别家店")
	wantStatus(t, other.call(t, http.MethodGet, path, "", stranger), http.StatusNotFound, "跨租户读地址")
	wantStatus(t, other.call(t, http.MethodPut, path+"/default", "", stranger), http.StatusNotFound, "跨租户设默认")
}

// POST /addresses 的幂等（契约：Idempotency-Key 必填）。
func TestAddressCreateIsIdempotent(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "幂等")
	key := freshIdemKey()
	body := addressBody("幂等收件人", false)

	w1 := bs.callWithKey(t, "/api/v1/addresses", body, b, key)
	var first api.Address
	decodeInto(t, w1, http.StatusCreated, "首次新增", &first)
	if w1.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatal("首次执行不该带 Idempotency-Replayed: true")
	}
	w2 := bs.callWithKey(t, "/api/v1/addresses", body, b, key)
	var second api.Address
	decodeInto(t, w2, http.StatusCreated, "重放", &second)
	if w2.Header().Get("Idempotency-Replayed") != "true" || second.Id != first.Id {
		t.Fatalf("同一把钥匙第二次应回放首次那条（id %d），实得 id %d、Replayed=%q",
			first.Id, second.Id, w2.Header().Get("Idempotency-Replayed"))
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM user_addresses WHERE user_id = $1 AND receiver_name = '幂等收件人'`,
		b.UserID); n != 1 {
		t.Fatalf("同一把钥匙打两次建出了 %d 条地址", n)
	}
	if typ := problemType(t, bs.callWithKey(t, "/api/v1/addresses", addressBody("换了个人", false), b, key),
		http.StatusUnprocessableEntity, "同钥匙不同请求体"); typ != problem.TypeIdempotencyKeyReused {
		t.Fatalf("同钥匙不同请求体的 type 是 %s", typ)
	}
	if typ := problemType(t, bs.callWithKey(t, "/api/v1/addresses", body, b, ""),
		http.StatusUnprocessableEntity, "不带钥匙"); typ != problem.TypeInvalidRequest {
		t.Fatalf("不带钥匙的 type 是 %s", typ)
	}
}

// 字段校验：一次报全，逐条给出 field（契约 422「见 Problem.errors」）。
func TestAddressValidationReportsEveryField(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "校验")
	body := fmt.Sprintf(`{"receiver_name":"  ","phone":"call-me","province":"广东省","city":"广州市",`+
		`"district":"天河区","detail":%q,"tag":9,"region_code":"44A"}`, strings.Repeat("长", 201))
	w := bs.call(t, http.MethodPost, "/api/v1/addresses", body, b)
	if typ := problemType(t, w, http.StatusUnprocessableEntity, "非法地址"); typ != problem.TypeInvalidRequest {
		t.Fatalf("type 是 %s", typ)
	}
	var p api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Errors == nil {
		t.Fatalf("422 里没有 errors：%s", w.Body.String())
	}
	got := map[string]bool{}
	for _, e := range *p.Errors {
		if e.Field != nil {
			got[*e.Field] = true
		}
	}
	for _, f := range []string{"receiver_name", "phone", "detail", "tag", "region_code"} {
		if !got[f] {
			t.Errorf("errors 里缺了 %s：%s", f, w.Body.String())
		}
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM user_addresses WHERE user_id = $1`, b.UserID); n != 1 {
		t.Fatalf("校验失败的请求落了库（地址数 %d，期望只有夹具那 1 条）", n)
	}
}

// 客户端链路：新建的地址能直接拿去下单；别人的、已删的不能（下单那一侧的越权过滤）。
//
// 买家端下单页今天用的是种子地址 id，接上 /addresses 之后它拿到的就是这里建出来的 id ——
// 这条测试钉的正是「地址簿里的 id 与下单要的 address_id 是同一个东西」。
func TestNewAddressIsUsableForCheckout(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "下单")
	other := bs.newBuyer(t, "别人")
	a := bs.createAddress(t, b, "下单收件人", true)

	body := fmt.Sprintf(`{"items":[{"sku_id":%d,"quantity":1}],"address_id":%d,"store_id":%d}`,
		bs.DressSKU, a.Id, bs.NorthStore)
	wantStatus(t, previewOrder(t, bs.Host, body, b.Token), http.StatusOK, "用新地址试算")
	wantStatus(t, createOrder(t, bs.Host, body, other.Token, freshIdemKey()),
		http.StatusUnprocessableEntity, "用别人的地址下单")
	var o api.Order
	decodeInto(t, createOrder(t, bs.Host, body, b.Token, freshIdemKey()), http.StatusCreated, "用新地址下单", &o)

	wantStatus(t, bs.call(t, http.MethodDelete, fmt.Sprintf("/api/v1/addresses/%d", a.Id), "", b),
		http.StatusNoContent, "删地址")
	wantStatus(t, createOrder(t, bs.Host, body, b.Token, freshIdemKey()),
		http.StatusUnprocessableEntity, "用已删地址下单")
	// 快照而非外键：删掉地址不影响已经下的那一单的收货信息。
	var d api.OrderDetail
	decodeInto(t, bs.call(t, http.MethodGet, "/api/v1/orders/"+o.OrderNo, "", b), http.StatusOK, "订单详情", &d)
	if d.Receiver == nil || d.Receiver.ReceiverName != "下单收件人" {
		t.Fatalf("删地址之后订单的收货快照变了：%+v", d.Receiver)
	}
}

// 地址带坐标（POI，00100）：搜索地点 / 选点填的地址带 WGS-84 坐标回显；整体替换不给就清掉；只给一个 422。
func TestAddressCoordinates(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "地址坐标")
	base := `"receiver_name":"王五","phone":"13900139000","province":"北京市","city":"北京市","district":"朝阳区","detail":"望京 SOHO"`
	var a api.Address
	w := bs.call(t, http.MethodPost, "/api/v1/addresses", `{`+base+`,"lat":39.996539,"lng":116.480983}`, b)
	decodeInto(t, w, http.StatusCreated, "带坐标新建", &a)
	if a.Lat == nil || a.Lng == nil || *a.Lat != 39.996539 || *a.Lng != 116.480983 {
		t.Fatalf("坐标没回显或丢了精度：%v %v", a.Lat, a.Lng)
	}
	var replaced api.Address // 新变量：响应里没有 lat 时 JSON 解码不会清掉旧值
	decodeInto(t, bs.call(t, http.MethodPut, fmt.Sprintf("/api/v1/addresses/%d", a.Id), `{`+base+`}`, b), http.StatusOK, "替换", &replaced)
	if replaced.Lat != nil || replaced.Lng != nil {
		t.Fatalf("整体替换没给坐标应清掉：%v %v", *replaced.Lat, *replaced.Lng)
	}
	if w := bs.call(t, http.MethodPost, "/api/v1/addresses", `{`+base+`,"lat":39.99}`, b); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("只给 lat 应 422，实得 %d", w.Code)
	}
}
