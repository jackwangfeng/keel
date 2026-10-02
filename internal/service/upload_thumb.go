package service

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // 注册解码器：商品图与头像可以是 PNG
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // 注册解码器：上传允许 image/webp
)

// 缩略图：GET /uploads/{upload_id}?w=（2026-09-28，Flutter 小程序在 iPhone 上滚动卡顿）。
//
// iOS 微信里 JS 与 wasm 都没有 JIT，图片在 wasm 里解码；商品图一律 800×800，而首页卡片只显示
// 约 173 宽、列表缩略图 56–76。客户端 cacheWidth 只省内存，省不了解码 —— 按显示尺寸出图，
// 解码量降到约 1/5。
//
// 契约：
//   - w 只认四档（ThumbWidths），其它值向上取最近一档，超过最大档按最大档 —— 缓存不被打散；
//   - 只对公开的两类（商品图、头像）生效；退款凭证照样给原图（没有列表场景，也不该多留一份副本）；
//   - 等比缩到这个宽度，不放大：原图不比这一档宽时直接给原图；
//   - 一律输出 JPEG（质量 80），透明底铺白 —— 商品图的透明 PNG 转 JPEG 时不铺底会变黑底；
//   - 按 (storage_key, w) 缓存在存储里（ThumbCache），第一次请求现算。

// ThumbWidths 是缩略图的档位，升序。
var ThumbWidths = []int{160, 320, 480, 640}

// ThumbQuality 是缩略图的 JPEG 质量。
const ThumbQuality = 80

// SnapThumbWidth 把请求的宽度归到档位上：<= 0 返回 0（原图），否则向上取最近一档，超过最大档取最大档。
func SnapThumbWidth(w int) int {
	if w <= 0 {
		return 0
	}
	for _, t := range ThumbWidths {
		if w <= t {
			return t
		}
	}
	return ThumbWidths[len(ThumbWidths)-1]
}

// ThumbCache 是存储 driver 可选的缩略图缓存能力。没实现它的 driver 每次现算（功能不变，只是慢）。
type ThumbCache interface {
	// OpenThumb 打开缓存的缩略图；不存在返回 os.ErrNotExist（可用 errors.Is 判）。
	OpenThumb(key string, w int) (io.ReadCloser, int64, error)
	// PutThumb 写入缩略图。并发写同一个 key 时后写的覆盖先写的（内容相同），不能留下半个文件。
	PutThumb(key string, w int, data []byte) error
}

func thumbPath(root, key string, w int) string {
	return filepath.Join(root, "thumbs", filepath.FromSlash(key)+".w"+strconv.Itoa(w)+".jpg")
}

// OpenThumb 实现 ThumbCache。
func (s *LocalDiskStore) OpenThumb(key string, w int) (io.ReadCloser, int64, error) {
	if s.root == "" || key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return nil, 0, os.ErrNotExist
	}
	f, err := os.Open(thumbPath(s.root, key, w))
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// PutThumb 实现 ThumbCache：先写临时文件再 rename，读的一方永远看不到半个文件。
func (s *LocalDiskStore) PutThumb(key string, w int, data []byte) error {
	if s.root == "" || key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return fmt.Errorf("缩略图 key %q 不合法", key)
	}
	full := thumbPath(s.root, key, w)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".thumb-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}

// thumbBlob 给这个文件出 w 档的缩略图。不需要缩（原图不比 w 宽、解不开）时 ok 为 false，调用方照给原图。
//
// st 是这一行的 driver（storeFor）：缩略图缓存在原图所在的那个 driver 里。
func (s *UploadService) thumbBlob(st UploadStore, storageKey string, w int) (UploadBlob, bool, error) {
	cache, _ := st.(ThumbCache)
	if cache != nil {
		if rc, n, err := cache.OpenThumb(storageKey, w); err == nil {
			return UploadBlob{ContentType: "image/jpeg", SizeBytes: n, Body: rc}, true, nil
		}
	}
	rc, err := st.Open(storageKey)
	if err != nil {
		return UploadBlob{}, false, err
	}
	src, _, err := image.Decode(rc)
	_ = rc.Close()
	if err != nil {
		// 声明是图片、内容解不开（上传时不嗅探内容）：不替它出缩略图，照给原字节 —— 与不带 w 时一样。
		slog.Warn("缩略图：原图解不开，照给原图", "storage_key", storageKey, "err", err)
		return UploadBlob{}, false, nil
	}
	b := src.Bounds()
	if b.Dx() <= w {
		return UploadBlob{}, false, nil // 不放大
	}
	h := max(1, b.Dy()*w/b.Dx())
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: ThumbQuality}); err != nil {
		return UploadBlob{}, false, err
	}
	data := buf.Bytes()
	if cache != nil {
		if err := cache.PutThumb(storageKey, w, data); err != nil {
			slog.Warn("缩略图没写进缓存（下次请求再算一次）", "storage_key", storageKey, "w", w, "err", err)
		}
	}
	return UploadBlob{ContentType: "image/jpeg", SizeBytes: int64(len(data)),
		Body: io.NopCloser(bytes.NewReader(data))}, true, nil
}
