package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 读文件（契约 GET /uploads/{upload_id}，M4 收尾）、买家上传（POST /uploads）、
// 后台读文件（GET /admin/uploads/{upload_id}）。
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
//	3 退款凭证  **仅上传者本人与后台客服** —— 不在这张表里。上传者本人带着自己的
//	            access_token 走 GET /uploads/{id}（ownerMayRead），后台客服走
//	            GET /admin/uploads/{id}（AdminRedirectTarget，按引用它的退款单判权）
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

// uploadBlobPrivateDomain 是**非公开**文件（退款凭证）限时地址的签名域。
//
// 与公开的那个分开，而不是在签名消息里多带一个标记：第二跳要能回答「这个地址是
// 按私有文件签出来的吗」，而这件事必须由签名本身证明。于是第二跳的规则是 ——
// 公开文件两种签名都认；私有文件只认私有域的签名。一个用途从公开改成私有时，
// 已经发出去的公开地址当场失效（原先那道「第二跳再查一次准入表」保护的正是这个）。
const uploadBlobPrivateDomain = "upload-blob-private"

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
		if !ownerMayRead(ctx, up) {
			return "", fmt.Errorf("%w: purpose=%d", ErrUploadForbidden, up.Purpose)
		}
		return s.blobLink(merchantID, up.ID, uploadBlobPrivateDomain), nil
	}
	return s.blobLink(merchantID, up.ID, uploadBlobDomain), nil
}

// ownerMayRead：这个请求带着上传者本人的买家身份。
//
// 身份来自 auth.OptionalBearer —— 没带令牌就没有身份（匿名），带了无效令牌在中间件
// 那一层已经 401 了，走不到这里。staff 上传的文件（user_id 为空）没有「本人」可言。
func ownerMayRead(ctx context.Context, up repository.Upload) bool {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return false
	}
	return up.UserID != nil && *up.UserID == id.UserID
}

// blobLink 签出一个 5 分钟后失效的第二跳地址。domain 决定它是公开的还是私有的
// （见 uploadBlobPrivateDomain）。
func (s *UploadService) blobLink(merchantID, uploadID int64, domain string) string {
	exp := s.now().UTC().Add(uploadBlobTTL).Unix()
	msg := blobMessage(merchantID, uploadID, exp)
	return fmt.Sprintf("%s%d/blob?exp=%d&sig=%s",
		uploadURLPrefix, uploadID, exp, s.signer.SignDetached(domain, msg))
}

// AdminRedirectTarget 是 GET /admin/uploads/{upload_id} 的第一跳：后台客服读文件。
//
// 商品图与头像本店员工都能读；退款凭证只认**被本店某张退款单引用了**的那些，
// 而且调用者要能看那张退款单（authorizeOrderStore，与后台退款单详情同一个判据）。
// 一张凭证被几张退款单引用（驳回后重新申请带着同一批图）时，能看其中任何一张就放行。
//
// 没被任何退款单引用的凭证（买家传了却没提交）一律 403：它与售后无关，
// 没有哪个员工有正当理由去看。
func (s *UploadService) AdminRedirectTarget(ctx context.Context, uploadID int64) (string, error) {
	if uploadID <= 0 {
		return "", fmt.Errorf("%w: upload_id 必须是正整数", ErrCatalogBadRequest)
	}
	if _, err := requireStaff(ctx); err != nil {
		return "", err
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return "", err
	}
	var up repository.Upload
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		if up, e = tx.FindUpload(ctx, uploadID); e != nil {
			return e
		}
		if publicPurposes[up.Purpose] {
			return nil
		}
		stores, e := tx.ListEvidenceRefundStores(ctx, UploadURL(up.ID))
		if e != nil {
			return e
		}
		if len(stores) == 0 {
			return fmt.Errorf("%w: upload %d 没有被本店任何一张退款单引用", ErrUploadForbidden, up.ID)
		}
		var lastErr error
		for _, st := range stores {
			if _, e := authorizeOrderStore(ctx, tx, st); e == nil {
				return nil
			} else {
				lastErr = e
			}
		}
		return lastErr
	}); err != nil {
		return "", err
	}
	if publicPurposes[up.Purpose] {
		return s.blobLink(merchantID, up.ID, uploadBlobDomain), nil
	}
	return s.blobLink(merchantID, up.ID, uploadBlobPrivateDomain), nil
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
	return s.BlobForWidth(ctx, uploadID, exp, sig, 0)
}

// RedirectTargetWidth 是带 ?w= 的第一跳：判权与 RedirectTarget 完全相同，限时地址后面带上归档后的 w。
// w 不进签名：它只决定缩略图的档位（已归到固定档），改它拿到的仍是同一个文件，不越权。
func (s *UploadService) RedirectTargetWidth(ctx context.Context, uploadID int64, w int) (string, error) {
	target, err := s.RedirectTarget(ctx, uploadID)
	if err != nil {
		return "", err
	}
	if w = SnapThumbWidth(w); w > 0 {
		target += "&w=" + strconv.Itoa(w)
	}
	return target, nil
}

// BlobForWidth 是 BlobFor 带缩略图档位的版本（upload_thumb.go）。w <= 0 即原图。
// 只对公开的两类（商品图、头像）出缩略图；私有文件（退款凭证）忽略 w，照给原图。
func (s *UploadService) BlobForWidth(ctx context.Context, uploadID int64, exp, sig string, w int) (UploadBlob, error) {
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
	msg := blobMessage(merchantID, uploadID, expUnix)
	privateSig := false
	if !s.signer.VerifyDetached(uploadBlobDomain, msg, sig) {
		if !s.signer.VerifyDetached(uploadBlobPrivateDomain, msg, sig) {
			return UploadBlob{}, fmt.Errorf("%w: 签名对不上", ErrUploadLinkInvalid)
		}
		privateSig = true
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
	// 准入表在这一跳也查一遍：非公开的文件只认私有域的签名（第一跳判过归属才签得出来）。
	// 签名是第一跳签的，所以理论上走不到这里；但「理论上走不到」的前提是签名那一段
	// 没被改过，而这一句的代价是一次 map 查询。一个用途从公开改成非公开时，
	// 已经发出去的那些公开地址应当立刻失效，而不是再活 5 分钟。
	if !publicPurposes[up.Purpose] && !privateSig {
		return UploadBlob{}, fmt.Errorf("%w: purpose=%d", ErrUploadForbidden, up.Purpose)
	}

	if s.store == nil {
		return UploadBlob{}, errors.New("没有配置文件存储 driver，GET /uploads/{upload_id} 不可用")
	}
	if w = SnapThumbWidth(w); w > 0 && publicPurposes[up.Purpose] {
		if blob, ok, err := s.thumbBlob(up.StorageKey, w); err != nil {
			return UploadBlob{}, err
		} else if ok {
			return blob, nil
		}
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

// ---------------------------------------------------------------------------
// 买家上传（契约 POST /uploads）
// ---------------------------------------------------------------------------

// scopeBuyerUploadCreate 是买家上传的幂等作用域。与后台那条（admin.uploads.create）
// 分开：主体不同（BuyerSubject / StaffSubject），作用域也不该共用一个名字。
const scopeBuyerUploadCreate = "uploads.create"

// buyerUploadFingerprint 是买家上传的 request_hash 素材：比后台那条多一个 purpose ——
// 同一张图先当头像传、再当退款凭证传，是两次不同的登记。
type buyerUploadFingerprint struct {
	Purpose   int16  `json:"purpose"`
	Mime      string `json:"mime"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// CreateBuyerUpload 实现 POST /uploads：买家传头像（2）或退款凭证（3）。
//
// 存储、类型与大小限制、「先落盘再抢幂等键」、重放 / 失败时删掉刚落盘的文件，
// 全部与后台那条（AdminCatalogService.CreateUpload）同一套 —— 同一个 UploadStore、
// 同一个 uploadExtensionFor、同一个 MaxUploadBytes、同一个 discardStoredUpload。
// 差别只有三处：上传者记 user_id；purpose 由请求给、只收 2 / 3；幂等主体是买家。
func (s *UploadService) CreateBuyerUpload(ctx context.Context, purpose int16, contentType string,
	body io.Reader, idemKey string) (repository.Upload, bool, error) {

	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.Upload{}, false, err
	}
	if purpose != repository.UploadPurposeAvatar && purpose != repository.UploadPurposeRefundProof {
		// 1 商品图是后台的事（POST /admin/uploads，上传者记 staff_id）；让买家传一张
		// 「商品图」，等于让它能被挂到商品上 —— 挂接那一步只核 purpose。
		return repository.Upload{}, false, fmt.Errorf("%w: purpose 只能是 2 头像或 3 退款凭证（商品图走后台上传）",
			ErrCatalogBadRequest)
	}
	if s.store == nil {
		return repository.Upload{}, false, errors.New("没有配置文件存储 driver，POST /uploads 不可用")
	}
	// 缺钥匙在读请求体之前就拒，理由同后台那条。
	if idemKey == "" {
		return repository.Upload{}, false, ErrIdempotencyKeyMissing
	}
	mime, ext, ok := uploadExtensionFor(contentType)
	if !ok {
		return repository.Upload{}, false, fmt.Errorf("%w: %q（只接受 image/jpeg、image/png、image/webp）",
			ErrUploadMediaType, contentType)
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return repository.Upload{}, false, err
	}
	key, size, sum, err := s.store.Put(merchantID, ext, body, MaxUploadBytes)
	if err != nil {
		return repository.Upload{}, false, err
	}
	hash, err := adminRequestHash(nil, buyerUploadFingerprint{Purpose: purpose, Mime: mime, SHA256: sum, SizeBytes: size})
	if err != nil {
		return repository.Upload{}, false, errors.Join(err, s.store.Remove(key))
	}
	out, replayed, err := idempotentTx(ctx, s.repo, repository.BuyerSubject(id.UserID),
		scopeBuyerUploadCreate, idemKey, hash, archivedCreated,
		func(tx repository.Tx) (repository.Upload, error) {
			return tx.CreateUserUpload(ctx, repository.NewUserUpload{
				UserID:      id.UserID,
				Purpose:     purpose,
				Driver:      s.store.Driver(),
				StorageKey:  key,
				ContentType: mime,
				SizeBytes:   size,
				SHA256:      sum,
			})
		})
	err = discardStoredUpload(ctx, s.store, key, err, replayed)
	return out, replayed, err
}

// discardStoredUpload 是「先落盘、再抢幂等键」那两条上传路径的善后：
// 重放或失败时把刚落盘的那个文件删掉，返回调用方该报的错误。
//
//   - 重放 —— 库里那一行指向的是**上一次**的 storage_key，这次写的这个永远不会有行
//     指向它，也就永远不会被孤儿回收看见（回收扫的是 uploads 表）。
//   - 失败 —— 事务回滚了，同理。
//
// 删不掉时：本来就要报错的，把善后失败一起带上去；**重放这一路只记日志，不改变调用
// 结果** —— 一个删不掉的残留文件不该让一次成功的重放变成 500，那会让客户端以为这次
// 重试失败了，于是换一把新钥匙再传一遍，磁盘上再多一份。
func discardStoredUpload(ctx context.Context, store UploadStore, key string, err error, replayed bool) error {
	if err == nil && !replayed {
		return nil
	}
	rmErr := store.Remove(key)
	if rmErr == nil {
		return err
	}
	if err != nil {
		return errors.Join(err, fmt.Errorf("删除孤儿文件 %s 失败: %w", key, rmErr))
	}
	slog.ErrorContext(ctx, "幂等重放后删除孤儿文件失败，它不会被孤儿回收看见",
		"storage_key", key, "err", rmErr)
	return nil
}
