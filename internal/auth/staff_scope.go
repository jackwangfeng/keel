package auth

// 分级权限的两个新角色（00025，数据模型 §14 staff_scopes）。
//
// 单独一个文件，不塞进 staff_middleware.go 那个常量块：中间件只负责「这个请求
// 是谁发的」，而「这个人能做什么」是业务层的事（internal/service/authz.go）。
// 这里只放两者都要认得的取值与几个纯函数。
const (
	// StaffRoleRegionManager 大区管理员：管 staff_scopes 里的那几个大区，
	// 以及这些大区下的门店。
	StaffRoleRegionManager int16 = 3
	// StaffRoleStoreManager 门店管理员：只管 staff_scopes 里的那几家门店的
	// 价格、上下架与库存。
	StaffRoleStoreManager int16 = 4
)

// MerchantWide 回答这个人对**当前这家店**是不是全店范围：管理员与操作员是
// （平台级的也是 —— 他们做的是 Host 那家店的事），大区 / 门店管理员不是。
//
// 按 role 白名单判，而不是「不是 3 也不是 4」：一个 role = 0 或者将来的
// role = 5 在这里必须落到「不是」，失败方向是拒绝。
func (s StaffIdentity) MerchantWide() bool {
	return s.Role == StaffRoleAdmin || s.Role == StaffRoleOperator
}

// ManagesRegion 回答这个大区在不在他的范围里。只对大区管理员有意义。
func (s StaffIdentity) ManagesRegion(regionID int64) bool {
	return s.Role == StaffRoleRegionManager && containsID(s.RegionIDs, regionID)
}

// ManagesStore 回答这家门店是不是**直接**挂在他名下。只对门店管理员有意义；
// 大区管理员管的门店要先查出它属于哪个大区，那一步要查库，在 service 里做。
func (s StaffIdentity) ManagesStore(storeID int64) bool {
	return s.Role == StaffRoleStoreManager && containsID(s.StoreIDs, storeID)
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
