package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 商家管理（契约 GET /admin/merchants、GET/PATCH /admin/merchants/{id}）与
// 单商家部署下的开店闸门。
//
// 一个单独的服务而不是挂在 StaffService 上：它要知道一件 StaffService 不知道、
// 也不该知道的事——**这套部署是单商家还是多商家**（KEEL_DEFAULT_MERCHANT）。
// 开店本身仍然是 StaffService.OpenShop（建管理员、签登录 token 那三步与
// CreateStaff 逐字相同，理由在 merchant.go 的文件头）；这里只在它前面加一道闸。
//
// 第二道闸在同一层：自有域名的登记（PATCH 的 domain）。它要拒的是两种
// 「登记了也进不去」——单商家部署里 Host 根本不参与解析，基础域名之下的名字
// 解析器只认 code。两种都不是数据错误，是部署形态与请求内容的错，
// 所以闸门在服务层而不是在库里。

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

// ErrMerchantDomainTaken 同理（契约 409 merchant-domain-taken）。
//
// 它不在这里判定，也不在这里查重：撞车是 merchant_domains.domain 上那条全表 UNIQUE
// 报的 23505，由 repository 分出来（先查后写在两家店同时登记同一个域名时两边都会
// 通过检查，而数据库只让一边提交）。这里放一个别名只为 handler 不必 import repository。
var ErrMerchantDomainTaken = repository.ErrMerchantDomainTaken

// MerchantDirectoryRepository 是本服务用到的 repository 方法。
type MerchantDirectoryRepository interface {
	ListMerchants(ctx context.Context, limit, offset int64) ([]repository.Merchant, int64, error)
	GetMerchant(ctx context.Context, id int64) (repository.Merchant, error)
	ReviseMerchant(ctx context.Context, id int64, in repository.MerchantEdit, changedBy int64) (repository.Merchant, error)
}

type MerchantAdminService struct {
	repo  MerchantDirectoryRepository
	staff *StaffService
	// defaultCode 非空即单商家部署，与 tenant.Config.DefaultCode 同一个值
	// （都来自 KEEL_DEFAULT_MERCHANT，由 internal/app 一处读出、两处传入）。
	defaultCode string
	// baseDomain 是 KEEL_BASE_DOMAIN，同样由 internal/app 一处读出、两处传入
	// （另一处是 tenant.Config.BaseDomain，走的是同一个归一化）。
	//
	// 这里要它只为一件事：拒绝「登记在基础域名之下」的域名。那条规矩的**判据**
	// 属于解析器（tenant.DomainUnderBase 与 Resolve 用的是同一个表达式），
	// 而这一层的配置值必须与那一层是同一个值——各读一遍环境变量的话，
	// 「这里放行、解析器不采纳」就重新出现了，而那正是这条闸门存在的理由。
	baseDomain string
}

func NewMerchantAdminService(r MerchantDirectoryRepository, staff *StaffService,
	defaultCode, baseDomain string) *MerchantAdminService {

	return &MerchantAdminService{
		repo:        r,
		staff:       staff,
		defaultCode: strings.TrimSpace(defaultCode),
		baseDomain:  tenant.NormalizeDomain(baseDomain),
	}
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

// Update 实现 PATCH /admin/merchants/{id}：改名、停用、启用、登记或摘掉自有域名。
// 平台级管理员。
//
// 单商家部署里有三种修改会让下一次启动失败或变成空话，全部拒绝：
//
//   - 停用默认商家 → 活跃商家变成 0 家（Preflight：默认商家不存在或已停用）
//   - 启用另一家   → 活跃商家变成 2 家（Preflight：单商家模式下有多家活跃商家）
//   - 动 domain    → 单商家模式下 Host 完全不参与解析（Resolve 第一行就返回默认商家），
//     登记的域名永远不会被采纳 —— 那种行不报错、不生效，只让运营以为配好了
//
// 改名不受影响。
func (s *MerchantAdminService) Update(ctx context.Context, merchantID int64,
	in repository.MerchantEdit) (repository.Merchant, error) {

	id, err := requirePlatform(ctx, true)
	if err != nil {
		return repository.Merchant{}, err
	}
	touchesDomain := in.Domain != nil || in.ClearDomain
	if in.Name == nil && in.Status == nil && !touchesDomain {
		return repository.Merchant{}, fmt.Errorf("%w: 一个字段都没传", ErrStaffBadRequest)
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			return repository.Merchant{}, fmt.Errorf("%w: name 不能为空", ErrStaffBadRequest)
		}
		in.Name = &n
	}
	if in.Status != nil && *in.Status != 1 && *in.Status != 2 {
		return repository.Merchant{}, fmt.Errorf("%w: status 只能是 1 或 2", ErrStaffBadRequest)
	}

	if touchesDomain {
		if s.SingleMerchantMode() {
			return repository.Merchant{}, fmt.Errorf("%w（KEEL_DEFAULT_MERCHANT=%s，单商家模式不看 Host）",
				ErrSingleMerchantMode, s.defaultCode)
		}
		if in.Domain != nil {
			d := tenant.NormalizeDomain(*in.Domain)
			switch {
			case d == "" || !tenant.ValidDomain(d):
				return repository.Merchant{}, fmt.Errorf("%w: domain %q 不是一个合法域名（至少两段，每段是合法 DNS 标签）",
					ErrStaffBadRequest, *in.Domain)
			case tenant.DomainUnderBase(d, s.baseDomain):
				return repository.Merchant{}, fmt.Errorf(
					"%w: domain %q 落在平台基础域名 %s 之下——那片地盘的名字归平台，"+
						"解析器在那里只认 code，这条登记永远不会被采纳。请改这家店的 code",
					ErrStaffBadRequest, d, s.baseDomain)
			}
			in.Domain = &d
		}
	}

	if in.Status != nil && s.SingleMerchantMode() {
		cur, err := s.repo.GetMerchant(ctx, merchantID)
		if err != nil {
			return repository.Merchant{}, err
		}
		isDefault := cur.Code == s.defaultCode
		if (isDefault && *in.Status == 2) || (!isDefault && *in.Status == 1 && cur.Status != 1) {
			return repository.Merchant{}, fmt.Errorf("%w（商家 %s，目标状态 %d）",
				ErrSingleMerchantMode, cur.Code, *in.Status)
		}
	}
	return s.repo.ReviseMerchant(ctx, merchantID, in, id.StaffID)
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
