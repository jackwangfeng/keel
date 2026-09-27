package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/keel/keel/internal/repository"
)

// 门店解析：买家侧两条接口（GET /stores、GET /stores/resolve）以及
// 每一条「结果取决于哪家店服务你」的读路径共用的那一段逻辑。数据模型 §4。
//
// ===========================================================================
// 四个边界问题的答案在这一个文件里，而且只有一份实现
// ===========================================================================
//
// 产品对这四个问题给过明确答复，它们合起来就是 resolve 的全部：
//
//  1. **围栏重叠** → 全部返回，按距离升序，服务端不替客户端挑
//     （挑哪一家涉及配送时效、是否自提、用户上次选过谁，服务端不知道）。
//  2. **不在任何围栏内** → 回落到「全国配送」的默认门店。
//  3. **拒绝授权定位 / 没传坐标** → **与第 2 条同一条路径**。
//     这不是两条相似的规则，是一条规则 —— 所以下面只有一个 fallback()，
//     两种输入在进入它之前就已经合流了。契约那一侧的约束是「两种输入的响应
//     必须逐字段相同」，而这里的形状让它不可能不同。
//  4. **没配默认店** → `match_type = none`，空数组，**HTTP 仍然 200**。
//     「不在服务范围」是一个正常的查询结果，不是错误：用 404 表达它会让
//     客户端的错误分支同时装着「网络失败」「鉴权失败」和「这个地方我们不送」，
//     而第三种要渲染的是一个完全不同的页面。

// MatchType 是「当前门店是怎么定下来的」。三个取值就是契约的 StoreMatchType。
type MatchType string

const (
	// MatchFence：坐标落在一个或多个围栏内。
	MatchFence MatchType = "fence"
	// MatchFallbackDefault：不在任何围栏内，**或根本没有位置**。
	MatchFallbackDefault MatchType = "fallback_default"
	// MatchNone：连默认门店都没配，这个租户对这次请求不在服务范围。
	MatchNone MatchType = "none"
)

// Coord 是买家坐标。两个值**同时给或同时不给**，所以用一个结构体的指针
// 而不是两个 *float64：后者让「只给了 lat」成为一个写得出来的状态，
// 而那是一次 422，不该由每个调用方各自记得去判。
type Coord struct{ Lat, Lng float64 }

// StoreResolution 是一次门店解析的结果。
//
// StoreID / RegionID 在 MatchNone 时是零值，而 Scope() 会把那种情况报成
// ErrOutOfServiceArea —— 让「不在服务范围」在类型上没法被当成「0 号门店」用。
type StoreResolution struct {
	MatchType MatchType
	StoreID   int64
	RegionID  int64
	Stores    []repository.StoreMatch
}

// ErrOutOfServiceArea：这次请求解析不到任何门店（商家没配默认店）。
//
// 它**不是** 404：读路径拿到它要回一个 200 + 空列表 + match_type = none。
// 做成错误只是为了让「没有门店」在 Go 这一侧没法被静默当成 store_id = 0 ——
// 而 store_id = 0 会让那些 WHERE store_id = $1 安静地返回空集，
// 症状与「这家店什么都不卖」一模一样。
var ErrOutOfServiceArea = errors.New("当前位置不在服务范围（这家商家没有默认门店）")

// ErrStoreNotFound：调用方显式指名的 store_id 不存在或不属于当前租户。
// 契约在买家侧读路径上把它定成 422（store_id 在 query 里，不在路径里）。
var ErrStoreNotFound = errors.New("指定的门店不存在或不属于当前租户")

// ErrStoreClosed：指名的门店存在，但现在接不了单 —— 它停业了，或者它所在的大区停用了
// （2026-09-27：大区停用之前对买家毫无影响）。契约 409 store-unavailable：状态问题，
// 不是请求写错了；客户端该做的是重新定位 / 换一家店。
var ErrStoreClosed = errors.New("门店已停业或所在大区已停用")

// ErrInvalidCoord：lat / lng 只给了一个，或取值超范围（契约 422）。
var ErrInvalidCoord = errors.New("lat 与 lng 必须同时给出且在有效范围内")

// 围栏命中最多返回几家。契约的 size 参数：默认 10，上限 50。
const (
	defaultResolveSize = 10
	maxResolveSize     = 50
)

// StoreRepository 是本服务需要的仓储能力。收接口不收 *repository.Repo，
// 理由与 ProductRepository 一字不差。
type StoreRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

type StoreService struct{ repo StoreRepository }

func NewStoreService(r StoreRepository) *StoreService { return &StoreService{repo: r} }

// Resolve 按坐标解析门店。coord 为 nil 即「没有位置」——**与「不在围栏内」
// 同一条路径**，这是产品第 3 条答复的兑现。
func (s *StoreService) Resolve(ctx context.Context, coord *Coord, size int) (StoreResolution, error) {
	if coord != nil {
		if coord.Lat < -90 || coord.Lat > 90 || coord.Lng < -180 || coord.Lng > 180 {
			return StoreResolution{}, fmt.Errorf("lat=%v lng=%v: %w",
				coord.Lat, coord.Lng, ErrInvalidCoord)
		}
	}
	if size <= 0 {
		size = defaultResolveSize
	}
	if size > maxResolveSize {
		size = maxResolveSize
	}

	var out StoreResolution
	err := s.repo.WithTenant(ctx, func(q repository.Tx) error {
		var err error
		out, err = resolveIn(ctx, q, coord, int32(size))
		return err
	})
	if err != nil {
		return StoreResolution{}, err
	}
	return out, nil
}

// resolveIn 是那段逻辑本身，跑在调用方的事务里。
//
// 抽成一个自由函数是为了让「读商品的那次解析」与「/stores/resolve 的那次解析」
// **在同一个事务里**完成 —— 分成两次的话，两者之间的一次门店软删会让
// 「解析到了 A 店」与「按 A 店读商品」看到不同的世界，而那种不一致偶发、
// 不报错、复现不了。
func resolveIn(ctx context.Context, q repository.Tx, coord *Coord, size int32) (StoreResolution, error) {
	if coord != nil {
		hits, err := q.ResolveStoresByPoint(ctx, coord.Lat, coord.Lng, size)
		if err != nil {
			return StoreResolution{}, err
		}
		if len(hits) > 0 {
			// 第一家就是最近的那一家（SQL 按 distance_m 升序）。
			// 「当前门店」取它，而全部命中都回给客户端 —— 它可以自己换一家。
			_, regionID, err := q.StoreScope(ctx, hits[0].ID)
			if err != nil {
				return StoreResolution{}, err
			}
			return StoreResolution{
				MatchType: MatchFence, StoreID: hits[0].ID, RegionID: regionID,
				Stores: hits,
			}, nil
		}
	}
	// 回落。**没有坐标与坐标落在围栏外走到的是这同一行**。
	def, regionID, err := q.DefaultStore(ctx)
	if errors.Is(err, repository.ErrCatalogNotFound) {
		return StoreResolution{MatchType: MatchNone, Stores: []repository.StoreMatch{}}, nil
	}
	if err != nil {
		return StoreResolution{}, err
	}
	// distance_m 在回落这一支恒为 nil，即使这次请求带了坐标：契约写死
	// 「为 null 当且仅当算不出距离」而回落不是按距离选出来的 —— 给它算一个
	// 距离会让客户端以为「这家店离你 42 公里但仍然在服务范围内」，
	// 而真相是「你不在任何围栏里，我们给你派了兜底店」。
	def.DistanceM = nil
	return StoreResolution{
		MatchType: MatchFallbackDefault, StoreID: def.ID, RegionID: regionID,
		Stores: []repository.StoreMatch{def},
	}, nil
}

// scopeIn 给读路径用：解析出 StoreScope，或者告诉调用方「不在服务范围」。
//
// storeID 非 nil 表示调用方**显式指名**了一家店（契约里 /products、
// /products/{id}、/search 三条都有这个参数）。此时不走围栏判定 —— 客户端
// 已经从 GET /stores 里挑过了，再按坐标覆盖它就是把用户的选择丢掉。
//
// 指名一家不存在 / 不属于本租户 / 已软删的门店返回 ErrStoreNotFound（422），
// **不是静默回落到默认店**：静默回落会让客户端以为自己看的是 A 店的价，
// 而实际上是 B 店的 —— 而两者都「正常工作」。
func scopeIn(ctx context.Context, q repository.Tx, storeID *int64) (repository.StoreScope, MatchType, error) {
	if storeID != nil {
		_, regionID, err := q.StoreScope(ctx, *storeID)
		if errors.Is(err, repository.ErrCatalogNotFound) {
			return repository.StoreScope{}, "", fmt.Errorf("store %d: %w", *storeID, ErrStoreNotFound)
		}
		if err != nil {
			return repository.StoreScope{}, "", err
		}
		// 客户端指名的那一家按 fence 报。
		//
		// 这是一处**拍板**：契约的 StoreMatchType 只有三个取值，而「客户端
		// 自己挑的」不在其中。三个里 fence 是唯一表示「一家具体的、不是兜底的
		// 门店」的那个，fallback_default 会让客户端以为服务端没认它传的参数。
		// 加第四个枚举值要改契约与三份产物，而它对客户端没有任何新的行为含义
		// —— 客户端本来就知道自己传了什么。
		return repository.StoreScope{StoreID: *storeID, RegionID: regionID}, MatchFence, nil
	}
	res, err := resolveIn(ctx, q, nil, defaultResolveSize)
	if err != nil {
		return repository.StoreScope{}, "", err
	}
	if res.MatchType == MatchNone {
		return repository.StoreScope{}, MatchNone, ErrOutOfServiceArea
	}
	return repository.StoreScope{StoreID: res.StoreID, RegionID: res.RegionID}, res.MatchType, nil
}

// StoreListing 是 GET /stores 的一页。
type StoreListing struct {
	Items    []repository.StoreMatch
	Page     int
	PageSize int
	Total    int64
}

// ListOpen 返回营业中的门店，供买家手动选店。
//
// 这条端点的存在理由是 resolve 的回落：拒绝定位的买家会被落到默认门店，
// 而他应该能自己改。没有这条，「回落」就成了一个用户无法纠正的结果。
func (s *StoreService) ListOpen(ctx context.Context, page, pageSize int) (StoreListing, error) {
	page, pageSize = clampPaging(page, pageSize)
	out := StoreListing{Items: []repository.StoreMatch{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(q repository.Tx) error {
		rows, total, err := q.ListOpenStores(ctx, int32(pageSize), int32(offsetOf(page, pageSize)))
		if err != nil {
			return err
		}
		out.Items, out.Total = rows, total
		return nil
	})
	if err != nil {
		return StoreListing{}, err
	}
	return out, nil
}
