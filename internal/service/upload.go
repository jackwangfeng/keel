package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 读文件（契约 GET /uploads/{upload_id}）。M4 收尾。
//
// ===========================================================================
// 为什么是两跳，而不是直接把字节吐出来
// ===========================================================================
//
// 契约把这条接口定成 **302**，跳到「driver 生成的限时地址」：本地磁盘 driver
// 跳到带签名与过期时间的站内地址，S3 driver 跳到预签名 URL。
//
// 这不是绕远路，它是这条接口存在的全部理由的另一半。§13 建 uploads 这张表
// 的第一条理由是**归属校验**：退款凭证是隐私内容，而 URL 会被转发、被日志
// 记录、被截图。如果第一跳直接吐字节，那么「归属校验」发生在一次性的那一跳
// 上，而真正会被复制出去的是那个地址本身 —— 也就是说校验拦住的人，
// 只要拿到别人转发的链接就照样能看。
//
// 两跳把这件事拆开了：
//
//	第一跳（本文件的 RedirectTarget）  判归属，回一个**有过期时间**的地址
//	第二跳（BlobFor）                 只认签名与过期时间，不判归属
//
// 于是被转发出去的那个地址自带一个 5 分钟的寿命，而契约里写死的那个形状
// （/api/v1/uploads/{id}）永远是要过归属校验的那一个。
//
// 换 S3 时变的只有第二跳：UploadStore.Open 改成返回预签名地址，第一跳一个字
// 都不用动 —— 那正是 §13「部署形态是配置，不是重写」在这一层的兑现。

// publicPurposes 是「所有人可读」的那些用途，逐条对应契约描述里那张表：
//
//	1 商品图    所有人（上架商品的图本就公开）
//	2 头像      所有人
//	3 退款凭证  **仅上传者本人与后台客服** —— 不在这张表里
//
// **默认是拒绝**：表里没有的用途一律 403。反过来写（列出要拦的那些）的话，
// 将来新增一个用途（比如「营业执照」）会默认公开，而那条改动看上去只是
// 在枚举里加了一个值。
//
// 取值用 repository 那一组常量，不在这里再写一遍 1/2：那一组是 purpose 这一列
// 的唯一真相源，而两份枚举分叉的那天，分叉的那一项会变成一个准入判断。
var publicPurposes = map[int16]bool{
	repository.UploadPurposeProductImage: true,
	repository.UploadPurposeAvatar:       true,
}

// ErrUploadForbidden：这个文件不是「所有人可读」的那一类（契约那个 403）。
//
// 契约刻意要求 403 而不是 404，原话：「文件 id 是自增的，用 404 掩盖存在性
// 并不能阻止枚举，反而让合法用户分不清『没权限』和『传错了 id』」。
//
// **它与跨租户不是一回事。** 别家店的文件在 RLS 之下根本读不出来，
// 那条路上返回的是 repository.ErrUploadNotFound → 404，与
// 「这个 id 不存在」同一个响应 —— 那一条反而必须掩盖存在性，
// 理由见 repository.FindUpload 上的注释。
var ErrUploadForbidden = errors.New("无权读取该文件")

// ErrUploadLinkInvalid：第二跳的签名不对、过期了，或者被改过。
var ErrUploadLinkInvalid = errors.New("文件访问地址无效或已过期")

// uploadBlobTTL 是第二跳那个地址的寿命。契约：「限时地址，默认有效期 5 分钟」。
const uploadBlobTTL = 5 * time.Minute

// uploadBlobDomain 是分离签名的域（auth.Signer.SignDetached）。
const uploadBlobDomain = "upload-blob"

// UploadBlob 是第二跳要交给 handler 的东西：一段字节和怎么把它写出去。
type UploadBlob struct {
	ContentType string
	SizeBytes   int64

	// Body 由 handler 负责 Close。
	Body io.ReadCloser
}

// UploadRepository 是读文件这条路需要的仓储能力。
//
// **只有 WithTenant。** 这条接口是买家侧的（没有 /admin/ 前缀），租户由请求
// 的 Host 定出来，而「一个租户的人不能读到另一个租户的文件」正是靠这个作用域
// 加 uploads 上那条 RLS 策略给出的 —— 不是靠这一层再比一次 merchant_id
// （那会让 RLS 有没有生效变得测不出来，见 scripts/check_query_tenancy.py）。
type UploadRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// UploadService 实现 GET /uploads/{upload_id} 那两跳。
type UploadService struct {
	repo   UploadRepository
	store  UploadStore
	signer *auth.Signer
	now    func() time.Time
}

func NewUploadService(r UploadRepository, store UploadStore, signer *auth.Signer) *UploadService {
	return &UploadService{repo: r, store: store, signer: signer, now: time.Now}
}

// WithClock 换掉时钟，**只给测试用**（同 auth.Signer.WithClock）。
//
// 「限时地址真的会过期」这件事在 handler 那一层验不了：唯一的办法是等 5 分钟，
// 而一条要等 5 分钟的测试等于一条不会被跑的测试。把 exp 改小再打过去也不行 ——
// 改了 exp 签名就对不上，于是那条断言验的是签名而不是过期。
// 所以这个钩子存在，而它守着的是一条真的会被绕过的规则。
func (s *UploadService) WithClock(now func() time.Time) *UploadService {
	out := *s
	out.now = now
	return &out
}

// RedirectTarget 是第一跳：判归属，返回一个限时的站内地址。
//
// 三条出路，逐条对应契约里那三个响应：
//
//	找不到 / 是别家店的  → repository.ErrUploadNotFound  → 404
//	用途不在准入表里      → ErrUploadForbidden           → 403
//	其余                  → 一个 5 分钟后失效的地址       → 302
func (s *UploadService) RedirectTarget(ctx context.Context, uploadID int64) (string, error) {
	if uploadID <= 0 {
		return "", fmt.Errorf("%w: upload_id 必须是正整数", ErrCatalogBadRequest)
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return "", err
	}

	var up repository.Upload
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		up, e = tx.FindUpload(ctx, uploadID)
		return e
	}); err != nil {
		return "", err
	}

	if !publicPurposes[up.Purpose] {
		return "", fmt.Errorf("%w: purpose=%d", ErrUploadForbidden, up.Purpose)
	}

	exp := s.now().UTC().Add(uploadBlobTTL).Unix()
	msg := blobMessage(merchantID, up.ID, exp)
	return fmt.Sprintf("%s%d/blob?exp=%d&sig=%s",
		uploadURLPrefix, up.ID, exp, s.signer.SignDetached(uploadBlobDomain, msg)), nil
}

// BlobFor 是第二跳：只认签名与过期时间，然后把字节交出来。
//
// ===========================================================================
// 它**仍然**走 WithTenant 再读一次那一行，而那不是重复劳动
// ===========================================================================
//
// 签名里带着 merchant_id，所以「拿 A 店签出来的地址去 B 店的 Host 上打」
// 已经被签名挡住了一次。这里再走一次 RLS 是第二道，挡的是另一件事：
// 签名密钥泄露、或者签名那一段哪天被人写坏。两道闸门失守的方式不一样 ——
// 前者靠密钥，后者靠数据库，而这条路上一次失守的后果是别家店的文件被读走。
//
// 顺带它还是唯一拿得到 content_type 与 size_bytes 的地方：
// 那两样不进签名（它们会让地址变长、也会让「改一下文件元数据」变成
// 「所有已发出的地址全部失效」）。
func (s *UploadService) BlobFor(ctx context.Context, uploadID int64, exp, sig string) (UploadBlob, error) {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return UploadBlob{}, err
	}
	expUnix, err := strconv.ParseInt(strings.TrimSpace(exp), 10, 64)
	if err != nil {
		return UploadBlob{}, fmt.Errorf("%w: exp 不是一个时间戳", ErrUploadLinkInvalid)
	}
	// 先验签再看过期：反过来的话，一个过期时间被改成很远的未来的地址会先
	// 通过过期检查，然后才在验签上被拒 —— 结论一样，但「先证明这串东西是
	// 我们签的」在读起来时才是对的顺序。
	if !s.signer.VerifyDetached(uploadBlobDomain, blobMessage(merchantID, uploadID, expUnix), sig) {
		return UploadBlob{}, fmt.Errorf("%w: 签名对不上", ErrUploadLinkInvalid)
	}
	if s.now().UTC().Unix() > expUnix {
		return UploadBlob{}, fmt.Errorf("%w: 地址已于 %s 过期", ErrUploadLinkInvalid,
			time.Unix(expUnix, 0).UTC().Format(time.RFC3339))
	}

	var up repository.Upload
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		up, e = tx.FindUpload(ctx, uploadID)
		return e
	}); err != nil {
		return UploadBlob{}, err
	}
	// 准入表在这一跳也查一遍。签名是第一跳签的，所以理论上走不到这里；
	// 但「理论上走不到」的前提是签名那一段没被改过，而这一句的代价是一次
	// map 查询。一个用途从公开改成非公开时，已经发出去的那些地址应当立刻
	// 失效，而不是再活 5 分钟。
	if !publicPurposes[up.Purpose] {
		return UploadBlob{}, fmt.Errorf("%w: purpose=%d", ErrUploadForbidden, up.Purpose)
	}

	if s.store == nil {
		return UploadBlob{}, errors.New("没有配置文件存储 driver，GET /uploads/{upload_id} 不可用")
	}
	body, err := s.store.Open(up.StorageKey)
	if err != nil {
		return UploadBlob{}, err
	}
	return UploadBlob{ContentType: up.ContentType, SizeBytes: up.SizeBytes, Body: body}, nil
}

// blobMessage 是被签的那段字节。
//
// 三样都必须在里面：
//
//	merchant_id  不带的话，A 店签出来的地址在 B 店的 Host 上也验得过 ——
//	             而 upload id 是全局自增的，B 店只要猜到编号就能读走 A 店的图。
//	upload_id    不带的话，一个地址能读任何文件。
//	exp          不带的话，「限时」两个字不成立。
//
// 分隔符用冒号而不是直接拼：三个都是十进制整数，不拼分隔符的话
// (1, 23, 4) 与 (12, 3, 4) 是同一串字节。
func blobMessage(merchantID, uploadID, exp int64) string {
	return strconv.FormatInt(merchantID, 10) + ":" +
		strconv.FormatInt(uploadID, 10) + ":" +
		strconv.FormatInt(exp, 10)
}
