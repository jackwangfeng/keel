package app_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/keel/keel/internal/app"
)

// 上传存储的配置不对就拒绝启动（app/upload_store.go）：写错的 driver、对象存储缺配置、桶不存在。
// 一个所有上传都 500 的进程比一个起不来的进程难查得多。
func TestRunRefusesBadUploadStorageConfig(t *testing.T) {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server()) // 一个桶都没有
	t.Cleanup(srv.Close)
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"写错的 driver", map[string]string{app.EnvUploadDriver: "oss"}, []string{app.EnvUploadDriver, "oss"}},
		{"对象存储缺配置", map[string]string{app.EnvUploadDriver: "s3", app.EnvS3Endpoint: srv.URL},
			[]string{app.EnvS3Bucket, app.EnvS3AccessKey}},
		{"桶不存在", map[string]string{app.EnvUploadDriver: "s3", app.EnvS3Endpoint: srv.URL, app.EnvS3Bucket: "keel-missing",
			app.EnvS3AccessKey: "ak", app.EnvS3SecretKey: "sk", app.EnvS3AddressStyle: "path"}, []string{"keel-missing"}},
		{"写错的地址风格", map[string]string{app.EnvUploadDriver: "s3", app.EnvS3Endpoint: srv.URL, app.EnvS3Bucket: "b",
			app.EnvS3AccessKey: "ak", app.EnvS3SecretKey: "sk", app.EnvS3AddressStyle: "vhost"}, []string{"vhost"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env(t, "", "example.com")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var s spy
			err := app.Run(context.Background(), s.listen)
			if err == nil {
				t.Fatal("应当拒绝启动")
			}
			if s.called {
				t.Fatal("拒绝启动之前已经开始监听")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("错误信息里没有 %q：%v", w, err)
				}
			}
		})
	}
}
