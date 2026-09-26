package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 门店与大区在 repository 边界上的领域类型与失败取值（数据模型 §4，
// 契约 Store tag 的 23 条操作）。
//
// 这一层是**手写的领域层**，包在 sqlc 之上 —— 与 admin_product.go 那一批同一个
// 形状。sqlc 的行类型不外泄：它们跟着 SQL 的 SELECT 列表走，而领域类型跟着
// 契约走，两者合成一个之后，SQL 里加一列就会让契约类型多一个字段。
//
// ===========================================================================
// 几何值在这一层的形状：三个指针，一个哨兵都不外泄
// ===========================================================================
//
// 库里 location 与 fence 都可空（一家还没定位、还没画围栏的门店是合法的
// 中间态），而 sqlc 把带 cast 的 PostGIS 表达式一律推断成非空。
// db/queries/stores.sql 的文件头记了实测与两次尝试：所以那一层返回的是
// has_location 布尔 + COALESCE 到哨兵值，**由这一层翻成指针**。
// 哨兵值（坐标 0/0、距离 -1、围栏空串）到这里为止，再往上一个都看不见 ——
// 几内亚湾那个点是真实坐标，让上层去猜「0,0 是不是真的」是在埋一个
// 十年后才会有人踩到的坑。

var (
	// ErrRegionCodeConflict：大区编号在本租户内已存在（契约 409
	// region-code-conflict）。判据是 uk_regions_code 这条**部分**唯一索引，
	// 它只管未软删的行 —— 软删掉的大区不占编号。
	ErrRegionCodeConflict = errors.New("大区编号在本租户内已存在")

	// ErrRegionHasStores：大区名下还有未软删的门店，不许软删（契约 409
	// region-has-stores）。
	//
	// **不做级联**：级联软删一个大区会连带让它下面所有门店接不到单，
	// 而调用方在点下删除时看到的只是一个大区名。
	ErrRegionHasStores = errors.New("大区名下还有门店")

	// ErrStoreCodeConflict：门店编号在本租户内已存在（契约 409
	// store-code-conflict）。
	ErrStoreCodeConflict = errors.New("门店编号在本租户内已存在")

	// ErrDefaultStoreConflict：已经有一家默认门店了（契约 409
	// default-store-conflict），对应 uk_stores_default 那条部分唯一索引。
	//
	// 建店时撞上它是一个真的冲突，不是一次「顺手切换」：切换默认店要先清旧
	// 再置新，那是 PUT /admin/stores/{id}/default 的事，而它在**同一个事务**
	// 里做那两步。让 POST 顺手把别人家的默认位抢过来，等于给「建一家店」
	// 这个动作附带一个谁也没预料到的副作用。
	ErrDefaultStoreConflict = errors.New("已经有一家默认门店")

	// ErrStoreFenceRequired：给一家**非默认**门店清空围栏（契约 409
	// store-fence-required）。
	//
	// 一家非默认店没有围栏就是一家永远接不到单的店 —— 它不会被任何坐标命中，
	// 也不是回落目标。后台会把它列出来、能配库存、能上下架、能定价，
	// 而它一单也接不到。
	//
	// **判据在 SetStoreFence 里，不在数据库上。** 本轮契约先行那一版把它写成
	// stores 的一条 CHECK (is_default OR fence IS NOT NULL)，落地实测那条 CHECK
	// 挡死了两条主路径（POST /admin/stores 建不出任何非默认店；
	// PUT .../default 在每一个迁移过的库上都失败，因为回填造出来的默认店没有
	// 围栏）。论证与两次实测记在 00020 里 stores 表的定义上。
	ErrStoreFenceRequired = errors.New("非默认门店必须有围栏")

	// ErrStoreUnavailable：这家门店已被软删或已停业，不能作为回落目标
	// （契约 409 store-unavailable）。回落目标接不了单的话，「回落」
	// 就成了一个把用户送进死胡同的动作。
	ErrStoreUnavailable = errors.New("门店已停业或已软删，不能作为默认门店")

	// ErrStoreAmbiguous：那条**不带门店**的库存 CAS（PUT
	// /admin/skus/{sku_id}/inventory）在本租户有多家门店时的失败
	// （契约 409 store-ambiguous）。
	//
	// 契约把它的语义写死成「恰好一家才可用」，而**不是**「落到默认门店」：
	// 库存是唯一真相，猜错一家店的后果是把另一家店的水位覆盖掉，
	// 而且没有任何东西会响。
	ErrStoreAmbiguous = errors.New("本租户有多家门店，这条路径无法确定是哪一家")
)

// InvalidFenceError 是围栏不合法（契约 422 invalid-fence）。
//
// 做成带字段的错误类型而不是一个裸 sentinel：契约要求把 PostGIS 给出的
// ST_IsValidReason 原样转述进 Problem 的 detail。实测领结形多边形
// [[0,0],[1,1],[1,0],[0,1],[0,0]] 的 reason 是
// "Self-intersection at or near point 0.5 0.5" —— 那句话是运营唯一能拿来
// 定位自己画错在哪儿的东西，丢掉它，422 就只剩「你画的不对」。
type InvalidFenceError struct{ Reason string }

func (e *InvalidFenceError) Error() string {
	return "围栏不是一个合法的多边形: " + e.Reason
}

// Region 是大区的领域类型。
type Region struct {
	ID         int64
	Code       string
	Name       string
	Status     int16
	StoreCount int32
	DeletedAt  *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Store 是后台视角的门店，**含围栏**。
//
// 与买家侧的 StoreMatch 分成两个类型，理由与 AdminProduct / ProductSummary
// 一样：围栏、停业状态、软删标记是运营要管的，把它们塞进买家类型等于让
// 生成出来的买家客户端带着一组它永远收不到、也不该收到的字段。
type Store struct {
	ID           int64
	RegionID     int64
	RegionName   string
	Code         string
	Name         string
	Phone        string
	Province     string
	City         string
	District     string
	Address      string
	Lat          *float64
	Lng          *float64
	FenceGeoJSON *string
	IsDefault    bool
	Status       int16
	DeletedAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// StoreMatch 是买家侧看到的门店。DistanceM 为 nil **当且仅当**算不出距离
// （本次请求没带坐标，或这家店自己没有坐标）。
type StoreMatch struct {
	ID        int64
	Name      string
	Phone     string
	Address   string
	IsDefault bool
	Lat       *float64
	Lng       *float64
	DistanceM *float64
}

// NewRegion / RegionPatch / NewStore / StorePatch 是四个入参结构。
// 指针字段为 nil 表示「不动」。
type NewRegion struct{ Code, Name string }

type RegionPatch struct {
	Code   *string
	Name   *string
	Status *int16
}

type NewStore struct {
	RegionID                          int64
	Code, Name, Phone                 string
	Province, City, District, Address string
	Lat, Lng                          *float64
	IsDefault                         bool
}

// StorePatch **刻意没有 Fence 与 IsDefault**：它们各有自己的端点，
// 因为它们不是普通字段 —— 一个要过 ST_IsValid，另一个要在同一个事务里
// 先清旧再置新。混进这个结构体就等于给那两条纪律留了第二条绕过去的路。
type StorePatch struct {
	RegionID                          *int64
	Code, Name, Phone                 *string
	Province, City, District, Address *string
	Status                            *int16
	SetLocation                       bool
	Lat, Lng                          *float64
}

// StoreTx 是门店与大区这一面。
type StoreTx interface {
	// —— 大区
	AdminListRegions(ctx context.Context, includeDeleted bool, limit, offset int32) ([]Region, int64, error)
	FindRegion(ctx context.Context, id int64) (Region, error)
	CreateRegion(ctx context.Context, n NewRegion) (Region, error)
	UpdateRegion(ctx context.Context, id int64, p RegionPatch) (Region, error)
	SoftDeleteRegion(ctx context.Context, id int64) error

	// —— 门店
	AdminListStores(ctx context.Context, regionID *int64, includeDeleted bool, limit, offset int32) ([]Store, int64, bool, error)
	FindStore(ctx context.Context, id int64) (Store, error)
	CreateStore(ctx context.Context, n NewStore) (Store, error)
	UpdateStore(ctx context.Context, id int64, p StorePatch) (Store, error)
	SoftDeleteStore(ctx context.Context, id int64) error
	SetStoreFence(ctx context.Context, id int64, geojson *string) (Store, error)
	MakeStoreDefault(ctx context.Context, id int64) (Store, error)

	// StoreScope 取一家未软删门店的 (id, region_id)。查不到返回 ErrCatalogNotFound。
	StoreScope(ctx context.Context, id int64) (int64, int64, error)

	// —— 买家侧
	ListOpenStores(ctx context.Context, limit, offset int32) ([]StoreMatch, int64, error)
	ResolveStoresByPoint(ctx context.Context, lat, lng float64, limit int32) ([]StoreMatch, error)
	DefaultStore(ctx context.Context) (StoreMatch, int64, error)
}

// ---------------------------------------------------------------------------
// 大区
// ---------------------------------------------------------------------------

const regionCodeIndex = "uk_regions_code"

func (t tenantTx) AdminListRegions(ctx context.Context, includeDeleted bool, limit, offset int32) ([]Region, int64, error) {
	rows, err := t.q.AdminListRegions(ctx, db.AdminListRegionsParams{
		IncludeDeleted: includeDeleted, PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.AdminCountRegions(ctx, includeDeleted)
	if err != nil {
		return nil, 0, err
	}
	out := make([]Region, 0, len(rows))
	for _, r := range rows {
		out = append(out, Region{
			ID: r.ID, Code: r.Code, Name: r.Name, Status: r.Status,
			StoreCount: r.StoreCount, DeletedAt: optTime(r.DeletedAt),
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, total, nil
}

// FindRegion 取一个**未软删**的大区。软删的与不存在的一律 ErrCatalogNotFound：
// 契约在这几条上都是 404，而把两者分开等于给自增 id 空间造一个存在性探针。
func (t tenantTx) FindRegion(ctx context.Context, id int64) (Region, error) {
	r, err := t.q.AdminGetRegion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Region{}, fmt.Errorf("region %d: %w", id, ErrCatalogNotFound)
	}
	if err != nil {
		return Region{}, err
	}
	if r.DeletedAt.Valid {
		return Region{}, fmt.Errorf("region %d 已软删: %w", id, ErrCatalogNotFound)
	}
	return Region{
		ID: r.ID, Code: r.Code, Name: r.Name, Status: r.Status,
		StoreCount: r.StoreCount, DeletedAt: optTime(r.DeletedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

func (t tenantTx) CreateRegion(ctx context.Context, n NewRegion) (Region, error) {
	r, err := t.q.CreateRegion(ctx, db.CreateRegionParams{Code: n.Code, Name: n.Name})
	if isUniqueViolation(err, regionCodeIndex) {
		return Region{}, fmt.Errorf("region code %q: %w", n.Code, ErrRegionCodeConflict)
	}
	if err != nil {
		return Region{}, err
	}
	return Region{
		ID: r.ID, Code: r.Code, Name: r.Name, Status: r.Status, StoreCount: 0,
		DeletedAt: optTime(r.DeletedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

func (t tenantTx) UpdateRegion(ctx context.Context, id int64, p RegionPatch) (Region, error) {
	row, err := t.q.UpdateRegion(ctx, db.UpdateRegionParams{
		ID: id, Code: p.Code, Name: p.Name, Status: p.Status,
	})
	if isUniqueViolation(err, regionCodeIndex) {
		return Region{}, fmt.Errorf("region code: %w", ErrRegionCodeConflict)
	}
	if err != nil {
		return Region{}, err
	}
	if row.VisibleRows == 0 || row.UpdatedRows == 0 {
		// 这条 UPDATE 没有「可见但改不成」的分支（不像 SoftDeleteRegion 那样
		// 带闸门），所以两个 0 是同一件事：这个大区在本租户下看不见。
		return Region{}, fmt.Errorf("region %d: %w", id, ErrCatalogNotFound)
	}
	// store_count 不在那条 UPDATE 的返回里（它是一次子查询，而 UPDATE 的
	// RETURNING 只认被改的那一行的列）。回读一次而不是回 0：契约里它是必返的，
	// 而 0 会让后台以为这个大区可以删了。
	return t.FindRegion(ctx, id)
}

func (t tenantTx) SoftDeleteRegion(ctx context.Context, id int64) error {
	row, err := t.q.SoftDeleteRegion(ctx, id)
	if err != nil {
		return err
	}
	if row.VisibleRows == 0 {
		return fmt.Errorf("region %d: %w", id, ErrCatalogNotFound)
	}
	if row.DeletedRows == 0 {
		// 可见却没删成，只有一种成因：名下还有未软删的门店。
		// 那个数一起回给调用方 —— 契约要它进 Problem 的 detail。
		return fmt.Errorf("region %d 名下还有 %d 家门店: %w",
			id, row.StoreRows, ErrRegionHasStores)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 门店
// ---------------------------------------------------------------------------

const (
	storeCodeIndex    = "uk_stores_code"
	storeDefaultIndex = "uk_stores_default"
)

// storeFromRow 是四条查询共用的一处转换。
//
// 抽出来而不是各写一遍：把哨兵翻成指针这件事写岔一处的症状是「列表里有坐标、
// 详情里没有」，而那看上去像数据不一致。
func storeFromRow(id, regionID int64, regionName, code, name, phone, province,
	city, district, address string, hasLocation bool, lat, lng float64,
	fenceGeoJSON string, isDefault bool, status int16,
	deletedAt, createdAt, updatedAt time.Time, deleted bool) Store {
	s := Store{
		ID: id, RegionID: regionID, RegionName: regionName, Code: code, Name: name,
		Phone: phone, Province: province, City: city, District: district,
		Address: address, IsDefault: isDefault, Status: status,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	if hasLocation {
		la, ln := lat, lng
		s.Lat, s.Lng = &la, &ln
	}
	if fenceGeoJSON != "" {
		f := fenceGeoJSON
		s.FenceGeoJSON = &f
	}
	if deleted {
		d := deletedAt
		s.DeletedAt = &d
	}
	return s
}

func (t tenantTx) AdminListStores(ctx context.Context, regionID *int64, includeDeleted bool,
	limit, offset int32) ([]Store, int64, bool, error) {
	rows, err := t.q.AdminListStores(ctx, db.AdminListStoresParams{
		IncludeDeleted: includeDeleted, RegionID: regionID,
		PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, false, err
	}
	total, err := t.q.AdminCountStores(ctx, db.AdminCountStoresParams{
		IncludeDeleted: includeDeleted, RegionID: regionID,
	})
	if err != nil {
		return nil, 0, false, err
	}
	// has_default 问的是**整个租户**有没有默认门店，不是这一页里有没有 ——
	// 那正是它存在的理由（契约：没配默认店的商家，店面对未授权定位的访客
	// 全是空的）。按页算的话，翻到第二页提示就消失了。
	hasDefault, err := t.q.HasDefaultStore(ctx)
	if err != nil {
		return nil, 0, false, err
	}
	out := make([]Store, 0, len(rows))
	for _, r := range rows {
		out = append(out, storeFromRow(r.ID, r.RegionID, r.RegionName, r.Code, r.Name,
			r.Phone, r.Province, r.City, r.District, r.Address,
			r.HasLocation, r.Lat, r.Lng, r.FenceGeojson, r.IsDefault, r.Status,
			r.DeletedAt.Time, r.CreatedAt.Time, r.UpdatedAt.Time, r.DeletedAt.Valid))
	}
	return out, total, hasDefault, nil
}

func (t tenantTx) FindStore(ctx context.Context, id int64) (Store, error) {
	r, err := t.q.AdminGetStore(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Store{}, fmt.Errorf("store %d: %w", id, ErrCatalogNotFound)
	}
	if err != nil {
		return Store{}, err
	}
	if r.DeletedAt.Valid {
		return Store{}, fmt.Errorf("store %d 已软删: %w", id, ErrCatalogNotFound)
	}
	return storeFromRow(r.ID, r.RegionID, r.RegionName, r.Code, r.Name,
		r.Phone, r.Province, r.City, r.District, r.Address,
		r.HasLocation, r.Lat, r.Lng, r.FenceGeojson, r.IsDefault, r.Status,
		r.DeletedAt.Time, r.CreatedAt.Time, r.UpdatedAt.Time, r.DeletedAt.Valid), nil
}

func (t tenantTx) CreateStore(ctx context.Context, n NewStore) (Store, error) {
	id, err := t.q.CreateStore(ctx, db.CreateStoreParams{
		RegionID: n.RegionID, Code: n.Code, Name: n.Name, Phone: n.Phone,
		Province: n.Province, City: n.City, District: n.District, Address: n.Address,
		Lng: n.Lng, Lat: n.Lat, IsDefault: n.IsDefault,
	})
	switch {
	case isUniqueViolation(err, storeCodeIndex):
		return Store{}, fmt.Errorf("store code %q: %w", n.Code, ErrStoreCodeConflict)
	case isUniqueViolation(err, storeDefaultIndex):
		return Store{}, fmt.Errorf("store code %q: %w", n.Code, ErrDefaultStoreConflict)
	case isForeignKeyViolation(err):
		// region_id 不存在或不属于本租户。契约 422，与 ErrCatalogNotFound 分开：
		// 路径里指名的资源不存在 → 404，请求体里指名的东西不存在 → 422
		// （数据模型 §4 那条分界线）。而 region_id 在请求体里。
		return Store{}, fmt.Errorf("region %d 不存在或不属于当前租户: %w",
			n.RegionID, ErrCatalogBadReference)
	case err != nil:
		return Store{}, err
	}
	return t.FindStore(ctx, id)
}

func (t tenantTx) UpdateStore(ctx context.Context, id int64, p StorePatch) (Store, error) {
	row, err := t.q.UpdateStore(ctx, db.UpdateStoreParams{
		ID: id, RegionID: p.RegionID, Code: p.Code, Name: p.Name, Phone: p.Phone,
		Province: p.Province, City: p.City, District: p.District, Address: p.Address,
		Status: p.Status, SetLocation: p.SetLocation, Lng: p.Lng, Lat: p.Lat,
	})
	switch {
	case isUniqueViolation(err, storeCodeIndex):
		return Store{}, fmt.Errorf("store code: %w", ErrStoreCodeConflict)
	case isForeignKeyViolation(err):
		return Store{}, fmt.Errorf("region 不存在或不属于当前租户: %w", ErrCatalogBadReference)
	case err != nil:
		return Store{}, err
	}
	if row.VisibleRows == 0 || row.UpdatedRows == 0 {
		return Store{}, fmt.Errorf("store %d: %w", id, ErrCatalogNotFound)
	}
	return t.FindStore(ctx, id)
}

func (t tenantTx) SoftDeleteStore(ctx context.Context, id int64) error {
	_, err := t.q.SoftDeleteStore(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store %d: %w", id, ErrCatalogNotFound)
	}
	return err
}

// SetStoreFence 整体替换围栏。geojson 为 nil 即清空。
//
// 三步，顺序有理由：
//
//	① 门店必须在本租户可见 —— 否则 404，而不是先去校验一个谁也用不上的多边形；
//	② 清空只对默认门店合法 —— 否则 409（判定见下面那段）；
//	③ 多边形必须合法 —— ST_IsValid，假则 422 并原样转述 ST_IsValidReason。
//
// ② 排在 ③ 前面：清空那一支根本没有多边形可校验，反过来写会让一次
// 「给非默认店清空围栏」先跑一遍不存在的校验。
func (t tenantTx) SetStoreFence(ctx context.Context, id int64, geojson *string) (Store, error) {
	st, err := t.FindStore(ctx, id)
	if err != nil {
		return Store{}, err
	}
	// **非默认门店不许清空围栏**（契约 409 store-fence-required）。
	//
	// 这一条原本是 stores 上的一条 CHECK (is_default OR fence IS NOT NULL)。
	// 落地时对着真库跑出来它挡住的是两条主路径（建一家普通店、切换默认店），
	// 完整论证与实测记在 00020 那张表的定义上。所以它退成了这一处判定：
	// 只管 PUT .../fence 这一条路径，别的路径不再被数据库挡着。
	//
	// 退让的是什么，说清楚：这中间有一个窗口 —— 这次 FindStore 与下面那条
	// UPDATE 之间，另一个事务可以把这家店的默认位抢走（MakeStoreDefault 会清
	// 旧的那家），于是一次「给默认店清空围栏」会落在一家已经不是默认的店上。
	// 两条都在 WithTenant 的同一个事务里跑，READ COMMITTED 下后写的那个会看到
	// 前者已提交的结果，所以它是一个真实但极窄的竞态，后果是多出一家
	// 「未完成」的门店 —— 而那个状态契约本来就允许存在、后台本来就会提示。
	// 用一条 CHECK 去关死这个窗口的代价，上面那两条主路径已经付过一次了。
	if geojson == nil && !st.IsDefault {
		return Store{}, fmt.Errorf("store %d: %w", id, ErrStoreFenceRequired)
	}
	if geojson != nil {
		v, err := t.q.CheckPolygonValidity(ctx, *geojson)
		if err != nil {
			// ST_GeomFromGeoJSON 对一段解不开的 GeoJSON 直接抛错。
			// 它与「解得开但自交」在契约里是同一个 problem type（invalid-fence），
			// 所以翻成同一个错误类型，只是 reason 来自 PostgreSQL 的错误消息。
			return Store{}, &InvalidFenceError{Reason: pgMessage(err)}
		}
		if !v.Valid {
			return Store{}, &InvalidFenceError{Reason: v.Reason}
		}
	}
	if _, err := t.q.SetStoreFence(ctx, db.SetStoreFenceParams{ID: id, Geojson: geojson}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Store{}, fmt.Errorf("store %d: %w", id, ErrCatalogNotFound)
		}
		return Store{}, err
	}
	return t.FindStore(ctx, id)
}

// MakeStoreDefault 把这一家设成默认门店。
//
// **同一个事务里先清旧、再置新**，顺序不能反 —— uk_stores_default 是一条部分
// 唯一索引，先置新再清旧会自己撞自己。这个方法本身就跑在 WithTenant 的事务里，
// 所以两条语句的原子性由那个事务保证，不需要在这里再开一个。
//
// 零行分成两支：门店不存在 → 404；存在但停业 / 已软删 → 409。
// 分不开的话，后台点「设为默认」得到 404，而那家店就在列表里摆着。
func (t tenantTx) MakeStoreDefault(ctx context.Context, id int64) (Store, error) {
	st, err := t.FindStore(ctx, id)
	if err != nil {
		return Store{}, err
	}
	if st.Status != 1 {
		return Store{}, fmt.Errorf("store %d 状态 %d: %w", id, st.Status, ErrStoreUnavailable)
	}
	if err := t.q.ClearDefaultStore(ctx); err != nil {
		return Store{}, err
	}
	if _, err := t.q.SetDefaultStore(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// FindStore 与这条语句之间那家店被停业或软删了。仍然是 409：
			// 调用方看到的事实与服务端此刻的事实不一致，重读一次再试是对的。
			return Store{}, fmt.Errorf("store %d: %w", id, ErrStoreUnavailable)
		}
		return Store{}, err
	}
	return t.FindStore(ctx, id)
}

func (t tenantTx) StoreScope(ctx context.Context, id int64) (int64, int64, error) {
	r, err := t.q.StoreExists(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("store %d: %w", id, ErrCatalogNotFound)
	}
	if err != nil {
		return 0, 0, err
	}
	return r.ID, r.RegionID, nil
}

// ---------------------------------------------------------------------------
// 买家侧
// ---------------------------------------------------------------------------

func (t tenantTx) ListOpenStores(ctx context.Context, limit, offset int32) ([]StoreMatch, int64, error) {
	rows, err := t.q.ListOpenStores(ctx, db.ListOpenStoresParams{
		PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.CountOpenStores(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]StoreMatch, 0, len(rows))
	for _, r := range rows {
		out = append(out, matchFrom(r.ID, r.Name, r.Phone, r.Address, r.IsDefault,
			r.HasLocation, r.Lat, r.Lng, false, 0))
	}
	return out, total, nil
}

// ResolveStoresByPoint 返回围栏命中的门店，**按距离升序**。零行是一个正常结果
// （这个坐标不在任何围栏内），由调用方回落到默认门店。
func (t tenantTx) ResolveStoresByPoint(ctx context.Context, lat, lng float64, limit int32) ([]StoreMatch, error) {
	rows, err := t.q.ResolveStoresByFence(ctx, db.ResolveStoresByFenceParams{
		Lng: lng, Lat: lat, PageLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]StoreMatch, 0, len(rows))
	for _, r := range rows {
		out = append(out, matchFrom(r.ID, r.Name, r.Phone, r.Address, r.IsDefault,
			r.HasLocation, r.Lat, r.Lng, r.HasLocation, r.DistanceM))
	}
	return out, nil
}

// DefaultStore 取回落目标，同时给出它所在的大区。
//
// 零行是一个正常结果（这家商家没配默认店），返回 ErrCatalogNotFound ——
// 由调用方翻成 match_type = none + 空数组 + HTTP 200，**不是 404**：
// 「不在服务范围」是一个正常的查询结果，不是错误。
func (t tenantTx) DefaultStore(ctx context.Context) (StoreMatch, int64, error) {
	r, err := t.q.GetDefaultStore(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoreMatch{}, 0, fmt.Errorf("本租户没有默认门店: %w", ErrCatalogNotFound)
	}
	if err != nil {
		return StoreMatch{}, 0, err
	}
	return matchFrom(r.ID, r.Name, r.Phone, r.Address, r.IsDefault,
		r.HasLocation, r.Lat, r.Lng, false, 0), r.RegionID, nil
}

// matchFrom 把哨兵翻成指针。hasDistance 单独一个参数而不是「distance >= 0」：
// 0 米是一个合法的距离（买家正站在门店里），拿它当哨兵会让最近的那一家
// 显示成「距离未知」。
func matchFrom(id int64, name, phone, address string, isDefault, hasLocation bool,
	lat, lng float64, hasDistance bool, distance float64) StoreMatch {
	m := StoreMatch{ID: id, Name: name, Phone: phone, Address: address, IsDefault: isDefault}
	if hasLocation {
		la, ln := lat, lng
		m.Lat, m.Lng = &la, &ln
	}
	if hasDistance && hasLocation {
		d := distance
		m.DistanceM = &d
	}
	return m
}
