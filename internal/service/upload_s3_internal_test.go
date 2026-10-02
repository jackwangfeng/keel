package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// 对象存储 driver 对着一个进程内的 S3 兼容服务（gofakes3）跑：请求是真 HTTP、真 S3 协议（minio-go 发、gofakes3 收），
// 只是后端在内存里。真 MinIO 的端到端在 compose.s3.yaml 那一栈上（部署指南「对象存储」）。

func fakeS3(t *testing.T, bucket string) (*httptest.Server, *s3mem.Backend) {
	t.Helper()
	be := s3mem.New()
	if bucket != "" {
		if err := be.CreateBucket(bucket); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(gofakes3.New(be).Server())
	t.Cleanup(srv.Close)
	return srv, be
}

func s3cfg(url, bucket string) S3Config {
	return S3Config{Endpoint: url, Bucket: bucket, AccessKey: "ak", SecretKey: "sk", AddressStyle: "path"}
}

func countObjects(t *testing.T, be *s3mem.Backend, bucket string) int {
	t.Helper()
	l, err := be.ListBucket(bucket, nil, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	return len(l.Contents)
}

func TestS3StoreRefusesMissingBucketAndBadConfig(t *testing.T) {
	srv, _ := fakeS3(t, "")
	if _, err := NewS3Store(context.Background(), s3cfg(srv.URL, "nope")); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("桶不存在应当拒绝，并点名桶：%v", err)
	}
	if _, err := NewS3Store(context.Background(), S3Config{Endpoint: srv.URL, Bucket: "b"}); err == nil {
		t.Error("没有凭据应当拒绝")
	}
	if _, err := NewS3Store(context.Background(), s3cfg("minio:9000", "b")); err == nil {
		t.Error("地址不是 http(s):// 应当拒绝")
	}
}

func TestS3StorePutOpenRemoveAndThumbs(t *testing.T) {
	srv, be := fakeS3(t, "keel")
	cfg := s3cfg(srv.URL, "keel")
	cfg.Prefix = "shop/"
	st, err := NewS3Store(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Driver() != 2 {
		t.Fatalf("driver = %d，期望 2", st.Driver())
	}

	data := bytes.Repeat([]byte("keel"), 1000)
	key, n, sum, err := st.Put(42, ".jpg", bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if n != int64(len(data)) || sum != hex.EncodeToString(want[:]) || !strings.HasPrefix(key, "42/") || !strings.HasSuffix(key, ".jpg") {
		t.Fatalf("Put 返回 key=%q n=%d sum=%s", key, n, sum)
	}
	if _, err := be.HeadObject("keel", "shop/"+key); err != nil {
		t.Fatalf("桶里应当有 shop/%s（前缀生效）：%v", key, err)
	}
	rc, err := st.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("读回来的字节不一致")
	}

	// 超限与空文件：拒绝，桶里不多出任何东西。
	before := countObjects(t, be, "keel")
	if _, _, _, err := st.Put(42, ".jpg", bytes.NewReader(make([]byte, 101)), 100); !errors.Is(err, ErrUploadTooLarge) {
		t.Errorf("超限应当 ErrUploadTooLarge：%v", err)
	}
	if _, _, _, err := st.Put(42, ".jpg", bytes.NewReader(nil), 100); err == nil {
		t.Error("空文件应当拒绝")
	}
	if _, _, _, err := st.Put(42, ".jpg", bytes.NewReader(make([]byte, 100)), 100); err != nil {
		t.Errorf("正好等于上限应当接受：%v", err)
	}
	if after := countObjects(t, be, "keel"); after != before+1 {
		t.Errorf("超限 / 空文件不该在桶里留东西：%d → %d", before, after)
	}

	// 不存在的 key：ErrUploadBlobMissing（与「记录不存在」的 404 分开）。
	if _, err := st.Open("42/zz/nothing.jpg"); !errors.Is(err, ErrUploadBlobMissing) {
		t.Errorf("不存在应当 ErrUploadBlobMissing：%v", err)
	}
	// key 形状：穿越与绝对路径一律拒绝。
	for _, bad := range []string{"", "/etc/passwd", "42/../../x"} {
		if _, err := st.Open(bad); err == nil {
			t.Errorf("Open(%q) 应当拒绝", bad)
		}
		if err := st.Remove(bad); err == nil {
			t.Errorf("Remove(%q) 应当拒绝", bad)
		}
	}

	// 缩略图：先不存在，写入后读得到；删原图时一起删掉。
	if _, _, err := st.OpenThumb(key, 160); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("还没生成的缩略图应当 os.ErrNotExist：%v", err)
	}
	if err := st.PutThumb(key, 160, []byte("thumb")); err != nil {
		t.Fatal(err)
	}
	trc, tn, err := st.OpenThumb(key, 160)
	if err != nil || tn != 5 {
		t.Fatalf("缩略图读不回来：%v %d", err, tn)
	}
	trc.Close()
	if err := st.Remove(key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Open(key); !errors.Is(err, ErrUploadBlobMissing) {
		t.Error("删了之后应当读不到")
	}
	if _, _, err := st.OpenThumb(key, 160); !errors.Is(err, os.ErrNotExist) {
		t.Error("原图删了，缩略图应当跟着删")
	}
	if err := st.Remove(key); err != nil {
		t.Errorf("删不存在的 key 应当是成功（确保它没了）：%v", err)
	}
}

func TestS3StorePresign(t *testing.T) {
	srv, _ := fakeS3(t, "keel")
	plain, err := NewS3Store(context.Background(), s3cfg(srv.URL, "keel"))
	if err != nil {
		t.Fatal(err)
	}
	key, _, _, err := plain.Put(1, ".png", strings.NewReader("png-bytes"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := plain.PresignGet(key, 0, "image/png", time.Minute); ok || err != nil {
		t.Errorf("没配对外地址时不该预签名：ok=%v err=%v", ok, err)
	}

	cfg := s3cfg(srv.URL, "keel")
	cfg.PresignEndpoint = srv.URL // 测试里对外地址就是同一个服务
	st, err := NewS3Store(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, ok, err := st.PresignGet(key, 0, "image/png", time.Minute)
	if err != nil || !ok || !strings.HasPrefix(u, srv.URL) || !strings.Contains(u, "X-Amz-Signature") ||
		!strings.Contains(u, "response-content-type=image%2Fpng") {
		t.Fatalf("预签名地址不对：%q ok=%v err=%v", u, ok, err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "png-bytes" {
		t.Errorf("照预签名地址取回来的是 %q", b)
	}
	// 缩略图：没生成的那一档不预签名（要走站内现算），生成之后才签。
	if _, ok, _ := st.PresignGet(key, 320, "image/png", time.Minute); ok {
		t.Error("还没生成的缩略图不该预签名")
	}
	if err := st.PutThumb(key, 320, []byte("t")); err != nil {
		t.Fatal(err)
	}
	if u, ok, err := st.PresignGet(key, 320, "image/png", time.Minute); err != nil || !ok || !strings.Contains(u, ".w320.jpg") {
		t.Errorf("生成之后的缩略图应当预签名：%q %v %v", u, ok, err)
	}
}

func TestUploadStorageRoutesByDriver(t *testing.T) {
	srv, _ := fakeS3(t, "keel")
	s3, err := NewS3Store(context.Background(), s3cfg(srv.URL, "keel"))
	if err != nil {
		t.Fatal(err)
	}
	local := NewLocalDiskStore(t.TempDir())
	st, err := NewUploadStorage(s3, local)
	if err != nil {
		t.Fatal(err)
	}
	if st.Driver() != 2 || st.Primary() != UploadStore(s3) {
		t.Fatal("写应当落在主 driver（S3）上")
	}
	if d := st.Drivers(); len(d) != 2 || d[0] != 1 || d[1] != 2 {
		t.Fatalf("Drivers() = %v", d)
	}
	// 旧文件在磁盘上：按行的 driver=1 读得到。
	lkey, _, _, err := local.Put(7, ".jpg", strings.NewReader("old"), 100)
	if err != nil {
		t.Fatal(err)
	}
	ld, err := storeFor(st, 1)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := ld.Open(lkey)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if _, err := storeFor(st, 3); !errors.Is(err, ErrUploadBlobMissing) {
		t.Errorf("没配的 driver 应当 ErrUploadBlobMissing：%v", err)
	}
	if _, err := NewUploadStorage(s3, s3); err == nil {
		t.Error("同一个 driver 配两次应当拒绝")
	}
	// 只有单个 driver 时：行的 driver 对不上就报错，不拿错的 driver 去读。
	if _, err := storeFor(local, 2); !errors.Is(err, ErrUploadBlobMissing) {
		t.Errorf("单 driver 对不上应当报错：%v", err)
	}
}
