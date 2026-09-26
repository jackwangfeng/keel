package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 商家管理（契约 GET /admin/merchants、GET/PATCH /admin/merchants/{id}）与
// 单商家部署下的开店闸门。
//
// 一个单独的服务而不是挂在 StaffService 上：它要知道一件 StaffService 不知道、
// 也不该知道的事——**这套部署是单商家还是多商家**（KEEL_DEFAULT_MERCHANT）。
// 开店本身仍然是 StaffService.OpenShop（建管理员、签登录 token 那三步与
// CreateStaff 逐字相同，理由在 merchant.go 的文件头）；这里只在它前面加一道闸。

// ErrSingleMerchantMode：单商家部署里开店，或者做了会让它下次起不来的状态修改
// （契约 409 single-merchant-mode）。
//
// 单商家部署忽略 Host，所有请求都落在默认商家上；tenant.Resolver.Preflight
// 规定此时活跃商家必须恰好一家，否则拒绝启动。所以在这里多开一家店的后果不是
// 「多了一家访问不到的店」而已——是**这套部署下一次重启就起不来**，而那一刻
// 离点下「开店」的人可能已经隔了几天。
var ErrSingleMerchantMode = errors.New("单商家部署不能有第二家活跃商家")

// ErrMerchantNotFound 是 repository.ErrMerchantNotFound 的别名，handler 按它翻 404。
var ErrMerchantNotFound = repository.ErrMerchantNotFound

// MerchantDirectoryRepository 是本服务用到的 repository 方法。
type MerchantDirectoryRepository interface {
	ListMerchants(ctx context.Context, limit, offset int64) ([]repository.Merchant, int64, error)
	GetMerchant(ctx context.Context, id int64) (repository.Merchant, error)
	ReviseMerchant(ctx context.Context, id int64, name *string, status *int16, changedBy int64) (repository.Merchant, error)
}

type MerchantAdminService struct {
	repo  MerchantDirectoryRepository
	staff *StaffService
	// defaultCode 非空即单商家部署，与 tenant.Config.DefaultCode 同一个值
	// （都来自 KEEL_DEFAULT_MERCHANT，由 internal/app 一处读出、两处传入）。
	defaultCode string
}

func NewMerchantAdminService(r MerchantDirectoryRepository, staff *StaffService, defaultCode string) *MerchantAdminService {
	return &MerchantAdminService{repo: r, staff: staff, defaultCode: strings.TrimSpace(defaultCode)}
}

// SingleMerchantMode 回答「这套部署能不能再开店」。契约 MerchantList.single_merchant_mode。
func (s *MerchantAdminService) SingleMerchantMode() bool { return s.defaultCode != "" }

// MerchantPage 是商家列表那一页。
type MerchantPage struct {
	Items    []repository.Merchant
	Total    int64
	Page     int
	PageSize int
}

// requirePlatform：平台级（任意角色）。商家目录是这个部署的全貌，不是任何一家店该看见的。
func requirePlatform(ctx context.Context, admin bool) (auth.StaffIdentity, error) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if !id.Platform() || (admin && !id.IsAdmin()) {
		return auth.StaffIdentity{}, fmt.Errorf("%w（当前身份：platform=%v role=%d）",
			ErrPlatformOnly, id.Platform(), id.Role)
	}
	return id, nil
}

// List 实现 GET /admin/merchants：含停用与待审核，不含软删。
func (s *MerchantAdminService) List(ctx context.Context, page, pageSize int) (MerchantPage, error) {
	if _, err := requirePlatform(ctx, false); err != nil {
		return MerchantPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	items, total, err := s.repo.ListMerchants(ctx, int64(pageSize), offsetOf(page, pageSize))
	if err != nil {
		return MerchantPage{}, err
	}
	return MerchantPage{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// Get 实现 GET /admin/merchants/{id}。
func (s *MerchantAdminService) Get(ctx context.Context, merchantID int64) (repository.Merchant, error) {
	if _, err := requirePlatform(ctx, false); err != nil {
		return repository.Merchant{}, err
	}
	return s.repo.GetMerchant(ctx, merchantID)
}

// Update 实现 PATCH /admin/merchants/{id}：改名、停用、启用。平台级管理员。
//
// 单商家部署里有两种状态修改会让下一次启动失败，都拒绝：
//
//   - 停用默认商家 → 活跃商家变成 0 家（Preflight：默认商家不存在或已停用）
//   - 启用另一家   → 活跃商家变成 2 家（Preflight：单商家模式下有多家活跃商家）
//
// 改名不受影响。
func (s *MerchantAdminService) Update(ctx context.Context, merchantID int64, name *string, status *int16) (repository.Merchant, error) {
	id, err := requirePlatform(ctx, true)
	if err != nil {
		return repository.Merchant{}, err
	}
	if name == nil && status == nil {
		return repository.Merchant{}, fmt.Errorf("%w: 一个字段都没传", ErrStaffBadRequest)
	}
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" {
			return repository.Merchant{}, fmt.Errorf("%w: name 不能为空", ErrStaffBadRequest)
		}
		name = &n
	}
	if status != nil && *status != 1 && *status != 2 {
		return repository.Merchant{}, fmt.Errorf("%w: status 只能是 1 或 2", ErrStaffBadRequest)
	}

	if status != nil && s.SingleMerchantMode() {
		cur, err := s.repo.GetMerchant(ctx, merchantID)
		if err != nil {
			return repository.Merchant{}, err
		}
		isDefault := cur.Code == s.defaultCode
		if (isDefault && *status == 2) || (!isDefault && *status == 1 && cur.Status != 1) {
			return repository.Merchant{}, fmt.Errorf("%w（商家 %s，目标状态 %d）",
				ErrSingleMerchantMode, cur.Code, *status)
		}
	}
	return s.repo.ReviseMerchant(ctx, merchantID, name, status, id.StaffID)
}

// OpenShop 实现 POST /admin/merchants：先过单商家闸门，再交给 StaffService.OpenShop。
//
// 身份判断排在单商家判断前面：一个商家级员工调这条接口该收到的是
// 「你不是平台管理员」，而不是「这套部署是单商家的」——后者对他是一条
// 与他无关、还泄露了部署形态的消息。
//
// 第二个返回值为 true 表示这是一次幂等重放（StaffService.OpenShop）。
func (s *MerchantAdminService) OpenShop(ctx context.Context, code, name, adminEmail, idemKey string) (ShopOpened, bool, error) {
	if _, err := requirePlatform(ctx, true); err != nil {
		return ShopOpened{}, false, err
	}
	if s.SingleMerchantMode() {
		return ShopOpened{}, false, fmt.Errorf("%w（KEEL_DEFAULT_MERCHANT=%s）", ErrSingleMerchantMode, s.defaultCode)
	}
	return s.staff.OpenShop(ctx, code, name, adminEmail, idemKey)
}
