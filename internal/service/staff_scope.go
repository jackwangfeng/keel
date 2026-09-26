package service

import (
	"context"
	"fmt"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 员工管辖范围的读写（00025，数据模型 §14 staff_scopes）。
//
// 判权不在这里（authz.go），这里只管三件事：会话校验时把范围读进身份、
// 往响应里补范围、以及建 / 改员工时「角色与范围配不配套」的请求体校验。

// loadStaffScopes 读一个人的范围，给 LoadStaffSession 在会话校验的**同一个事务**
// 里调 —— 于是 auth.StaffIdentity 里的 Role 与 RegionIDs / StoreIDs 来自同一个
// 快照：一个刚被从大区管理员降成操作员的人，不会在某一个请求里同时带着
// 「新角色」与「旧范围」。
//
// 平台级身份一律没有范围，而且**不能**去读：staff_scopes 的策略调
// current_merchant()，平台作用域里那是 RAISE（整条后台会话校验会因此 401）。
func loadStaffScopes(ctx context.Context, tx repository.StaffTx, platform bool,
	staffID int64) (repository.StaffScopes, error) {
	if platform {
		return repository.StaffScopes{RegionIDs: []int64{}, StoreIDs: []int64{}}, nil
	}
	return tx.ListStaffScopes(ctx, staffID)
}

// withScopes 给一个员工补上范围，用在所有返回 Staff 的地方（契约里
// region_ids / store_ids 是必返的）。
func withScopes(ctx context.Context, tx repository.StaffTx, platform bool,
	st repository.Staff) (repository.Staff, error) {
	sc, err := loadStaffScopes(ctx, tx, platform, st.ID)
	if err != nil {
		return repository.Staff{}, err
	}
	st.RegionIDs, st.StoreIDs = sc.RegionIDs, sc.StoreIDs
	return st, nil
}

// withScopesAll 是 withScopes 的批量版，一次查完一页。
func withScopesAll(ctx context.Context, tx repository.StaffTx, platform bool,
	items []repository.Staff) ([]repository.Staff, error) {
	if platform || len(items) == 0 {
		return items, nil
	}
	ids := make([]int64, 0, len(items))
	for _, st := range items {
		ids = append(ids, st.ID)
	}
	m, err := tx.ListStaffScopesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		sc := m[items[i].ID]
		items[i].RegionIDs, items[i].StoreIDs = sc.RegionIDs, sc.StoreIDs
	}
	return items, nil
}

// checkRoleScopes 是「角色与范围配不配套」：请求体层面的校验，与谁在调无关。
//
//   - role 1 / 2：两样都不带（全店范围不需要、也不该有范围行）；
//   - role 3：至少一个大区、不带门店；
//   - role 4：至少一家门店、不带大区；
//   - 平台级作用域里只能是 1 / 2 —— 范围只对商家级员工有意义，00025 的复合外键
//     也会挡住，但那会是一句 23503，而这里给的是一句能读懂的 422；
//   - 引用的大区 / 门店必须存在且未软删（外键只挡「不存在」）。
//
// 不配套一律 ErrStaffBadRequest（422）。
func checkRoleScopes(ctx context.Context, tx repository.StaffTx, platform bool,
	role int16, sc repository.StaffScopes) error {

	switch role {
	case auth.StaffRoleAdmin, auth.StaffRoleOperator:
		if len(sc.RegionIDs) > 0 || len(sc.StoreIDs) > 0 {
			return fmt.Errorf("%w: 管理员与操作员是全店范围，不带 region_ids / store_ids", ErrStaffBadRequest)
		}
		return nil
	case auth.StaffRoleRegionManager, auth.StaffRoleStoreManager:
		if platform {
			return fmt.Errorf("%w: 平台级员工只能是管理员或操作员", ErrStaffBadRequest)
		}
	default:
		return fmt.Errorf("%w: role 只能是 1、2、3 或 4", ErrStaffBadRequest)
	}

	if role == auth.StaffRoleRegionManager {
		if len(sc.RegionIDs) == 0 || len(sc.StoreIDs) > 0 {
			return fmt.Errorf("%w: 大区管理员要带至少一个 region_ids，且不带 store_ids", ErrStaffBadRequest)
		}
		live, err := tx.LiveRegionIDs(ctx, sc.RegionIDs)
		if err != nil {
			return err
		}
		if len(live) != len(sc.RegionIDs) {
			return fmt.Errorf("%w: region_ids 里有不存在或已删除的大区", ErrStaffBadRequest)
		}
		return nil
	}
	if len(sc.StoreIDs) == 0 || len(sc.RegionIDs) > 0 {
		return fmt.Errorf("%w: 门店管理员要带至少一个 store_ids，且不带 region_ids", ErrStaffBadRequest)
	}
	live, err := tx.LiveStoreRegions(ctx, sc.StoreIDs)
	if err != nil {
		return err
	}
	if len(live) != len(sc.StoreIDs) {
		return fmt.Errorf("%w: store_ids 里有不存在或已删除的门店", ErrStaffBadRequest)
	}
	return nil
}
