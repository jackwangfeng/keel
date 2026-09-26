package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// UploadTx 是文件元数据这一面（数据模型 §13）。
//
// 只有后台那条上传路径在这里。C 端的 POST /uploads 填的是 user_id，
// 与这条填 staff_id 的是**两条路**，不是一条路的两种用法：哪一列被填上取决于
// 这次调用带的是买家 token 还是 staff 会话 token，而 chk_upload_owner 这条
// CHECK 约束把「二选一」钉死在库里。同一个入口同时接受两种身份，等于把
// 「这次是谁传的」变成一个要靠 token 形状猜的东西。
type UploadTx interface {
	// CreateStaffUpload 登记一个后台操作员上传的商品图。
	// purpose 固定为 1 商品图，不是入参 —— 路径已经决定了它。
	CreateStaffUpload(ctx context.Context, n NewUpload) (Upload, error)

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
