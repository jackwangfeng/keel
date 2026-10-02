package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// UploadTx 是文件元数据这一面（数据模型 §13）。
//
// 后台那条上传路径（CreateStaffUpload，填 staff_id）与 C 端的 POST /uploads
// （CreateUserUpload，填 user_id）是**两条路**，不是一条路的两种用法：哪一列被填上取决于
// 这次调用带的是买家 token 还是 staff 会话 token，而 chk_upload_owner 这条
// CHECK 约束把「二选一」钉死在库里。同一个入口同时接受两种身份，等于把
// 「这次是谁传的」变成一个要靠 token 形状猜的东西。
type UploadTx interface {
	// CreateStaffUpload 登记一个后台操作员上传的商品图。
	// purpose 固定为 1 商品图，不是入参 —— 路径已经决定了它。
	CreateStaffUpload(ctx context.Context, n NewUpload) (Upload, error)
	// CreateChannelUpload 登记一张渠道适配层从商品源下载的商品图（上传者是 binding，00302）。
	CreateChannelUpload(ctx context.Context, n NewChannelUpload) (Upload, error)

	// CreateUserUpload 登记一个 C 端买家上传的头像或退款凭证（契约 POST /uploads）。
	// purpose 只收 2 / 3，其余返回错误 —— 商品图走 CreateStaffUpload。
	CreateUserUpload(ctx context.Context, n NewUserUpload) (Upload, error)

	// ListEvidenceRefundStores 引用了这个凭证地址的退款单各自所属订单的履约门店（去重）。
	// 后台读退款凭证按它判权。
	ListEvidenceRefundStores(ctx context.Context, url string) ([]int64, error)

	// FindUpload 取一条文件元数据。查不到（不存在，或被 RLS 挡在租户外）
	// 返回 ErrUploadNotFound。
	FindUpload(ctx context.Context, id int64) (Upload, error)

	// MarkUploadReferenced 把 referenced 置为 TRUE。
	//
	// **它必须与引用这个文件的那次业务写入在同一个事务里**（数据模型 §13 明写）。
	// 否则存在这样的窗口：业务对象刚提交、孤儿回收恰好扫到、文件被删，
	// 而页面上那张图已经是 404。跑在 WithTenant 的事务里这件事由结构保证。
	//
	// 商品图那一路（ReplaceProductImages）在包内直接调 q，不经过这里；
	// 这个方法是给 skus.image_url 那一路用的 —— 建 / 改 SKU 时传
	// image_upload_id，服务端据此写出 image_url，那同样是一次「业务对象引用
	// 了一个文件」。少了这一句，那张规格小图会在 24 小时后被孤儿回收删掉，
	// 而 skus.image_url 还指着它。
	//
	// 幂等：已经是 TRUE 时再调一次什么也不改。
	MarkUploadReferenced(ctx context.Context, id int64) error

	// UnmarkUploadReferenced 取消引用：只动 userID 自己传的头像（purpose = 2）。
	// 头像换掉之后旧头像走这里，之后由孤儿回收处理。不是这个买家的头像时什么也不改、不报错 ——
	// 旧的 avatar_url 可能是这一版之前写进去的外链或别人的地址，那不是「取消引用」该管的事。
	UnmarkUploadReferenced(ctx context.Context, id, userID int64) error

	// ListOrphanUploads 孤儿回收的候选：没被引用、创建早于 cutoff、这个 driver 的文件，
	// 按创建时间从早到晚，至多 limit 条。只是预筛。
	ListOrphanUploads(ctx context.Context, driver int16, cutoff time.Time, limit int32) ([]OrphanUpload, error)

	// DeleteOrphanUpload 删一条孤儿记录，返回它的 storage_key。谓词在行锁之下重判「没被引用、
	// 早于 cutoff」，不再成立（刚被引用了）时返回 ok = false、什么也不删。
	DeleteOrphanUpload(ctx context.Context, id int64, cutoff time.Time) (storageKey string, ok bool, err error)

	// UploadsByDriver 是存量迁移（cmd/keel-uploads）的翻页：本店写在 driver 上、id 大于 afterID 的文件，至多 limit 条。
	UploadsByDriver(ctx context.Context, driver int16, afterID int64, limit int32) ([]StoredUpload, error)
	// MoveUploadDriver 把一行从 from 改指到 to（字节已经拷过去并核对过）。返回 false = 这一行已经不在 from 上了。
	MoveUploadDriver(ctx context.Context, id int64, from, to int16) (bool, error)
}

// OrphanUpload 是孤儿回收扫到的一条文件记录。
type OrphanUpload struct {
	ID         int64
	StorageKey string
}

// NewUpload 是登记一个上传的入参。
//
// 没有 Purpose：走这条路的一律是 1 商品图。给它一个参数位，等于让调用方能把
// 一张退款凭证登记成商品图，而 chk_upload_owner 只管上传者二选一，管不了这个。
//
// 没有 MerchantID：租户由列默认值 current_merchant() 填。
type NewUpload struct {
	StaffID     int64
	Driver      int16
	StorageKey  string
	ContentType string
	SizeBytes   int64
	SHA256      string
}

func (t tenantTx) CreateStaffUpload(ctx context.Context, n NewUpload) (Upload, error) {
	if n.SizeBytes <= 0 {
		// chk_upload_size 也会拦，但它给出的是 23514，而那条错误里没有任何
		// 东西指向「文件是空的」。契约把空文件与超限分别定成 422 / 413，
		// 两者都要在服务层说得出话。
		return Upload{}, fmt.Errorf("文件大小 %d 必须为正", n.SizeBytes)
	}
	r, err := t.q.CreateStaffUpload(ctx, db.CreateStaffUploadParams{
		StaffID:     &n.StaffID,
		Purpose:     UploadPurposeProductImage,
		Driver:      n.Driver,
		StorageKey:  n.StorageKey,
		ContentType: n.ContentType,
		SizeBytes:   n.SizeBytes,
		Sha256:      n.SHA256,
	})
	if err != nil {
		return Upload{}, err
	}
	// 两个查询的列清单逐字相同，所以两种行类型的字段名、顺序、类型也相同，
	// Go 允许它们之间直接转换。哪天有人动了其中一边的 SELECT，这一行会**编译
	// 失败** —— 那正是想要的：两条查询回传的形状不一样时，要有人来决定怎么办。
	return uploadFrom(db.GetUploadRow(r)), nil
}

// NewChannelUpload 是登记一张渠道适配层从商品源下载的商品图的入参（00302）。
type NewChannelUpload struct {
	BindingID   int64
	Driver      int16
	StorageKey  string
	ContentType string
	SizeBytes   int64
	SHA256      string
}

func (t tenantTx) CreateChannelUpload(ctx context.Context, n NewChannelUpload) (Upload, error) {
	if n.SizeBytes <= 0 {
		return Upload{}, fmt.Errorf("文件大小 %d 必须为正", n.SizeBytes)
	}
	r, err := t.q.CreateChannelUpload(ctx, db.CreateChannelUploadParams{
		ChannelBindingID: &n.BindingID,
		Driver:           n.Driver,
		StorageKey:       n.StorageKey,
		ContentType:      n.ContentType,
		SizeBytes:        n.SizeBytes,
		Sha256:           n.SHA256,
	})
	if err != nil {
		return Upload{}, err
	}
	return uploadFrom(db.GetUploadRow(r)), nil
}

// NewUserUpload 是登记一个买家上传的入参。与 NewUpload 分开而不是加一个 UserID 字段：
// 一个结构上同时有 StaffID 与 UserID 的入参，等于把 chk_upload_owner 那条「二选一」
// 交给调用方去记得。
type NewUserUpload struct {
	UserID      int64
	Purpose     int16
	Driver      int16
	StorageKey  string
	ContentType string
	SizeBytes   int64
	SHA256      string
}

func (t tenantTx) CreateUserUpload(ctx context.Context, n NewUserUpload) (Upload, error) {
	if n.Purpose != UploadPurposeAvatar && n.Purpose != UploadPurposeRefundProof {
		return Upload{}, fmt.Errorf("买家上传的 purpose 只能是 2 头像或 3 退款凭证，得到 %d", n.Purpose)
	}
	if n.SizeBytes <= 0 {
		return Upload{}, fmt.Errorf("文件大小 %d 必须为正", n.SizeBytes)
	}
	r, err := t.q.CreateUserUpload(ctx, db.CreateUserUploadParams{
		UserID:      &n.UserID,
		Purpose:     n.Purpose,
		Driver:      n.Driver,
		StorageKey:  n.StorageKey,
		ContentType: n.ContentType,
		SizeBytes:   n.SizeBytes,
		Sha256:      n.SHA256,
	})
	if err != nil {
		return Upload{}, err
	}
	return uploadFrom(db.GetUploadRow(r)), nil
}

func (t tenantTx) ListEvidenceRefundStores(ctx context.Context, url string) ([]int64, error) {
	return t.q.ListEvidenceRefundStores(ctx, url)
}

func (t tenantTx) FindUpload(ctx context.Context, id int64) (Upload, error) {
	r, err := t.q.GetUpload(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		// 「不存在」与「是别家的」合成同一个错误，理由与 ErrProductNotFound
		// 那一条一样：upload id 是自增的，两者一旦分开报，这个接口就成了一个
		// 能数出别家店传了多少文件的探测器。
		return Upload{}, fmt.Errorf("upload %d: %w", id, ErrUploadNotFound)
	}
	if err != nil {
		return Upload{}, err
	}
	return uploadFrom(r), nil
}

// uploadFrom 把 sqlc 的行收成领域类型。两处各写一遍的话，写岔一处的症状是
// 某一条路径上 referenced 恒为 false，而孤儿回收会在 24 小时后把刚用上的图删掉。
func uploadFrom(r db.GetUploadRow) Upload {
	return Upload{
		ID: r.ID, UserID: r.UserID, StaffID: r.StaffID,
		Purpose: r.Purpose, Driver: r.Driver, StorageKey: r.StorageKey,
		ContentType: r.ContentType, SizeBytes: r.SizeBytes, SHA256: r.Sha256,
		Referenced: r.Referenced, CreatedAt: r.CreatedAt.Time,
	}
}

func (t tenantTx) MarkUploadReferenced(ctx context.Context, id int64) error {
	n, err := t.q.MarkUploadReferenced(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		// 0 行只可能是这个 id 不在本租户视野内（RLS）或者根本不存在。
		// 调用方应当先 FindUpload 过一遍，所以走到这里说明那一步漏了 ——
		// 静默返回 nil 会让「文件被标记过了」成为一句假话，
		// 而它的症状是 24 小时后那张图消失。
		return fmt.Errorf("upload %d: %w", id, ErrUploadNotFound)
	}
	return nil
}

func (t tenantTx) UnmarkUploadReferenced(ctx context.Context, id, userID int64) error {
	_, err := t.q.UnmarkUploadReferenced(ctx, db.UnmarkUploadReferencedParams{ID: id, UserID: &userID})
	return err
}

func (t tenantTx) ListOrphanUploads(ctx context.Context, driver int16, cutoff time.Time,
	limit int32) ([]OrphanUpload, error) {
	rows, err := t.q.ListOrphanUploads(ctx, db.ListOrphanUploadsParams{
		Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true}, Driver: driver, PageLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]OrphanUpload, 0, len(rows))
	for _, r := range rows {
		out = append(out, OrphanUpload{ID: r.ID, StorageKey: r.StorageKey})
	}
	return out, nil
}

func (t tenantTx) DeleteOrphanUpload(ctx context.Context, id int64, cutoff time.Time) (string, bool, error) {
	key, err := t.q.DeleteOrphanUpload(ctx, db.DeleteOrphanUploadParams{
		ID: id, Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// StoredUpload 是迁移要的那几列。
type StoredUpload struct {
	ID          int64
	StorageKey  string
	ContentType string
	SizeBytes   int64
	SHA256      string
}

func (t tenantTx) UploadsByDriver(ctx context.Context, driver int16, afterID int64, limit int32) ([]StoredUpload, error) {
	rows, err := t.q.ListUploadsByDriver(ctx, db.ListUploadsByDriverParams{Driver: driver, AfterID: afterID, PageLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]StoredUpload, 0, len(rows))
	for _, r := range rows {
		out = append(out, StoredUpload{ID: r.ID, StorageKey: r.StorageKey, ContentType: r.ContentType,
			SizeBytes: r.SizeBytes, SHA256: r.Sha256})
	}
	return out, nil
}

func (t tenantTx) MoveUploadDriver(ctx context.Context, id int64, from, to int16) (bool, error) {
	n, err := t.q.MoveUploadDriver(ctx, db.MoveUploadDriverParams{ID: id, FromDriver: from, ToDriver: to})
	return n == 1, err
}
