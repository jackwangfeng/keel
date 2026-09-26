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
