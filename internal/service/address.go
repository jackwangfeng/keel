package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 地址簿（/addresses，数据模型 §9 user_addresses）。
//
// # 越权：查不到即 404，「别人的」与「不存在的」同形
//
// 地址对外用自增 id（契约约定 4、数据模型 §1），越权防护靠的是每一条 SQL 都带
// user_id = 当前买家（db/queries/addresses.sql 文件头）。这一层不再判一次
// 「这一行是不是你的」—— 判据只有一份，在 SQL 里；这里拿到的 ErrAddressNotFound
// 已经同时覆盖了两种情形。
//
// # 默认地址：唯一性由服务端事务负责
//
// 「每个买家至多一个默认地址」的执行者是 uk_user_addresses_default 那个部分唯一
// 索引。服务端的职责是**别让客户端看到它**：切换默认要在同一事务里先清旧、再置新，
// 并且事务开头先锁住这个买家的 users 行（LockUser）—— 不锁的话，两个并发的
// 「设为默认」各自清旧时都看不见对方还没提交的新默认，第二个置新时撞上唯一索引，
// 客户端拿到 500。契约把切换收进专用接口，理由原话就是这个。

// AddressRepository 是本服务需要的仓储能力。
type AddressRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// AddressService 实现 /addresses 的 6 个操作。
type AddressService struct{ repo AddressRepository }

func NewAddressService(r AddressRepository) *AddressService { return &AddressService{repo: r} }

// scopeAddressCreate 是 POST /addresses 在 idempotency_keys 里的作用域。
const scopeAddressCreate = "addresses.create"

var (
	// ErrUseDefaultEndpoint：试图用 PUT /addresses/{id} 把一条非默认地址改成默认。
	// 契约：422 use-default-endpoint，切换默认请走 PUT /addresses/{id}/default。
	ErrUseDefaultEndpoint = errors.New("请用设为默认地址的专用接口切换默认地址")
)

// FieldProblem 是一条字段级校验错误（契约的 FieldError）。
type FieldProblem struct {
	Field   string
	Message string
}

// InvalidFieldsError：请求体里有字段不合法。handler 翻成 422 + Problem.errors。
//
// 一次报全而不是撞到第一个就返回：表单页要把每个出错的输入框都标红，
// 只报第一个会让用户改一个、提交、再看到下一个。
type InvalidFieldsError struct{ Fields []FieldProblem }

func (e *InvalidFieldsError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "字段校验失败: " + strings.Join(parts, "; ")
}

// fieldChecker 攒字段错误。
type fieldChecker struct{ fields []FieldProblem }

func (c *fieldChecker) add(field, msg string) {
	c.fields = append(c.fields, FieldProblem{Field: field, Message: msg})
}

func (c *fieldChecker) err() error {
	if len(c.fields) == 0 {
		return nil
	}
	return &InvalidFieldsError{Fields: c.fields}
}

// text 校验一个文本字段并返回去掉首尾空白后的值。长度按字符（码点）数，
// 与契约的 maxLength 同一个口径（JSON Schema 的 maxLength 数的是码点，不是字节）。
func (c *fieldChecker) text(field, v string, required bool, max int) string {
	v = strings.TrimSpace(v)
	switch {
	case required && v == "":
		c.add(field, "不能为空")
	case utf8.RuneCountInString(v) > max:
		c.add(field, fmt.Sprintf("不能超过 %d 个字", max))
	}
	return v
}

// optText 同上，但字段可空：空串或全空白记为 nil（列是可空的，存一个空串
// 会让「没填」与「填了个空」在库里成为两种状态）。
func (c *fieldChecker) optText(field string, v *string, max int, allowed func(rune) bool) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > max {
		c.add(field, fmt.Sprintf("不能超过 %d 个字符", max))
		return nil
	}
	if allowed != nil && strings.IndexFunc(s, func(r rune) bool { return !allowed(r) }) >= 0 {
		c.add(field, "含有不允许的字符")
		return nil
	}
	return &s
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isPhoneRune(r rune) bool { return isDigit(r) || r == '+' || r == '-' || r == ' ' }

// 字段上限。契约写了的照契约（receiver_name 32、detail 200），没写的给一个
// 宽松的上限 —— 不设上限的文本字段是一个谁都能往库里塞一兆字节的口子，
// 而这张表的每一行都会被拍进订单快照。
const (
	maxReceiverName = 32
	maxAddrDetail   = 200
	maxAddrArea     = 64 // province / city / district / street
	maxAddrPhone    = 20
	maxRegionCode   = 12 // GB/T 2260 六位，加乡镇街道的扩展码到 12 位
	maxPostalCode   = 10
)

// AddressInput 是契约 AddressInput 在 service 边界上的形状。
type AddressInput struct {
	ReceiverName string
	Phone        string
	Province     string
	City         string
	District     string
	Street       *string
	Detail       string
	RegionCode   *string
	PostalCode   *string
	Tag          *int
	IsDefault    *bool
	// Lat / Lng：地址的坐标（WGS-84，00100），两个都给或都不给。PUT 是整体替换：不给就清掉。
	Lat, Lng *float64
}

// normalize 校验并收成仓储那一层的字段。
func (in AddressInput) normalize() (repository.AddressFields, error) {
	var c fieldChecker
	f := repository.AddressFields{
		ReceiverName: c.text("receiver_name", in.ReceiverName, true, maxReceiverName),
		Province:     c.text("province", in.Province, true, maxAddrArea),
		City:         c.text("city", in.City, true, maxAddrArea),
		District:     c.text("district", in.District, true, maxAddrArea),
		Detail:       c.text("detail", in.Detail, true, maxAddrDetail),
	}
	f.Phone = c.text("phone", in.Phone, true, maxAddrPhone)
	if f.Phone != "" && strings.IndexFunc(f.Phone, func(r rune) bool { return !isPhoneRune(r) }) >= 0 {
		c.add("phone", "只能包含数字、加号、连字符与空格")
	}
	if in.Street != nil {
		f.Street = c.text("street", *in.Street, false, maxAddrArea)
	}
	f.RegionCode = c.optText("region_code", in.RegionCode, maxRegionCode, isDigit)
	f.PostalCode = c.optText("postal_code", in.PostalCode, maxPostalCode, nil)
	if in.Tag != nil {
		if *in.Tag < 0 || *in.Tag > 3 {
			c.add("tag", "只能是 0 / 1 / 2 / 3")
		} else {
			f.Tag = int16(*in.Tag)
		}
	}
	f.IsDefault = in.IsDefault != nil && *in.IsDefault
	switch {
	case in.Lat == nil && in.Lng == nil:
	case in.Lat == nil || in.Lng == nil:
		c.add("lat", "lat 与 lng 要么都给要么都不给")
	case *in.Lat < -90 || *in.Lat > 90 || *in.Lng < -180 || *in.Lng > 180:
		c.add("lat", "坐标越界（WGS-84：纬度 -90–90、经度 -180–180）")
	default:
		f.Lat, f.Lng = in.Lat, in.Lng
	}
	return f, c.err()
}

// List 实现 GET /addresses。
//
// storeID 非 nil 时每条地址多标一个 InServiceArea（契约 Address.in_service_area）：
// 结算页据此自动挑一条那家门店送得到的地址。门店不存在 / 已软删 / 别家店 →
// ErrStoreNotFound（422），与 GET /cart 的 store_id 同一个约定，不静默当作没传。
// 停业的门店不拦：围栏还在，标记照样有意义；能不能下单由试算说。
func (s *AddressService) List(ctx context.Context, storeID *int64) ([]repository.SavedAddress, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	var out []repository.SavedAddress
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if storeID == nil {
			out, err = tx.ListAddresses(ctx, id.UserID)
			return err
		}
		// 先问门店：ListAddressesForStore 在门店不在时是空集，与「地址簿是空的」分不开。
		if _, _, err := tx.StoreScope(ctx, *storeID); err != nil {
			if errors.Is(err, repository.ErrCatalogNotFound) {
				return fmt.Errorf("%w: store_id=%d", ErrStoreNotFound, *storeID)
			}
			return err
		}
		out, err = tx.ListAddressesForStore(ctx, id.UserID, *storeID)
		return err
	})
	return out, err
}

// Get 实现 GET /addresses/{address_id}。
func (s *AddressService) Get(ctx context.Context, addressID int64) (repository.SavedAddress, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.SavedAddress{}, err
	}
	var out repository.SavedAddress
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		out, err = tx.FindSavedAddress(ctx, addressID, id.UserID)
		return err
	})
	return out, err
}

// Create 实现 POST /addresses。返回的 bool 为真表示这是一次幂等重放。
//
// is_default=true 时在同一事务里先清旧默认、再插新（契约原话），并先锁住买家行。
// 抢幂等键、清旧、插新、存档全在一个事务里：任何一步失败都整体回滚，
// 客户端拿同一把钥匙原样重试即可（与领券同一个形状，coupon.go 的 idempotentTenantWrite）。
func (s *AddressService) Create(ctx context.Context, in AddressInput, idemKey string) (repository.SavedAddress, bool, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.SavedAddress{}, false, err
	}
	f, err := in.normalize()
	if err != nil {
		return repository.SavedAddress{}, false, err
	}
	hash, err := adminRequestHash(nil, f)
	if err != nil {
		return repository.SavedAddress{}, false, err
	}
	return idempotentTenantWrite(ctx, s.repo, scopeAddressCreate, repository.BuyerSubject(id.UserID),
		idemKey, hash, archivedCreated, func(tx repository.Tx) (repository.SavedAddress, error) {
			if f.IsDefault {
				if err := tx.LockUser(ctx, id.UserID); err != nil {
					return repository.SavedAddress{}, err
				}
				if err := tx.ClearDefaultAddress(ctx, id.UserID, 0); err != nil {
					return repository.SavedAddress{}, err
				}
			}
			return tx.InsertAddress(ctx, id.UserID, f)
		})
}

// Replace 实现 PUT /addresses/{address_id}（整体替换）。
//
// is_default 在这条接口上**不生效**（契约 AddressInput.is_default：「仅在新增时有效」），
// 唯一的例外是它写明的那一条：收到 true 且当前不是默认 → 422 use-default-endpoint。
// 收到 false 而当前是默认时不取消默认：那同样是在用本接口切换默认，
// 而契约只给了「设为默认」一个动作，没有「取消默认」。
func (s *AddressService) Replace(ctx context.Context, addressID int64, in AddressInput) (repository.SavedAddress, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.SavedAddress{}, err
	}
	f, err := in.normalize()
	if err != nil {
		return repository.SavedAddress{}, err
	}
	var out repository.SavedAddress
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		cur, err := tx.FindSavedAddress(ctx, addressID, id.UserID)
		if err != nil {
			return err
		}
		if f.IsDefault && !cur.IsDefault {
			return ErrUseDefaultEndpoint
		}
		out, err = tx.ReplaceAddress(ctx, addressID, id.UserID, f)
		return err
	})
	return out, err
}

// Delete 实现 DELETE /addresses/{address_id}（软删）。
//
// 删掉的若是默认地址，**不自动把别的某一条扶成默认**：「至多一个」不是「恰好一个」
// （数据模型 §9），而替用户挑一个新默认等于替他决定下一单寄到哪。
func (s *AddressService) Delete(ctx context.Context, addressID int64) error {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.SoftDeleteAddress(ctx, addressID, id.UserID)
	})
}

// SetDefault 实现 PUT /addresses/{address_id}/default。
//
// 互斥切换：锁买家行 → 确认这一条是你的 → 清掉别的默认 → 置这一条。
// 对已是默认的地址重复调用返回 200（契约：幂等），清旧那一步因为排除了它自己，
// 什么都不改。
func (s *AddressService) SetDefault(ctx context.Context, addressID int64) (repository.SavedAddress, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.SavedAddress{}, err
	}
	var out repository.SavedAddress
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.LockUser(ctx, id.UserID); err != nil {
			return err
		}
		// 先确认归属再清旧：顺序反过来时，一个指向别人地址的请求会先把自己的默认
		// 清掉、再在置新那一步 404 —— 事务回滚会把清掉的恢复，结果仍然对，
		// 但那是靠回滚兜住的，读的人要多想一步。
		if _, err := tx.FindSavedAddress(ctx, addressID, id.UserID); err != nil {
			return err
		}
		if err := tx.ClearDefaultAddress(ctx, id.UserID, addressID); err != nil {
			return err
		}
		out, err = tx.MarkDefaultAddress(ctx, addressID, id.UserID)
		return err
	})
	return out, err
}
