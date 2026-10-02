package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 对着一个**真的** S3 兼容服务跑一遍 driver（SeaweedFS、OSS、COS、AWS……接新的对象存储之前先跑它）。
// 平时跳过；设了 KEEL_TEST_S3_ENDPOINT 等变量才跑（部署指南「对象存储」一节有用 compose.s3.yaml 起 SeaweedFS 的命令）：
//
//	KEEL_TEST_S3_ENDPOINT=http://127.0.0.1:8333 KEEL_TEST_S3_BUCKET=keel KEEL_TEST_S3_ACCESS_KEY=... \
//	KEEL_TEST_S3_SECRET_KEY=... [KEEL_TEST_S3_ADDRESS_STYLE=path] go test ./internal/service/ -run TestS3StoreAgainstRealService
func TestS3StoreAgainstRealService(t *testing.T) {
	ep := os.Getenv("KEEL_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("没设 KEEL_TEST_S3_ENDPOINT，跳过真对象存储的兼容性测试")
	}
	cfg := S3Config{Endpoint: ep, Bucket: os.Getenv("KEEL_TEST_S3_BUCKET"), Region: os.Getenv("KEEL_TEST_S3_REGION"),
		AccessKey: os.Getenv("KEEL_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("KEEL_TEST_S3_SECRET_KEY"),
		AddressStyle: os.Getenv("KEEL_TEST_S3_ADDRESS_STYLE"), Prefix: "keel-compat-test/", PresignEndpoint: ep}
	st, err := NewS3Store(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0, 1, 2, 3, 250, 251, 252, 253}, 4096)
	key, n, _, err := st.Put(1, ".jpg", bytes.NewReader(data), 1<<20)
	if err != nil || n != int64(len(data)) {
		t.Fatalf("Put：%v n=%d", err, n)
	}
	t.Cleanup(func() { _ = st.Remove(key) })
	rc, err := st.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("读回来不一致")
	}
	if _, err := st.Open("1/zz/does-not-exist.jpg"); !errors.Is(err, ErrUploadBlobMissing) {
		t.Errorf("不存在的对象应当 ErrUploadBlobMissing：%v", err)
	}
	if err := st.PutThumb(key, 160, []byte("thumb")); err != nil {
		t.Fatal(err)
	}
	u, ok, err := st.PresignGet(key, 0, "image/jpeg", time.Minute)
	if err != nil || !ok {
		t.Fatalf("预签名：%v %v", ok, err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, data) || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/jpeg") {
		t.Fatalf("预签名地址取回：%d ct=%q 一致=%v", resp.StatusCode, resp.Header.Get("Content-Type"), bytes.Equal(body, data))
	}
	if err := st.Remove(key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.OpenThumb(key, 160); !errors.Is(err, os.ErrNotExist) {
		t.Error("删原图应当连缩略图一起删")
	}
}
