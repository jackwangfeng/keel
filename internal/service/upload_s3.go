package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 兼容的对象存储 driver（数据模型 §13 的 driver = 2）：AWS S3、阿里云 OSS、腾讯云 COS、MinIO……
// 主流对象存储都兼容 S3 协议，所以一个实现对接全部。
//
// # 为什么要它
//
// 本地磁盘 driver 把字节写在本进程挂的卷上，多实例部署时 A 实例收的图 B 实例读不到；
// 容器、机器重建还会丢（卷没挂好的时候）。对象存储是所有实例共用的一份，按用量付费，还能直接接 CDN。
//
// # 与本地磁盘 driver 逐条对齐的地方
//
//   - storage_key 同一个形状（newStorageKey：商家前缀 + 两字符散列目录 + 随机名），桶里的对象名 = 前缀 + storage_key。
//     所以存量从磁盘迁过来（cmd/keel-uploads migrate）key 一个字都不变，只把 uploads.driver 从 1 改成 2。
//   - Put 先判大小再写：读进内存（上限 10 MB，契约的单文件上限）边读边算 sha256，超了直接拒、桶里什么都不留。
//     不走「大小未知」的分片上传：那样超限要等写到一半才知道，还得回头删。
//   - key 的形状检查与本地 driver 同一道（storageKeyValid）：key 来自库里，但换 driver、迁存量时来源会变。
//   - 缩略图缓存在同一个桶的 thumbs/ 下（ThumbCache），原图删了跟着删。
//
// # 预签名（可选）
//
// 配了对外地址（S3Config.PresignEndpoint）时，GET /uploads/{id} 的第一跳直接 302 到对象存储的预签名地址
// （5 分钟，与站内第二跳同一个寿命），字节不再经过应用——图片流量由对象存储 / CDN 扛。没配时字节照旧由
// 应用第二跳代为读出（桶可以完全私有、不对外）。归属校验永远在第一跳，与 driver 无关。

// S3Config 是 S3 兼容 driver 的配置。
type S3Config struct {
	// Endpoint 形如 http://minio:9000、https://oss-cn-shenzhen.aliyuncs.com、https://s3.amazonaws.com。
	// 协议决定是否 TLS。
	Endpoint  string
	Region    string // 空用 us-east-1（MinIO 与多数兼容实现不在乎；OSS / COS 要填对应地域）
	Bucket    string
	AccessKey string
	SecretKey string
	// Prefix 是桶里的对象名前缀（如 keel/），多个部署共用一个桶时分开。可空。
	Prefix string
	// AddressStyle 是桶怎么出现在地址里：path = http://host/bucket/key（SeaweedFS、MinIO 等自建的要），
	// virtual = http://bucket.host/key（OSS、COS 要），auto（空也是）= 按地址自动判（AWS、阿里云、腾讯云等
	// 已知域名走 virtual，其余走 path）。
	AddressStyle string
	// PresignEndpoint 是浏览器能访问到的对象存储地址；非空时第一跳直接 302 到它上面的预签名地址。
	PresignEndpoint string
}

// S3Store 是 S3 兼容的 driver。
type S3Store struct {
	c       *minio.Client
	presign *minio.Client // 签对外地址用；nil = 不预签名
	bucket  string
	prefix  string
}

var (
	_ UploadStore = (*S3Store)(nil)
	_ ThumbCache  = (*S3Store)(nil)
	_ Presigner   = (*S3Store)(nil)
)

func newS3Client(endpoint string, cfg S3Config) (*minio.Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(endpoint), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("对象存储地址 %q 不是 http(s)://host[:port] 形式", endpoint)
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	lookup := minio.BucketLookupAuto
	switch strings.ToLower(cfg.AddressStyle) {
	case "", "auto":
	case "path":
		lookup = minio.BucketLookupPath
	case "virtual":
		lookup = minio.BucketLookupDNS
	default:
		return nil, fmt.Errorf("桶地址风格 %q 不认识，只能是 auto / path / virtual", cfg.AddressStyle)
	}
	return minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       u.Scheme == "https",
		Region:       region, // 显式给地域：预签名是离线计算，不给的话 minio-go 会先发一次请求问桶在哪
		BucketLookup: lookup,
	})
}

// NewS3Store 建 driver 并确认桶存在、凭据可用（启动时就失败，而不是第一次上传时）。
func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("对象存储 driver 要桶名、AccessKey 与 SecretKey")
	}
	c, err := newS3Client(cfg.Endpoint, cfg)
	if err != nil {
		return nil, err
	}
	s := &S3Store{c: c, bucket: cfg.Bucket, prefix: strings.TrimLeft(cfg.Prefix, "/")}
	if cfg.PresignEndpoint != "" {
		if s.presign, err = newS3Client(cfg.PresignEndpoint, cfg); err != nil {
			return nil, fmt.Errorf("预签名地址: %w", err)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ok, err := c.BucketExists(cctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("连对象存储 %s 失败（地址、凭据、地域）: %w", cfg.Endpoint, err)
	}
	if !ok {
		return nil, fmt.Errorf("对象存储里没有桶 %q：先建好（MinIO: mc mb；云厂商在控制台建），Keel 不替你建——桶的地域、权限、生命周期是运维要定的", cfg.Bucket)
	}
	return s, nil
}

func (*S3Store) Driver() int16 { return 2 }

// Describe 给启动日志用。
func (s *S3Store) Describe() string {
	return "s3://" + s.bucket + "/" + s.prefix + "（" + s.c.EndpointURL().String() + "）"
}

func (s *S3Store) object(key string) string { return s.prefix + key }

func (s *S3Store) thumbObject(key string, w int) string {
	return s.prefix + "thumbs/" + key + ".w" + strconv.Itoa(w) + ".jpg"
}

// Put 实现 UploadStore。
func (s *S3Store) Put(merchantID int64, ext string, r io.Reader, limit int64) (string, int64, string, error) {
	key, err := newStorageKey(merchantID, ext)
	if err != nil {
		return "", 0, "", err
	}
	var buf bytes.Buffer
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(&buf, h), io.LimitReader(r, limit+1))
	if err != nil {
		return "", 0, "", err
	}
	if n > limit {
		return "", 0, "", ErrUploadTooLarge
	}
	if n == 0 {
		return "", 0, "", fmt.Errorf("%w: 文件是空的", ErrCatalogBadRequest)
	}
	if err := s.PutObject(key, bytes.NewReader(buf.Bytes()), n); err != nil {
		return "", 0, "", err
	}
	return key, n, hex.EncodeToString(h.Sum(nil)), nil
}

// PutObject 按给定的 storage_key 写入（迁存量用：key 沿用本地 driver 的那一个）。
func (s *S3Store) PutObject(key string, r io.Reader, size int64) error {
	if !storageKeyValid(key) {
		return fmt.Errorf("storage_key %q 的形状不合法", key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := s.c.PutObject(ctx, s.bucket, s.object(key), r, size, minio.PutObjectOptions{})
	return err
}

// Open 实现 UploadStore。先 Stat：GetObject 是懒的，不存在要等第一次 Read 才报，而那时已经开始回响应了。
func (s *S3Store) Open(key string) (io.ReadCloser, error) {
	if !storageKeyValid(key) {
		return nil, fmt.Errorf("%w: storage_key %q 的形状不合法", ErrUploadBlobMissing, key)
	}
	ctx := context.Background()
	if _, err := s.c.StatObject(ctx, s.bucket, s.object(key), minio.StatObjectOptions{}); err != nil {
		if isNoSuchKey(err) {
			return nil, fmt.Errorf("%w: %s", ErrUploadBlobMissing, key)
		}
		return nil, err
	}
	return s.c.GetObject(ctx, s.bucket, s.object(key), minio.GetObjectOptions{})
}

// Remove 实现 UploadStore（不存在当成功；缩略图跟着删）。
func (s *S3Store) Remove(key string) error {
	if !storageKeyValid(key) {
		return fmt.Errorf("storage_key %q 的形状不合法，拒绝删除", key)
	}
	ctx := context.Background()
	if err := s.c.RemoveObject(ctx, s.bucket, s.object(key), minio.RemoveObjectOptions{}); err != nil && !isNoSuchKey(err) {
		return err
	}
	for _, w := range ThumbWidths {
		if err := s.c.RemoveObject(ctx, s.bucket, s.thumbObject(key, w), minio.RemoveObjectOptions{}); err != nil && !isNoSuchKey(err) {
			return err
		}
	}
	return nil
}

// OpenThumb 实现 ThumbCache；不存在返回 os.ErrNotExist。
func (s *S3Store) OpenThumb(key string, w int) (io.ReadCloser, int64, error) {
	if !storageKeyValid(key) {
		return nil, 0, os.ErrNotExist
	}
	ctx := context.Background()
	st, err := s.c.StatObject(ctx, s.bucket, s.thumbObject(key, w), minio.StatObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, 0, os.ErrNotExist
		}
		return nil, 0, err
	}
	rc, err := s.c.GetObject(ctx, s.bucket, s.thumbObject(key, w), minio.GetObjectOptions{})
	return rc, st.Size, err
}

// PutThumb 实现 ThumbCache。对象存储的 PUT 本来就是原子的（读的一方看不到半个对象）。
func (s *S3Store) PutThumb(key string, w int, data []byte) error {
	if !storageKeyValid(key) {
		return fmt.Errorf("缩略图 key %q 不合法", key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.c.PutObject(ctx, s.bucket, s.thumbObject(key, w), bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "image/jpeg"})
	return err
}

// Presigner 是 driver 可选的「给浏览器一个直连地址」能力（GET /uploads/{id} 第一跳用）。
type Presigner interface {
	// PresignGet 返回 key（w > 0 时是那一档缩略图）的限时直连地址。ok = false 表示这一次不预签名
	// （没配对外地址、缩略图还没生成），调用方照旧走站内第二跳。
	PresignGet(key string, w int, contentType string, ttl time.Duration) (u string, ok bool, err error)
}

// PresignGet 实现 Presigner。缩略图要先确认已经缓存过：没生成的那一档得走站内第二跳现算。
func (s *S3Store) PresignGet(key string, w int, contentType string, ttl time.Duration) (string, bool, error) {
	if s.presign == nil || !storageKeyValid(key) {
		return "", false, nil
	}
	obj := s.object(key)
	if w > 0 {
		obj = s.thumbObject(key, w)
		if _, err := s.c.StatObject(context.Background(), s.bucket, obj, minio.StatObjectOptions{}); err != nil {
			if isNoSuchKey(err) {
				return "", false, nil
			}
			return "", false, err
		}
		contentType = "image/jpeg"
	}
	params := url.Values{}
	if contentType != "" {
		params.Set("response-content-type", contentType)
	}
	u, err := s.presign.PresignedGetObject(context.Background(), s.bucket, obj, ttl, params)
	if err != nil {
		return "", false, err
	}
	return u.String(), true, nil
}

func isNoSuchKey(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.StatusCode == 404
	}
	return false
}
