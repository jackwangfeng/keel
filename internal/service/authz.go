package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 同一租户内的分级权限（v0.1.0，迁移 00025，数据模型 §14 staff_scopes）。
//
// ===========================================================================
// 这个文件是全部判据，别处一个 if 都不写
// ===========================================================================
//
// 后台每一条写接口在 service 里**恰好调一个** authorize* / require* 函数，
// 读接口要么调 requireStaff（商品目录全量可读）、要么调一个 authorize*
// （单个门店 / 大区）、要么拿 listFilter 收窄列表。handler 里没有任何权限判断，
// 也没有 SQL（CONTRIBUTING 的规矩一）。
//
// 这么收拢的理由是可测性：internal/handler 的 TestAdminPermissionMatrix 把全部
// /admin/ 路由乘以各个角色逐格断言，而「每个接口的判据是哪一个函数」必须是
// 一件看一眼就知道的事 —— 散在各处的 if 写错一个，那张表会红，但红的那一格
// 指不回是哪一行代码。
//
// ===========================================================================
// 与租户隔离是两层，不是一层
// ===========================================================================
//
// 租户之间由 RLS 兜底，这里一行都不碰：下面每一次查库都在调用者那家店的
// WithTenant 事务里，别家店的门店 / 大区在这里根本查不出来（404，契约 /admin/
// 段头约定 3）。这一层回答的是「同一家店里，这个人能不能动这一个」，
// 而它的答案是 403 —— 目标就在调用者自己的租户里，说「不归你管」是真话
// （契约约定 6）。
//
// ===========================================================================
// 范围从哪来
// ===========================================================================
//
// auth.StaffIdentity 的 RegionIDs / StoreIDs，由 StaffService.LoadStaffSession
// 在会话校验的同一个事务里读出（staff_scope.go）。每个请求都重读，理由与
// Role / Status 一样：会话 7 天，一个被收回了某家店的店长必须在下一个请求就
// 失去它。

var (
	// ErrRoleForbidden：这个角色做不了这类事（门店管理员建员工、大区管理员建
	// 大区、操作员设默认门店、任何人改自己的角色）。handler 翻成
	// 403 role-forbidden。
	ErrRoleForbidden = errors.New("你的角色不能做这件事")

	// ErrOutOfScope：这类事你能做，但这一个不归你管（华北的大区管理员改华东的
	// 门店、给门店管理员分配别的大区的店）。handler 翻成 403 out-of-scope。
	//
	// 与上一个分开：前端拿到它们要说两句完全不同的话 —— 一个是「找你的上级」，
	// 一个是「找管那个大区的人」。
	ErrOutOfScope = errors.New("目标不在你的管辖范围内")
)

// storeAccess 是门店维度上两类不同的动作。
type storeAccess int

const (
	// storeOperate：门店价、门店上下架、门店库存，以及这几样的读。
	// 大区管理员（本大区的店）与门店管理员（自己的店）都能做。
	storeOperate storeAccess = iota + 1
	// storeManage：门店本身的改、删、围栏。门店管理员不能做 ——
	// 否则一个店长能把自己的店挪进别的大区、或者把围栏画到隔壁城市。
	storeManage
)

// requireMerchantWide 放行对这家店是全店范围的人（管理员、操作员，含平台级的）。
//
// 用在：商品 / SKU / 基准价 / 类目 / 上传的写，建大区。
func requireMerchantWide(ctx context.Context) (auth.StaffIdentity, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if !id.MerchantWide() {
		return auth.StaffIdentity{}, fmt.Errorf("%w: 这件事要全店范围的权限（管理员或操作员）", ErrRoleForbidden)
	}
	return id, nil
}

// requireMerchantAdmin 只放行管理员（role 1，含平台级管理员）。
//
// 用在：设默认门店。默认门店是全国兜底 —— 所有不在任何围栏里、或者没授权定位
// 的访客都落到它 —— 换它等于换掉整个店面对大多数访客的样子，
// 所以操作员也不行。
func requireMerchantAdmin(ctx context.Context) (auth.StaffIdentity, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if !id.IsAdmin() {
		return auth.StaffIdentity{}, fmt.Errorf("%w: 只有管理员能设默认门店", ErrRoleForbidden)
	}
	return id, nil
}

// authorizeRegion 放行对这个大区有权的人：全店范围的，或者这个大区在他范围里
// 的大区管理员。门店管理员一律不行。
//
// 用在：改 / 删大区、大区价、大区上下架，以及这个大区的商品列表。
//
// 不查库：大区管理员的范围就是一串 region_id，比一下就知道。大区存不存在
// 由调用方接着查（查不到是 404）—— 对全店范围的人那是唯一的判据，
// 对大区管理员，范围外的 id 不论存在与否都是 403，不泄露任何东西：
// 他本来就看得见自己租户里的全部大区 id（大区列表对他收窄，但门店详情带着
// region_id）。
func authorizeRegion(ctx context.Context, regionID int64) (auth.StaffIdentity, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	switch {
	case id.MerchantWide():
		return id, nil
	case id.Role == auth.StaffRoleRegionManager:
		if id.ManagesRegion(regionID) {
			return id, nil
		}
		return auth.StaffIdentity{}, fmt.Errorf("%w: 大区 %d 不归你管", ErrOutOfScope, regionID)
	default:
		return auth.StaffIdentity{}, fmt.Errorf("%w: 大区层面的事要全店范围或大区管理员", ErrRoleForbidden)
	}
}

// authorizeStore 放行对这家门店有权做 access 这类事的人。
//
//   - 全店范围的：一律放行，**不查库** —— 门店存不存在由调用方接着查，
//     与改权限之前的行为一个字不差。
//   - 大区管理员：先查出这家店属于哪个大区（StoreScope，走 RLS，别家店的门店
//     查不到 → 404），再看那个大区在不在他范围里。
//   - 门店管理员：只有 storeOperate，且这家店直接挂在他名下。
//
// 必须在调用方的**同一个事务**里调（它收 tx）：分成两次的话，中间的一次
// 「换大区」会让判权与写落在两个不同的大区上 —— 正是 admin_store.go 文件头
// 那段「scope 与写在同一个事务里」的同一条理由。
func authorizeStore(ctx context.Context, tx repository.Tx, storeID int64,
	access storeAccess) (auth.StaffIdentity, error) {

	id, err := requireStaff(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	switch {
	case id.MerchantWide():
		return id, nil
	case id.Role == auth.StaffRoleRegionManager:
		_, regionID, err := tx.StoreScope(ctx, storeID)
		if err != nil {
			return auth.StaffIdentity{}, err
		}
		if id.ManagesRegion(regionID) {
			return id, nil
		}
		return auth.StaffIdentity{}, fmt.Errorf("%w: 门店 %d 所在的大区不归你管", ErrOutOfScope, storeID)
	case id.Role == auth.StaffRoleStoreManager:
		if access != storeOperate {
			return auth.StaffIdentity{}, fmt.Errorf("%w: 门店管理员不能改门店本身（信息、围栏、所属大区）", ErrRoleForbidden)
		}
		if id.ManagesStore(storeID) {
			return id, nil
		}
		return auth.StaffIdentity{}, fmt.Errorf("%w: 门店 %d 不归你管", ErrOutOfScope, storeID)
	default:
		return auth.StaffIdentity{}, fmt.Errorf("%w: 未知角色 %d", ErrRoleForbidden, id.Role)
	}
}

// authorizeNewStore 是建店那一条的判据：建在哪个大区、要不要当默认店。
//
// 默认店只有管理员能设（与 PUT .../default 同一条规矩，否则建店时带一个
// is_default: true 就绕过去了）；非默认店按「在这个大区里管门店」判，
// 也就是 authorizeRegion 的口径 —— 门店管理员不能建店。
func authorizeNewStore(ctx context.Context, regionID int64, isDefault bool) (auth.StaffIdentity, error) {
	if isDefault {
		return requireMerchantAdmin(ctx)
	}
	return authorizeRegion(ctx, regionID)
}

// authorizeStoreMove 是改门店那一条的判据：先按 storeManage 判这家店，
// 若请求要把它换到另一个大区，那个大区也得在范围里（新旧两个都要）。
//
// 只判旧的不够：华北的大区管理员能把自己的店挪进华东，于是华东凭空多出一家
// 华东的大区管理员从没见过的店，而华北那位从此也管不着它了 ——
// 一次越权，两边都没人发现。
func authorizeStoreMove(ctx context.Context, tx repository.Tx, storeID int64,
	newRegionID *int64) (auth.StaffIdentity, error) {

	id, err := authorizeStore(ctx, tx, storeID, storeManage)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if newRegionID == nil || id.MerchantWide() {
		return id, nil
	}
	if !id.ManagesRegion(*newRegionID) {
		return auth.StaffIdentity{}, fmt.Errorf("%w: 目标大区 %d 不归你管", ErrOutOfScope, *newRegionID)
	}
	return id, nil
}

// listFilter 把调用者的范围翻成列表接口的过滤条件。全店范围的人拿到零值
// （nil 切片 = 不限）。
//
// 大区列表：大区管理员看自己的大区；门店管理员看自己那几家店所在的大区
// （门店详情里有 region_id，他本来就知道；给他列出来是为了界面能显示大区名）。
// 门店列表：大区管理员看本大区的店；门店管理员看自己的店。
//
// 返回的切片**永远非 nil**（对受限的人）：nil 在 SQL 里是「不限」，
// 一个范围被清空了的大区管理员拿到的必须是空列表，而不是全部。
func listFilter(ctx context.Context, tx repository.Tx) (regions, stores repository.ScopeFilter, err error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return regions, stores, err
	}
	switch {
	case id.MerchantWide():
		return regions, stores, nil
	case id.Role == auth.StaffRoleRegionManager:
		ids := nonNil(id.RegionIDs)
		return repository.ScopeFilter{RegionIDs: ids}, repository.ScopeFilter{RegionIDs: ids}, nil
	case id.Role == auth.StaffRoleStoreManager:
		live, err := tx.LiveStoreRegions(ctx, id.StoreIDs)
		if err != nil {
			return regions, stores, err
		}
		regionIDs := []int64{}
		seen := map[int64]bool{}
		for _, sr := range live {
			if !seen[sr.RegionID] {
				seen[sr.RegionID] = true
				regionIDs = append(regionIDs, sr.RegionID)
			}
		}
		return repository.ScopeFilter{RegionIDs: regionIDs},
			repository.ScopeFilter{StoreIDs: nonNil(id.StoreIDs)}, nil
	default:
		empty := repository.ScopeFilter{RegionIDs: []int64{}, StoreIDs: []int64{}}
		return empty, empty, nil
	}
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// ---------------------------------------------------------------------------
// 员工管理
// ---------------------------------------------------------------------------

// staffWrite 是一次建 / 改员工在授权眼里的样子：动的是谁、改完之后是什么角色、
// 改完之后管什么。
type staffWrite struct {
	// Target 为 nil 表示新建。
	Target *repository.Staff
	// TargetScopes 是 Target 改之前的范围（新建时为零值）。
	TargetScopes repository.StaffScopes
	// RoleChanged / ScopesChanged：请求里有没有给这两样，且与现状不同。
	RoleChanged, ScopesChanged bool
	// NewRole / NewScopes 是改完之后的角色与范围。
	NewRole   int16
	NewScopes repository.StaffScopes
}

// authorizeStaffWrite 是 POST /admin/staff 与 PATCH /admin/staff/{id} 的唯一判据。
//
// 防提权的每一条都在这里，顺序是硬的：
//
//  1. 操作员、门店管理员（以及任何未知角色）不能管员工 → staff-forbidden
//     （沿用改权限之前那个 type：「你不是管理员」对他们仍是真话）。
//  2. **任何人不能改自己的角色或范围** → role-forbidden。排在最前面
//     （紧跟 1），因为它对管理员也成立：一个管理员把自己改成大区管理员之后，
//     「最后一个在岗管理员」那条 409 未必拦得住（店里还有别的管理员），
//     而那一步是不可逆的自我降级，或者对大区管理员来说是自我扩权。
//  3. 管理员：放行（跨行约束 —— 最后一个管理员 —— 由调用方接着判）。
//  4. 大区管理员：
//     a. 改完之后的角色只能是门店管理员 → 否则 role-forbidden
//     （他建不出管理员、操作员，也建不出和自己平级的大区管理员）；
//     b. 改的是已有的人时，那个人**现在**必须归他管：门店管理员、至少一家店、
//     每一家都在他的大区里、没有大区范围 → 否则 out-of-scope；
//     c. 改完之后的每一家店都必须在他的大区里 → 否则 out-of-scope。
//
// 2 的判据是「给了且与现状不同」，不是「给了」：界面提交整张表单时会把没改的
// 角色原样带回来，把那种请求拒掉只会让管理员连自己的状态都改不了。
func authorizeStaffWrite(ctx context.Context, tx repository.StaffTx, w staffWrite) (auth.StaffIdentity, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if id.Role != auth.StaffRoleAdmin && id.Role != auth.StaffRoleRegionManager {
		return auth.StaffIdentity{}, ErrStaffForbidden
	}
	if w.Target != nil && w.Target.ID == id.StaffID && (w.RoleChanged || w.ScopesChanged) {
		return auth.StaffIdentity{}, fmt.Errorf("%w: 不能改自己的角色或管辖范围", ErrRoleForbidden)
	}
	if id.Role == auth.StaffRoleAdmin {
		return id, nil
	}

	// —— 大区管理员
	if w.NewRole != auth.StaffRoleStoreManager {
		return auth.StaffIdentity{}, fmt.Errorf("%w: 大区管理员只能加、改门店管理员", ErrRoleForbidden)
	}
	if w.Target != nil {
		if w.Target.ID == id.StaffID {
			// 自己改自己的状态 —— 走到这里说明角色与范围都没动。大区管理员
			// 的「本人」不是门店管理员，按 4a 本该在上面就被拒；单独写一支是
			// 为了让这条拒绝的理由说得清。
			return auth.StaffIdentity{}, fmt.Errorf("%w: 大区管理员不能改自己", ErrRoleForbidden)
		}
		ok, err := managedByRegionManager(ctx, tx, id, w.Target.Role, w.TargetScopes)
		if err != nil {
			return auth.StaffIdentity{}, err
		}
		if !ok {
			return auth.StaffIdentity{}, fmt.Errorf("%w: 员工 %d 不是只管你大区门店的门店管理员", ErrOutOfScope, w.Target.ID)
		}
	}
	if len(w.NewScopes.RegionIDs) > 0 {
		return auth.StaffIdentity{}, fmt.Errorf("%w: 大区管理员不能分配大区", ErrOutOfScope)
	}
	live, err := tx.LiveStoreRegions(ctx, w.NewScopes.StoreIDs)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	for _, sr := range live {
		if !id.ManagesRegion(sr.RegionID) {
			return auth.StaffIdentity{}, fmt.Errorf("%w: 门店 %d 不在你的大区里", ErrOutOfScope, sr.StoreID)
		}
	}
	// 不存在 / 已软删的门店不在 live 里，由调用方的配套校验回 422。
	return id, nil
}

// managedByRegionManager 判一个已有的员工归不归这个大区管理员管。
//
// 判据与 db/queries/staff_scopes.sql 的 ListManagedStaff 逐字一致（员工列表里
// 看得见的，就是这里放行的）：门店管理员、至少一家未软删的店、每一家都在
// 他的大区里、没有大区范围。
func managedByRegionManager(ctx context.Context, tx repository.StaffTx, id auth.StaffIdentity,
	role int16, sc repository.StaffScopes) (bool, error) {

	if role != auth.StaffRoleStoreManager || len(sc.RegionIDs) > 0 {
		return false, nil
	}
	live, err := tx.LiveStoreRegions(ctx, sc.StoreIDs)
	if err != nil {
		return false, err
	}
	if len(live) == 0 {
		return false, nil
	}
	for _, sr := range live {
		if !id.ManagesRegion(sr.RegionID) {
			return false, nil
		}
	}
	return true, nil
}

// normalizeIDs 去重、升序。请求体里的 id 列表原样落库的话，一个 [3,3] 会撞
// uk_staff_scopes_store 回 500，而响应里的顺序会随提交顺序漂。
func normalizeIDs(ids []int64) []int64 {
	out := []int64{}
	seen := map[int64]bool{}
	for _, v := range ids {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sameIDs(a, b []int64) bool {
	a, b = normalizeIDs(a), normalizeIDs(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
