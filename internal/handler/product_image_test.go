package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/search"
)

// 商品图接进买家读路径（GET /products、POST /search、GET /products/{id}）。
//
// 契约早就声明了 ProductSummary.image_url 与 ProductDetail.images，后台也早就能
// 整组维护商品图（PUT /admin/products/{id}/images），但买家那三条读路径从来没填过
// —— 买家端因此只能显示色块占位。这一组钉住的是：
//
//   - 主图 = sort_order 最小的那一张，**不是 upload id 最小的那一张**。夹具刻意把
//     后传的那张（upload id 更大）排在第 0 位：按 upload id 排、按上传时间排的
//     实现在这里会红（实测：把 ORDER BY 改成 pi.upload_id，列表那条断言立刻红）；
//   - 列表、检索、详情三处给出的是**同一个** image_url；
//   - 详情的 images 是全部地址、按 sort_order 排好；
//   - 没有图的商品 image_url / images **缺席**，不是空串 / 空数组；
//   - 那个地址匿名打得开（商品图是公开用途，service/upload.go 的 publicPurposes），
//     否则买家端的 <image src> 拿到的是一个 403。
//
// 夹具全走真实接口（建商品、上传、挂图、上架），只有「让检索找得到它」那一步
// 直接写 search_text —— 那一步在生产上是异步索引任务做的，与商品图无关。

// productImageFixture 是一家店里的两件在架商品：一件挂两张图，一件一张没有。
type productImageFixture struct {
	Shop         adminShop
	WithImages   int64
	WithoutImage int64

	// Main / Second 是 WithImages 的第 0 / 第 1 张图。Main 是**后**上传的那张。
	Main, Second api.Upload
}

func newProductImageFixture(t *testing.T) productImageFixture {
	t.Helper()
	sh := newAdminShop(t)
	fx := productImageFixture{Shop: sh}
	fx.WithImages, _ = seedPublishedProduct(t, sh, "图片靶子", 1990, 5)
	fx.WithoutImage, _ = seedPublishedProduct(t, sh, "无图对照", 990, 5)

	decodeInto(t, uploadImage(t, sh, "image/png", []byte("\x89PNG\r\n\x1a\n first uploaded")),
		http.StatusCreated, "上传第一张", &fx.Second)
	decodeInto(t, uploadImage(t, sh, "image/jpeg", []byte("\xff\xd8\xff second uploaded")),
		http.StatusCreated, "上传第二张", &fx.Main)
	if fx.Main.Id <= fx.Second.Id {
		t.Fatalf("夹具前提不成立：后传的 upload id (%d) 应当大于先传的 (%d)", fx.Main.Id, fx.Second.Id)
	}

	var imgs []api.ProductImage
	decodeInto(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/images", fx.WithImages),
		fmt.Sprintf(`{"images":[{"upload_id":%d},{"upload_id":%d}]}`, fx.Main.Id, fx.Second.Id),
		sh.Token), http.StatusOK, "挂两张图", &imgs)
	if len(imgs) != 2 || imgs[0].UploadId != fx.Main.Id || imgs[0].SortOrder != 0 {
		t.Fatalf("挂图之后后台回的是 %+v，期望第 0 张是 upload %d", imgs, fx.Main.Id)
	}
	return fx
}

// wantImageURL 断言一条商品 JSON 的 image_url：want 为空串表示必须**缺席**。
func wantImageURL(t *testing.T, where string, item map[string]json.RawMessage, want string) {
	t.Helper()
	raw, present := item["image_url"]
	if want == "" {
		if present {
			t.Fatalf("%s：没有图的商品不该带 image_url，实得 %s —— "+
				"空串 / null 会让客户端去请求一个不存在的地址", where, raw)
		}
		return
	}
	var got string
	if !present || json.Unmarshal(raw, &got) != nil || got != want {
		t.Fatalf("%s：image_url 是 %s，期望 %q（sort_order = 0 的那张）", where, raw, want)
	}
}

// countFixtureItems 在一页 items 里找两件夹具商品并逐件断言 image_url，返回找到几件。
func countFixtureItems(t *testing.T, where string, fx productImageFixture,
	items []map[string]json.RawMessage) int {
	t.Helper()
	seen := 0
	for _, it := range items {
		var id int64
		_ = json.Unmarshal(it["id"], &id)
		switch id {
		case fx.WithImages:
			wantImageURL(t, where+"里挂了图的商品", it, fx.Main.Url)
			seen++
		case fx.WithoutImage:
			wantImageURL(t, where+"里没挂图的商品", it, "")
			seen++
		}
	}
	return seen
}

func TestBuyerListAndDetailCarryProductImages(t *testing.T) {
	fx := newProductImageFixture(t)
	sh := fx.Shop

	// 地址的形状以契约为准，而不是「和上传接口回的一样就行」：
	// 两边一起拼错的话，互相比对照样相等。
	if want := fmt.Sprintf("/api/v1/uploads/%d", fx.Main.Id); fx.Main.Url != want {
		t.Fatalf("Upload.url 是 %q，契约要求 %q", fx.Main.Url, want)
	}

	// ① 列表。
	w := do(t, sh.Host, "/api/v1/products?page_size=100")
	wantStatus(t, w, http.StatusOK, "买家列表")
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if n := countFixtureItems(t, "列表", fx, list.Items); n != 2 {
		t.Fatalf("列表里只找到 %d / 2 件夹具商品：%s", n, w.Body.String())
	}

	// ② 详情：image_url 是 images[0]，images 按 sort_order。
	code, d, raw := productDetail(t, sh.Host, fx.WithImages)
	if code != http.StatusOK {
		t.Fatalf("详情失败：%d %s", code, raw)
	}
	if d.ImageUrl == nil || *d.ImageUrl != fx.Main.Url {
		t.Fatalf("详情的 image_url 是 %v，期望 %q", d.ImageUrl, fx.Main.Url)
	}
	if d.Images == nil || strings.Join(*d.Images, ",") != fx.Main.Url+","+fx.Second.Url {
		t.Fatalf("详情的 images 是 %v，期望 [%s %s]（按 sort_order，不按 upload id）",
			d.Images, fx.Main.Url, fx.Second.Url)
	}

	code, _, raw = productDetail(t, sh.Host, fx.WithoutImage)
	if code != http.StatusOK {
		t.Fatalf("详情失败：%d %s", code, raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	wantImageURL(t, "没挂图的详情", m, "")
	if v, present := m["images"]; present {
		t.Fatalf("没挂图的详情不该带 images，实得 %s —— 空数组会让轮播组件显示「无图」", v)
	}
	// 阳性对照：别的字段必须在。整个响应是空对象的话，上面两句缺席断言是废话。
	for _, field := range []string{"id", "title", "skus"} {
		if _, ok := m[field]; !ok {
			t.Fatalf("响应里连 %s 都没有 —— 缺席断言没有区分力：%s", field, raw)
		}
	}

	// ③ 这个地址匿名打得开：买家端的 <image> 带不了 Authorization 头。
	for _, u := range []string{fx.Main.Url, fx.Second.Url} {
		wantStatus(t, getNoAuth(t, sh.Host, u), http.StatusFound, "匿名读商品图 "+u)
	}
}

func TestSearchHitsCarryProductImages(t *testing.T) {
	fx := newProductImageFixture(t)
	sh := fx.Shop

	// 让关键词那一路找得到这两件：search_text 在生产上由异步索引任务写，
	// 这里直接写，算法与索引任务同一份（search.ProductText）。
	for _, id := range []int64{fx.WithImages, fx.WithoutImage} {
		title := adminQueryText(t, `SELECT title FROM products WHERE id = $1`, id)
		if _, err := admin(t).Exec(context.Background(),
			`UPDATE products SET search_text = $2 WHERE id = $1`,
			id, search.ProductText{Title: title}.SearchText()); err != nil {
			t.Fatalf("写 search_text 失败: %v", err)
		}
	}

	w, body := doSearch(t, sh.Host, `{"query":"图片靶子 无图对照"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if n := countFixtureItems(t, "检索结果", fx, resp.Items); n != 2 {
		t.Fatalf("检索结果里只找到 %d / 2 件夹具商品：%v", n, titlesOf(body))
	}
}
