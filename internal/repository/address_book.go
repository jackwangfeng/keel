package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// SavedAddress 是地址簿里的一条（/addresses）。
//
// 与下单用的 Address（order.go）分开：那一个是「拍进订单快照的素材」，
// 刻意不带 is_default、tag 这些属于地址簿管理的字段（契约 ReceiverSnapshot 的
// 描述写着同一条）；这一个是地址簿本身，要带。共用一个类型的话，
// 快照那一侧迟早会被顺手多拷一列 tag 进去。
type SavedAddress struct {
	ID           int64
	ReceiverName string
	Phone        string
	Province     string
	City         string
	District     string
	Street       string
	Detail       string
	RegionCode   *string
	PostalCode   *string
	Tag          int16
	IsDefault    bool
	// Lat / Lng 是地址的坐标（WGS-84，00100）：搜索地点 / 地图选点时由客户端写入，手填与老地址为 nil。
	Lat, Lng  *float64
	CreatedAt time.Time
	UpdatedAt time.Time
	// InServiceArea 只由 ListAddressesForStore 填：那家门店送不送得到这条地址。
	// nil = 判断不了（门店有围栏而地址没坐标）。它是按门店算的，不是地址的属性，
	// 其余查询一律留 nil —— 契约 Address.in_service_area 也只在带 store_id 时给。
	InServiceArea *bool
}

// AddressFields 是新增与整体替换共用的那一组字段。IsDefault 只在新增时有意义
// （契约：「仅在新增时有效」），整体替换那条 SQL 根本不写那一列。
type AddressFields struct {
	ReceiverName string
	Phone        string
	Province     string
	City         string
	District     string
	Street       string
	Detail       string
	RegionCode   *string
	PostalCode   *string
	Tag          int16
	IsDefault    bool
	Lat, Lng     *float64
}

// ErrUserGone：锁买家行时发现这一行已经不在了（注销软删）。令牌还没过期，
// 人已经注销 —— access_token 是无状态的，这个窗口本来就存在（数据模型 §9 买家会话）。
var ErrUserGone = errors.New("买家账号已不存在")

// AddressBookTx 是地址簿这一面。
//
// **每个方法都带 userID**，而且那不是可选的过滤条件：RLS 只保证行属于本店，
// 「属于这个买家」只能由 user_id 回答（契约约定 4）。查不到 —— 含「是别人的」——
// 一律 ErrAddressNotFound，与下单时 FindAddress 同一个 sentinel、同一个语义。
type AddressBookTx interface {
	// ListAddresses 地址簿：默认在首，其余按更新时间倒序。
	ListAddresses(ctx context.Context, userID int64) ([]SavedAddress, error)
	// ListAddressesForStore 同 ListAddresses，另按 storeID 那家门店给每条填 InServiceArea
	// （判据与 StoreServesPoint 同一条）。门店不在时返回空集而不是错误 —— 调用方先问 StoreScope。
	ListAddressesForStore(ctx context.Context, userID, storeID int64) ([]SavedAddress, error)
	// FindSavedAddress 取一条。
	FindSavedAddress(ctx context.Context, id, userID int64) (SavedAddress, error)
	// InsertAddress 新增一条。f.IsDefault 为真时调用方必须已经 ClearDefaultAddress。
	InsertAddress(ctx context.Context, userID int64, f AddressFields) (SavedAddress, error)
	// ReplaceAddress 整体替换（不碰 is_default）。
	ReplaceAddress(ctx context.Context, id, userID int64, f AddressFields) (SavedAddress, error)
	// ClearDefaultAddress 清掉这个买家现有的默认，keepID 那一条除外（0 = 不排除）。
	ClearDefaultAddress(ctx context.Context, userID, keepID int64) error
	// MarkDefaultAddress 把一条置为默认。
	MarkDefaultAddress(ctx context.Context, id, userID int64) (SavedAddress, error)
	// SoftDeleteAddress 软删一条。
	SoftDeleteAddress(ctx context.Context, id, userID int64) error

	// LockUser 锁住这个买家的 users 行直到事务结束 —— 每个买家一把互斥锁，
	// 理由写在 db/queries/users.sql 的 LockUserRow 上。行不在了返回 ErrUserGone。
	LockUser(ctx context.Context, userID int64) error
}

// savedAddressOf 收口五条查询的行。五个 Row 类型字段一模一样（sqlc 按查询各生成
// 一个），Go 允许在字段完全相同的结构体之间直接转换，所以调用点写
// savedAddressOf(db.FindUserAddressRow(r))。哪天某条查询多 SELECT 了一列，
// 那个转换会当场编译失败 —— 那正是想要的：五条查询该返回同一个形状。
func savedAddressOf(r db.FindUserAddressRow) SavedAddress {
	return SavedAddress{
		ID:           r.ID,
		ReceiverName: r.ReceiverName,
		Phone:        r.Phone,
		Province:     r.Province,
		City:         r.City,
		District:     r.District,
		Street:       r.Street,
		Detail:       r.Detail,
		RegionCode:   r.RegionCode,
		PostalCode:   r.PostalCode,
		Tag:          r.Tag,
		IsDefault:    r.IsDefault,
		Lat:          r.Lat,
		Lng:          r.Lng,
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
}

func (t tenantTx) ListAddresses(ctx context.Context, userID int64) ([]SavedAddress, error) {
	rows, err := t.q.ListUserAddresses(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]SavedAddress, 0, len(rows))
	for _, r := range rows {
		out = append(out, savedAddressOf(db.FindUserAddressRow(r)))
	}
	return out, nil
}

func (t tenantTx) ListAddressesForStore(ctx context.Context, userID, storeID int64) ([]SavedAddress, error) {
	rows, err := t.q.ListUserAddressesForStore(ctx, db.ListUserAddressesForStoreParams{
		UserID: userID, StoreID: storeID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SavedAddress, 0, len(rows))
	for _, r := range rows {
		a := savedAddressOf(db.FindUserAddressRow{
			ID: r.ID, ReceiverName: r.ReceiverName, Phone: r.Phone, Province: r.Province,
			City: r.City, District: r.District, Street: r.Street, Detail: r.Detail,
			RegionCode: r.RegionCode, PostalCode: r.PostalCode, Tag: r.Tag, IsDefault: r.IsDefault,
			Lat: r.Lat, Lng: r.Lng, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
		// serves 为真（默认店 / 无围栏 / 在围栏内）就是 true，不看有没有坐标；
		// 为假时再分：有坐标 = 围栏外（false），没坐标 = 判断不了（nil）。
		// 这个三分与试算的 checkAddressInRange 一致：它对没坐标的地址不拦。
		switch {
		case r.Serves:
			a.InServiceArea = ptrBool(true)
		case r.HasPoint:
			a.InServiceArea = ptrBool(false)
		}
		out = append(out, a)
	}
	return out, nil
}

func ptrBool(v bool) *bool { return &v }

func (t tenantTx) FindSavedAddress(ctx context.Context, id, userID int64) (SavedAddress, error) {
	r, err := t.q.FindUserAddress(ctx, db.FindUserAddressParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return SavedAddress{}, ErrAddressNotFound
	}
	if err != nil {
		return SavedAddress{}, err
	}
	return savedAddressOf(r), nil
}

func (t tenantTx) InsertAddress(ctx context.Context, userID int64, f AddressFields) (SavedAddress, error) {
	r, err := t.q.InsertUserAddress(ctx, db.InsertUserAddressParams{
		UserID:       userID,
		ReceiverName: f.ReceiverName,
		Phone:        f.Phone,
		Province:     f.Province,
		City:         f.City,
		District:     f.District,
		Street:       f.Street,
		Detail:       f.Detail,
		RegionCode:   f.RegionCode,
		PostalCode:   f.PostalCode,
		Tag:          f.Tag,
		IsDefault:    f.IsDefault,
		Lat:          f.Lat,
		Lng:          f.Lng,
	})
	if err != nil {
		return SavedAddress{}, err
	}
	return savedAddressOf(db.FindUserAddressRow(r)), nil
}

func (t tenantTx) ReplaceAddress(ctx context.Context, id, userID int64, f AddressFields) (SavedAddress, error) {
	r, err := t.q.UpdateUserAddress(ctx, db.UpdateUserAddressParams{
		ReceiverName: f.ReceiverName,
		Phone:        f.Phone,
		Province:     f.Province,
		City:         f.City,
		District:     f.District,
		Street:       f.Street,
		Detail:       f.Detail,
		RegionCode:   f.RegionCode,
		PostalCode:   f.PostalCode,
		Tag:          f.Tag,
		Lat:          f.Lat,
		Lng:          f.Lng,
		ID:           id,
		UserID:       userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return SavedAddress{}, ErrAddressNotFound
	}
	if err != nil {
		return SavedAddress{}, err
	}
	return savedAddressOf(db.FindUserAddressRow(r)), nil
}

func (t tenantTx) ClearDefaultAddress(ctx context.Context, userID, keepID int64) error {
	return t.q.ClearDefaultAddress(ctx, db.ClearDefaultAddressParams{UserID: userID, KeepID: keepID})
}

func (t tenantTx) MarkDefaultAddress(ctx context.Context, id, userID int64) (SavedAddress, error) {
	r, err := t.q.MarkDefaultAddress(ctx, db.MarkDefaultAddressParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return SavedAddress{}, ErrAddressNotFound
	}
	if err != nil {
		return SavedAddress{}, err
	}
	return savedAddressOf(db.FindUserAddressRow(r)), nil
}

func (t tenantTx) SoftDeleteAddress(ctx context.Context, id, userID int64) error {
	_, err := t.q.SoftDeleteUserAddress(ctx, db.SoftDeleteUserAddressParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAddressNotFound
	}
	return err
}

func (t tenantTx) LockUser(ctx context.Context, userID int64) error {
	_, err := t.q.LockUserRow(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUserGone
	}
	return err
}
