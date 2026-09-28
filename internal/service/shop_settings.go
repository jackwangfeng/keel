package service

import (
	"context"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 店铺设置（契约 GET / PUT /admin/shop-settings，00059 的 shop_preferences）。
//
// 只有管理员能读能改（requireMerchantAdmin，与设默认门店同一行）：自动确认天数与退货寄回
// 时限改的是全店每一单的时效，时区改的是全部报表的切天口径。
//
// 店铺名称只读地回显（merchants 目录的当前名称，平台经 merchant_revisions 改）。
//
// # 改了之后什么时候生效
//
// 读它的地方都是「每次用的时候现读」，没有缓存：报表每个请求读一次时区，
// 自动确认与退货超时的定时任务每一轮每家店读一次天数，订单详情与售后单每次读一次。
// 所以改完的下一个请求、下一轮扫描（至多十分钟）就按新值算，不需要重启。

// ShopSettingsRepository 是本服务需要的仓储能力。GetMerchant 走商家目录（平台那一半），
// 店铺名称从那里来。
type ShopSettingsRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	GetMerchant(ctx context.Context, id int64) (repository.Merchant, error)
}

// ShopSettingsService 实现店铺设置的两条接口。
type ShopSettingsService struct{ repo ShopSettingsRepository }

// NewShopSettingsService 建店铺设置服务。
func NewShopSettingsService(r ShopSettingsRepository) *ShopSettingsService {
	return &ShopSettingsService{repo: r}
}

// ShopSettings 是 GET / PUT 的响应。
type ShopSettings struct {
	ShopName string
	repository.ShopPreferences
}

// ShopSettingsInput 是 PUT 的请求体。ServicePhone 为 nil 即清空（整体替换）。
type ShopSettingsInput struct {
	ServicePhone    *string
	Timezone        string
	AutoConfirmDays int
	ReturnShipDays  int
	// AfterSaleDays 为 nil 即默认 15（契约里它是可选的，老客户端整体替换不带它）。
	AfterSaleDays *int
}

// 契约 ShopSettingsInput 的边界。天数的边界与 00059 的两条 CHECK 一致。
const (
	shopSettingsMinDays    = 1
	shopSettingsMaxDays    = 365
	shopServicePhoneMaxLen = 32
	shopTimezoneMaxLen     = 64
)

// Get 实现 GET /admin/shop-settings。
func (s *ShopSettingsService) Get(ctx context.Context) (ShopSettings, error) {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return ShopSettings{}, err
	}
	var p repository.ShopPreferences
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		p, err = tx.ShopPreferences(ctx)
		return err
	}); err != nil {
		return ShopSettings{}, err
	}
	return s.withName(ctx, p)
}

// Replace 实现 PUT /admin/shop-settings（整体替换，天然幂等）。
func (s *ShopSettingsService) Replace(ctx context.Context, in ShopSettingsInput) (ShopSettings, error) {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return ShopSettings{}, err
	}
	var c fieldChecker
	p := repository.ShopPreferences{
		Timezone:        strings.TrimSpace(in.Timezone),
		AutoConfirmDays: in.AutoConfirmDays,
		ReturnShipDays:  in.ReturnShipDays,
		AfterSaleDays:   repository.DefaultAfterSaleDays,
	}
	if in.AfterSaleDays != nil {
		p.AfterSaleDays = *in.AfterSaleDays
	}
	if !validShopTimezone(p.Timezone) {
		c.add("timezone", "不是能加载的 IANA 时区名（如 Asia/Shanghai）")
	}
	if p.AutoConfirmDays < shopSettingsMinDays || p.AutoConfirmDays > shopSettingsMaxDays {
		c.add("auto_confirm_days", "只能是 1 到 365 之间的整数")
	}
	if p.ReturnShipDays < shopSettingsMinDays || p.ReturnShipDays > shopSettingsMaxDays {
		c.add("return_ship_days", "只能是 1 到 365 之间的整数")
	}
	if p.AfterSaleDays < shopSettingsMinDays || p.AfterSaleDays > shopSettingsMaxDays {
		c.add("after_sale_days", "只能是 1 到 365 之间的整数")
	}
	if in.ServicePhone != nil {
		// 空串与全空白按契约 minLength 1 是 422，不悄悄当成「清空」：清空的写法是不给这个字段。
		phone := c.text("service_phone", *in.ServicePhone, true, shopServicePhoneMaxLen)
		p.ServicePhone = &phone
	}
	if err := c.err(); err != nil {
		return ShopSettings{}, err
	}
	var out repository.ShopPreferences
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		out, err = tx.ReplaceShopPreferences(ctx, p)
		return err
	}); err != nil {
		return ShopSettings{}, err
	}
	return s.withName(ctx, out)
}

func (s *ShopSettingsService) withName(ctx context.Context, p repository.ShopPreferences) (ShopSettings, error) {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return ShopSettings{}, err
	}
	m, err := s.repo.GetMerchant(ctx, merchantID)
	if err != nil {
		return ShopSettings{}, err
	}
	return ShopSettings{ShopName: m.Name, ShopPreferences: p}, nil
}

// validShopTimezone 判一个时区名能不能写进店铺设置。
//
// 判据比报表读它时的 reportLocation 严一格：那边读到坏值回落默认，这边写入口直接挡。
// 能 time.LoadLocation（tzdata 编进了二进制，与部署机器无关）、不是空串、不含空白、
// 不是 Local —— Local 在 Go 里是「服务器所在时区」，写进去之后报表的「今天」随部署机器变。
// 「CST」这类缩写在 tzdata 里不存在，LoadLocation 本来就拒绝。
func validShopTimezone(name string) bool {
	if name == "" || len(name) > shopTimezoneMaxLen || strings.EqualFold(name, "local") ||
		strings.ContainsAny(name, " \t\r\n") {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
