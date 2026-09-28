package handler_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 缩略图 GET /uploads/{upload_id}?w=（2026-09-28，小程序在 iPhone 上滚动卡顿：图片在 wasm 里解码）。
//
// 一张 800×600 的透明 PNG 商品图：
//   - ?w=300 → 302 的限时地址带 w=320（向上取档）→ 第二跳 image/jpeg、宽 320、高按比例 240、透明处铺白；
//   - 第二次请求同一档读缓存，字节逐字相同；
//   - ?w=2000 → 640（超过最大档取最大档）；
//   - ?w=abc / ?w=0 → 422；
//   - 不带 w → 原字节（行为不变）。
//
// 另一张 100 宽的小图带 ?w=160：不放大，给原字节。
func TestUploadThumbnails(t *testing.T) {
	sh := newAdminShop(t)
	src := image.NewNRGBA(image.Rect(0, 0, 800, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 800; x++ {
			if x < 400 {
				src.Set(x, y, color.NRGBA{R: 200, G: 30, B: 30, A: 255})
			} // 右半边留透明
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatal(err)
	}
	var up api.Upload
	decodeInto(t, uploadImage(t, sh, "image/png", pngBuf.Bytes()), http.StatusCreated, "传图", &up)

	fetch := func(query string) (*http.Response, []byte, string) {
		t.Helper()
		first := getNoAuth(t, sh.Host, up.Url+query)
		if first.Code != http.StatusFound {
			t.Fatalf("GET %s%s 回了 %d：%s", up.Url, query, first.Code, first.Body.String())
		}
		loc := first.Header().Get("Location")
		blob := getNoAuth(t, sh.Host, loc)
		if blob.Code != http.StatusOK {
			t.Fatalf("第二跳 %s 回了 %d：%s", loc, blob.Code, blob.Body.String())
		}
		return blob.Result(), blob.Body.Bytes(), loc
	}
	wOf := func(loc string) string {
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatal(err)
		}
		return u.Query().Get("w")
	}

	res, body, loc := fetch("?w=300")
	if wOf(loc) != "320" {
		t.Fatalf("?w=300 的限时地址是 %q，期望带 w=320（向上取档）", loc)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
		t.Fatalf("缩略图 Content-Type 是 %q，期望 image/jpeg", ct)
	}
	img, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("缩略图不是一张 JPEG：%v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Fatalf("缩略图 %dx%d，期望 320x240（等比）", b.Dx(), b.Dy())
	}
	if r, g, b, _ := img.At(300, 120).RGBA(); r>>8 < 230 || g>>8 < 230 || b>>8 < 230 {
		t.Errorf("透明处是 (%d,%d,%d)，期望铺白 —— 透明 PNG 转 JPEG 不铺底会变黑", r>>8, g>>8, b>>8)
	}
	if _, again, _ := fetch("?w=300"); !bytes.Equal(again, body) {
		t.Error("同一档第二次请求的字节不一样 —— 没走缓存，或者每次现算结果不稳定")
	}

	if _, _, loc := fetch("?w=2000"); wOf(loc) != "640" {
		t.Errorf("?w=2000 的限时地址是 %q，期望 w=640（超过最大档取最大档）", loc)
	}
	for _, bad := range []string{"?w=abc", "?w=0", "?w=-5"} {
		if w := getNoAuth(t, sh.Host, up.Url+bad); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET %s%s 回了 %d，期望 422", up.Url, bad, w.Code)
		}
	}
	if _, orig, loc := fetch(""); wOf(loc) != "" || !bytes.Equal(orig, pngBuf.Bytes()) {
		t.Errorf("不带 w 应给原字节、地址上没有 w（loc=%q，%d 字节 vs 原 %d）", loc, len(orig), pngBuf.Len())
	}

	// 小图不放大。
	small := image.NewRGBA(image.Rect(0, 0, 100, 80))
	var smallBuf bytes.Buffer
	if err := png.Encode(&smallBuf, small); err != nil {
		t.Fatal(err)
	}
	var sup api.Upload
	decodeInto(t, uploadImage(t, sh, "image/png", smallBuf.Bytes()), http.StatusCreated, "传小图", &sup)
	first := getNoAuth(t, sh.Host, sup.Url+"?w=160")
	blob := getNoAuth(t, sh.Host, first.Header().Get("Location"))
	if !bytes.Equal(blob.Body.Bytes(), smallBuf.Bytes()) {
		t.Error("100 宽的小图带 ?w=160 应原样返回（不放大）")
	}
}
