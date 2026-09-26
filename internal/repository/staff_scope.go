package repository

import (
	"context"
	"fmt"
	"math"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 大区管理员 / 门店管理员的管辖范围（数据模型 §14 staff_scopes，迁移 00025）。
//
// 这一层只管读写那张表，**不判任何权限**：「谁能给谁分配哪家店」是业务规则，
// 在 internal/service/authz.go。这里的每个方法都只在租户作用域里有意义 ——
// staff_scopes 的策略调 current_merchant()，平台作用域里那是 RAISE。

// ScopeFilter 是列表接口在同一租户内的权限过滤。
//
// **nil 切片 = 不限**，空切片 = 一个都不给。两者的区别是要紧的：一个范围被清空了
// 的大区管理员（数据不一致，但可达）拿到的必须是空列表，而不是全部大区。
// pgx 把 nil 切片编码成 NULL、把空切片编码成 '{}'，SQL 那一侧
// （AdminListRegions 的 only_ids）据此分成「不限」与「= ANY('{}')」两支。
type ScopeFilter struct {
	RegionIDs []int64
	StoreIDs  []int64
}

// StaffScopes 是一个人的范围：管的大区与管的门店，各自按 id 升序。
type StaffScopes struct {
	RegionIDs []int64
	StoreIDs  []int64
}

// StoreRegion 是一家未软删门店与它所属的大区。
type StoreRegion struct {
	StoreID  int64
	RegionID int64
}

// StaffScopeTx 是这张表的接口面。
type StaffScopeTx interface {
	// ListStaffScopes 读一个人的范围。每个后台请求都会调一次（会话校验那一次）。
	ListStaffScopes(ctx context.Context, staffID int64) (StaffScopes, error)
	// ListStaffScopesFor 一次读一批人的范围，键是 staff_id。没有范围的人不在 map 里。
	ListStaffScopesFor(ctx context.Context, staffIDs []int64) (map[int64]StaffScopes, error)
	// ReplaceStaffScopes 整体替换一个人的范围：先清后插，同一个事务。
	ReplaceStaffScopes(ctx context.Context, staffID int64, sc StaffScopes) error
	// LiveRegionIDs 返回给定大区里存在且未软删的那些。
	LiveRegionIDs(ctx context.Context, ids []int64) ([]int64, error)
	// LiveStoreRegions 返回给定门店里存在且未软删的那些，连同所属大区。
	LiveStoreRegions(ctx context.Context, ids []int64) ([]StoreRegion, error)
	// ListManagedStaff / CountManagedStaff 是大区管理员看得见的员工，
	// 判据写在 db/queries/staff_scopes.sql 的 ListManagedStaff 上。
	ListManagedStaff(ctx context.Context, selfID int64, regionIDs []int64, limit, offset int64) ([]Staff, error)
	CountManagedStaff(ctx context.Context, selfID int64, regionIDs []int64) (int64, error)
}

func (t tenantTx) ListStaffScopes(ctx context.Context, staffID int64) (StaffScopes, error) {
	rows, err := t.q.ListStaffScopes(ctx, staffID)
	if err != nil {
		return StaffScopes{}, err
	}
	out := StaffScopes{RegionIDs: []int64{}, StoreIDs: []int64{}}
	for _, r := range rows {
		switch {
		case r.RegionID != nil:
			out.RegionIDs = append(out.RegionIDs, *r.RegionID)
		case r.StoreID != nil:
			out.StoreIDs = append(out.StoreIDs, *r.StoreID)
		}
	}
	return out, nil
}

func (t tenantTx) ListStaffScopesFor(ctx context.Context, staffIDs []int64) (map[int64]StaffScopes, error) {
	out := map[int64]StaffScopes{}
	if len(staffIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListStaffScopesFor(ctx, staffIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		sc := out[r.StaffID]
		switch {
		case r.RegionID != nil:
			sc.RegionIDs = append(sc.RegionIDs, *r.RegionID)
		case r.StoreID != nil:
			sc.StoreIDs = append(sc.StoreIDs, *r.StoreID)
		}
		out[r.StaffID] = sc
	}
	return out, nil
}

func (t tenantTx) ReplaceStaffScopes(ctx context.Context, staffID int64, sc StaffScopes) error {
	if err := t.q.DeleteStaffScopes(ctx, staffID); err != nil {
		return err
	}
	for _, id := range sc.RegionIDs {
		id := id
		if err := t.q.InsertStaffRegionScope(ctx, db.InsertStaffRegionScopeParams{
			StaffID: staffID, RegionID: &id,
		}); err != nil {
			return fmt.Errorf("给员工 %d 挂大区 %d: %w", staffID, id, err)
		}
	}
	for _, id := range sc.StoreIDs {
		id := id
		if err := t.q.InsertStaffStoreScope(ctx, db.InsertStaffStoreScopeParams{
			StaffID: staffID, StoreID: &id,
		}); err != nil {
			return fmt.Errorf("给员工 %d 挂门店 %d: %w", staffID, id, err)
		}
	}
	return nil
}

func (t tenantTx) LiveRegionIDs(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return []int64{}, nil
	}
	return t.q.LiveRegionIDs(ctx, ids)
}

func (t tenantTx) LiveStoreRegions(ctx context.Context, ids []int64) ([]StoreRegion, error) {
	out := []StoreRegion{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := t.q.LiveStoreRegions(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, StoreRegion{StoreID: r.ID, RegionID: r.RegionID})
	}
	return out, nil
}

func (t tenantTx) ListManagedStaff(ctx context.Context, selfID int64, regionIDs []int64,
	limit, offset int64) ([]Staff, error) {
	if limit < 0 || limit > math.MaxInt32 || offset < 0 || offset > math.MaxInt32 {
		return nil, fmt.Errorf("limit %d / offset %d 超出范围", limit, offset)
	}
	if regionIDs == nil {
		// nil 在 SQL 里是 NULL，而 NOT (x = ANY(NULL)) 是 NULL —— 于是「范围外」
		// 那一支永远判不成真，任何一个门店管理员都会被当成「归我管」。
		// 这里把它压成空集，让失败方向是「一个都看不见」。
		regionIDs = []int64{}
	}
	rows, err := t.q.ListManagedStaff(ctx, db.ListManagedStaffParams{
		SelfID: selfID, RegionIds: regionIDs,
		PageLimit: int32(limit), PageOffset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Staff, 0, len(rows))
	for _, r := range rows {
		out = append(out, t.staffOf(r.ID, r.Email, r.Name, r.Role, r.Status,
			r.LastLoginAt, r.CreatedAt))
	}
	return out, nil
}

func (t tenantTx) CountManagedStaff(ctx context.Context, selfID int64, regionIDs []int64) (int64, error) {
	if regionIDs == nil {
		regionIDs = []int64{}
	}
	return t.q.CountManagedStaff(ctx, db.CountManagedStaffParams{SelfID: selfID, RegionIds: regionIDs})
}
