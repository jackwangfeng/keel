package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/keel/keel/internal/service"
)

// 上传文件的存储（数据模型 §13）：本地磁盘（driver 1）或 S3 兼容的对象存储（driver 2）。
//
//	KEEL_UPLOAD_DRIVER=local（默认）  写到 KEEL_UPLOAD_ROOT 指的目录；单实例用。
//	KEEL_UPLOAD_DRIVER=s3             写到对象存储（KEEL_S3_*）；多实例、k8s、serverless 要它。
//	                                  KEEL_UPLOAD_ROOT 仍配着时，磁盘上的旧文件（driver 1 的行）照旧读得到——
//	                                  按行路由（service.UploadStorage），存量由 cmd/keel-uploads migrate 搬过去。
const (
	EnvUploadDriver      = "KEEL_UPLOAD_DRIVER"
	EnvS3Endpoint        = "KEEL_S3_ENDPOINT"
	EnvS3Region          = "KEEL_S3_REGION"
	EnvS3Bucket          = "KEEL_S3_BUCKET"
	EnvS3AccessKey       = "KEEL_S3_ACCESS_KEY"
	EnvS3SecretKey       = "KEEL_S3_SECRET_KEY"
	EnvS3Prefix          = "KEEL_S3_PREFIX"
	EnvS3AddressStyle    = "KEEL_S3_ADDRESS_STYLE"
	EnvS3PresignEndpoint = "KEEL_S3_PRESIGN_ENDPOINT"
)

// S3ConfigFromEnv 读 KEEL_S3_*（迁移工具 cmd/keel-uploads 也用它）。
func S3ConfigFromEnv() service.S3Config {
	return service.S3Config{
		Endpoint:        strings.TrimSpace(os.Getenv(EnvS3Endpoint)),
		Region:          strings.TrimSpace(os.Getenv(EnvS3Region)),
		Bucket:          strings.TrimSpace(os.Getenv(EnvS3Bucket)),
		AccessKey:       strings.TrimSpace(os.Getenv(EnvS3AccessKey)),
		SecretKey:       os.Getenv(EnvS3SecretKey),
		Prefix:          strings.TrimSpace(os.Getenv(EnvS3Prefix)),
		AddressStyle:    strings.TrimSpace(os.Getenv(EnvS3AddressStyle)),
		PresignEndpoint: strings.TrimSpace(os.Getenv(EnvS3PresignEndpoint)),
	}
}

// uploadStorageFromEnv 按环境变量建存储。对象存储连不上、桶不存在时报错（Run 据此拒绝启动）：
// 一个上传全部 500 的进程比一个起不来的进程难查得多。
func uploadStorageFromEnv(ctx context.Context) (service.UploadStore, error) {
	switch d := strings.ToLower(strings.TrimSpace(os.Getenv(EnvUploadDriver))); d {
	case "", "local":
		return uploadStoreFromEnv(), nil
	case "s3":
		cfg := S3ConfigFromEnv()
		if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, fmt.Errorf("%s=s3 要配 %s、%s、%s、%s", EnvUploadDriver, EnvS3Endpoint, EnvS3Bucket, EnvS3AccessKey, EnvS3SecretKey)
		}
		s3, err := service.NewS3Store(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("对象存储（%s）: %w", EnvUploadDriver, err)
		}
		var others []service.UploadStore
		if root := strings.TrimSpace(os.Getenv(EnvUploadRoot)); root != "" {
			others = append(others, service.NewLocalDiskStore(root))
			slog.InfoContext(ctx, "上传写到对象存储，磁盘上的旧文件照旧可读（迁完存量之后可以撤掉 "+EnvUploadRoot+"）",
				"s3", s3.Describe(), "local_root", root, "presign", cfg.PresignEndpoint != "")
		} else {
			slog.InfoContext(ctx, "上传写到对象存储", "s3", s3.Describe(), "presign", cfg.PresignEndpoint != "")
		}
		return service.NewUploadStorage(s3, others...)
	default:
		return nil, fmt.Errorf("%s=%q 不认识，只能是 local（默认）或 s3", EnvUploadDriver, d)
	}
}

// WithUploadStore 指定公网路由用的上传存储（Run 在启动时建好、校验过再交进来）。不给时按 KEEL_UPLOAD_ROOT
// 建本地磁盘 driver（测试与旧调用方）。
func WithUploadStore(s service.UploadStore) RouterOption {
	return func(o *routerOptions) { o.uploads = s }
}
