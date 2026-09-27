package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 门店 / 大区后台那 21 条接口共用的东西：一个 handler 类型、**一张**错误映射表，
// 以及把领域类型装成契约类型的几个函数。00020，契约 Store tag 的 /admin/ 那一段。
//
// ===========================================================================
// 为什么错误映射只能有一张表
// ===========================================================================
//
// 与 admin_catalog.go 那一处一字不差的理由：这 21 条共用同一批业务错误
// （ErrCatalogNotFound 出现在其中 15 条上），而契约对它们的状态码要求一致。
// 分成几份之后，两边对同一个业务错误给出不同状态码的那天不会有任何东西变红。
//
// 这里有**三对**混掉会让客户端做错事，逐对写在 internal/problem 那一组常量上：
// store-code-conflict / default-store-conflict、store-fence-required /
// invalid-fence、inventory-precondition-failed / store-ambiguous。
//
// ===========================================================================
// 文件怎么分：由 contract_test.go 的参数对账决定，不由行数决定
// ===========================================================================
//
// contract_test.go 的 queryParamsReadByHandler 按 route.HandlerFile 解析**整份
// 源码**里的 c.Query 调用，然后与**每一条**路由声明的 query 参数集合两向对账。
// 也就是说：**同一个文件里的两条路由，query 参数集合必须相同**，
// 否则参数多的那条会让参数少的那条被判成「handler 读了一个契约里没有的参数」。
//
// 这 21 条里有 5 条带 query 参数，而它们的集合两两不同（除了两条 products
// 列表），所以拆成：
//
//	admin_region_list.go        GET /admin/regions             {page, page_size, include_deleted}
//	admin_store_list.go         GET /admin/stores              {page, page_size, region_id, include_deleted}
//	admin_scoped_products.go    两条 .../products              {page, page_size, listed}
//	admin_store_inventories.go  GET .../inventories            {page, page_size, low_stock_only}
//	（这个文件）                 其余 16 条                      一个都没有
//
// 拆得细不是为了让文件短 —— 是那条对账测试的直接后果，而它抓的是真实漂移。

// AdminStoreHandler 实现契约 Store tag 里 /admin/ 前缀那 21 条。
type AdminStoreHandler struct{ svc *service.AdminStoreService }

func NewAdminStoreHandler(s *service.AdminStoreService) *AdminStoreHandler {
	return &AdminStoreHandler{svc: s}
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

// writeStoreError 把业务错误翻成契约里那几种响应。
//
// **顺序要紧的有两处**：
//
//	① *repository.StoreInventoryConflict 的 errors.As 要排在
//	   errors.Is(ErrInventoryPrecondition) 前面 —— 后者是前者的 Unwrap 目标，
//	   反过来写的话 409 仍然是 409，但响应体里那个 current 会消失，
//	   而契约把它定成必填（InventoryConflict schema）。
//	② *repository.InvalidFenceError 的 errors.As 要在兜底之前 —— 它不是
//	   sentinel，漏了就掉进 500，而那句 ST_IsValidReason 是运营唯一能拿来
//	   定位自己画错在哪儿的东西。
func writeStoreError(c *gin.Context, err error) {
	if writePermissionError(c, err) || writeInventoryUnavailable(c, err) {
		return
	}
	var invConflict *repository.StoreInventoryConflict
	var badFence *repository.InvalidFenceError

	switch {
	case errors.Is(err, service.ErrCatalogBadRequest):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求参数不合法")

	// —— 404。契约 /admin/ 段头的约定 3：查不到当前租户名下的那一个一律 404
	// 而不是 403 —— 403 会让自增 id 空间变成一个跨租户的存在性探针。
	case errors.Is(err, repository.ErrCatalogNotFound):
		problem.Write(c, http.StatusNotFound,
			problem.TypeNotFound, "目标不存在或不属于当前租户")

	case errors.Is(err, repository.ErrSKUNotInTenant):
		problem.Write(c, http.StatusNotFound,
			problem.TypeNotFound, "SKU 不存在、不属于当前租户，或已被软删")

	// —— 422：请求体里指名的东西不存在。与上面那组 404 的分界线是数据模型 §4
	// 那一条：**路径里指名的资源不存在 → 404；请求体里指名的东西不存在 → 422**。
	// 合成一个的话，POST /admin/stores 里一个错的 region_id 会让整条端点回 404，
	// 而客户端会以为 /admin/stores 这个 URL 不存在。
	case errors.Is(err, repository.ErrCatalogBadReference):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体里引用的对象不存在或不属于当前租户")

	case errors.As(err, &badFence):
		// 多边形本身画错了。**detail 转述 PostGIS 的 ST_IsValidReason** ——
		// 实测领结形 [[0,0],[1,1],[1,0],[0,1],[0,0]] 的 reason 是
		// "Self-intersection at or near point 0.5 0.5"，那句话是运营唯一能
		// 拿来定位自己画错在哪儿的东西。丢掉它，422 就只剩「你画的不对」。
		//
		// 这是全仓库唯一一处把 PostgreSQL 的错误消息转述给调用方的地方，
		// 而且只在**校验**失败这一支上 —— 别处那么做一般是在泄露 schema。
		//
		// **原因放 detail，不放 title**（契约原话：detail 转述 ST_IsValidReason）。
		// 第一版放在了 title 里，对应的测试也照着断言 title，于是两边一起错、
		// 闸门一直绿 —— 后台界面是两个字段都去找才碰巧能用。按 RFC 7807，title 是
		// 同一个 type 下**固定不变**的一句话，每次出错的具体情况属于 detail；
		// 客户端按 title 分组统计或者做本地化时，一个每次都不同的 title 会把
		// 同一类错误拆成成百上千种。
		reason := badFence.Reason
		problem.WriteValue(c, http.StatusUnprocessableEntity, api.Problem{
			Type:   problem.TypeInvalidFence,
			Title:  "围栏不是一个合法的多边形",
			Status: http.StatusUnprocessableEntity,
			Detail: &reason,
		})

	// —— 409。状态码分不开它们，契约在每条端点上都写着「按 type 区分」。
	case errors.As(err, &invConflict):
		// 按门店那条 CAS 对不上。响应体是 InventoryConflict（Problem + 必填的
		// current），用生成类型而不是手拼 map：契约改字段名时这里当场编译失败。
		problem.WriteValue(c, http.StatusConflict, api.InventoryConflict{
			Type:    problem.TypeInventoryPrecondition,
			Title:   "库存的 expected_available_qty 与当前值不符",
			Status:  http.StatusConflict,
			Current: apiStoreInventory(invConflict.Current),
		})

	case errors.Is(err, repository.ErrRegionCodeConflict):
		problem.Write(c, http.StatusConflict,
			problem.TypeRegionCodeConflict, "大区编号在本租户内已存在")

	case errors.Is(err, repository.ErrRegionHasStores):
		problem.Write(c, http.StatusConflict,
			problem.TypeRegionHasStores, "这个大区名下还有未软删的门店，请先把它们挪走或删掉")

	case errors.Is(err, repository.ErrStoreCodeConflict):
		problem.Write(c, http.StatusConflict,
			problem.TypeStoreCodeConflict, "门店编号在本租户内已存在")

	case errors.Is(err, repository.ErrDefaultStoreConflict):
		// **不是「顺手切换」**：切换默认店要先清旧再置新，那是
		// PUT /admin/stores/{store_id}/default 的事，它在同一个事务里做两步。
		// 让 POST 顺手抢过默认位，等于给「建一家店」附带一个谁也没预料到的副作用。
		problem.Write(c, http.StatusConflict, problem.TypeDefaultStoreConflict,
			"已经有一家默认门店了；切换请用 PUT /admin/stores/{store_id}/default")

	case errors.Is(err, repository.ErrStoreFenceRequired):
		problem.Write(c, http.StatusConflict, problem.TypeStoreFenceRequired,
			"非默认门店必须有围栏，清空会让它永远接不到单")

	case errors.Is(err, repository.ErrStoreUnavailable):
		problem.Write(c, http.StatusConflict, problem.TypeStoreUnavailable,
			"这家门店已停业或已软删，不能作为回落目标")

	case errors.Is(err, repository.ErrStoreAmbiguous):
		problem.Write(c, http.StatusConflict, problem.TypeStoreAmbiguous,
			"本租户的门店数不是 1，这条路径无法确定是哪一家")

	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
	}
}

// ---------------------------------------------------------------------------
// 领域类型 → 契约类型
// ---------------------------------------------------------------------------
//
// 一律用 internal/api 里生成的类型（CONTRIBUTING 硬规矩二）：契约里把某个字段
// 改名或挪出 required，这里当场编译失败，而不是等线上客户端解析失败。

func apiAdminRegion(r repository.Region) api.AdminRegion {
	out := api.AdminRegion{
		Id:         r.ID,
		Code:       r.Code,
		Name:       r.Name,
		Status:     api.AdminRegionStatus(r.Status),
		StoreCount: int(r.StoreCount),
		DeletedAt:  r.DeletedAt,
		CreatedAt:  r.CreatedAt,
	}
	if !r.UpdatedAt.IsZero() {
		u := r.UpdatedAt
		out.UpdatedAt = &u
	}
	return out
}

func apiAdminStore(s repository.Store) api.AdminStore {
	out := api.AdminStore{
		Id:        s.ID,
		RegionId:  s.RegionID,
		Code:      s.Code,
		Name:      s.Name,
		IsDefault: s.IsDefault,
		Status:    api.AdminStoreStatus(s.Status),
		DeletedAt: s.DeletedAt,
		CreatedAt: s.CreatedAt,
	}
	// 空串与缺席是两件事，而契约把这几个定成可选（omitempty）：一家没填
	// 电话的门店应当是「没有电话」，不是「电话是空字符串」——
	// 后者会让客户端渲染一个空白的拨号按钮。
	out.RegionName = optStr(s.RegionName)
	out.Phone = optStr(s.Phone)
	out.Province = optStr(s.Province)
	out.City = optStr(s.City)
	out.District = optStr(s.District)
	out.Address = optStr(s.Address)
	out.Lat, out.Lng = optCoord(s.Lat), optCoord(s.Lng)
	out.Fence = decodeFence(s.FenceGeoJSON)
	if !s.UpdatedAt.IsZero() {
		u := s.UpdatedAt
		out.UpdatedAt = &u
	}
	return out
}

func apiScopedListing(l repository.ScopedListing) api.ScopedProductListing {
	min, max := api.Money(l.MinPriceCents), api.Money(l.MaxPriceCents)
	st := api.ScopedProductListingStatus(l.Status)
	return api.ScopedProductListing{
		ProductId:       l.ProductID,
		Title:           l.Title,
		Status:          &st,
		Listed:          l.Listed,
		EffectiveListed: l.EffectiveListed,
		MinPriceCents:   &min,
		MaxPriceCents:   &max,
		PriceSource:     api.ScopedProductListingPriceSource(l.PriceSource),
	}
}

func apiScopedPrice(p repository.ScopedPrice) api.ScopedSkuPrice {
	base := api.Money(p.BasePriceCents)
	return api.ScopedSkuPrice{
		SkuId:          p.SKUID,
		SkuCode:        optStr(p.SKUCode),
		BasePriceCents: &base,
		// OverridePriceCents 为 nil 表示**这一层没有覆盖**，沿用上一层 ——
		// 那正是两张覆盖表「缺一行即继承」的语义。原样传指针：
		// 把它折成 0 会让「没定价」看起来像「定价 0 元」。
		OverridePriceCents:  p.OverridePriceCents,
		EffectivePriceCents: api.Money(p.EffectivePriceCents),
		PriceSource:         api.ScopedSkuPricePriceSource(p.PriceSource),
	}
}

func apiStoreInventory(in repository.StoreInventory) api.AdminInventory {
	return api.AdminInventory{
		SkuId:        in.SKUID,
		StoreId:      in.StoreID,
		SkuCode:      optStr(in.SKUCode),
		AvailableQty: int(in.AvailableQty),
		WarningQty:   int(in.WarningQty),
		UpdatedAt:    in.UpdatedAt,
	}
}

// optStr 把空串翻成缺席。
func optStr(v string) *string {
	if v == "" {
		return nil
	}
	s := v
	return &s
}

// optCoord 把 repository 的 *float64 收窄成契约的 *float32。
//
// **这一步会掉精度**，而且是有意接受的：契约把 lat / lng 写成裸 `type: number`
// （没有 format: double），生成器据此产出 float32，约 7 位有效数字 ——
// 在 116.3974 这个量级上大约 1 米。它只影响**回显**：围栏判定与距离排序都在
// PostGIS 里用 float64 算（stores.location 是 GEOGRAPHY(POINT, 4326)），
// 客户端拿这个值去画地图上的一个点，1 米的偏差看不出来。
//
// 真要改成 float64，改的是契约里那两行 format，然后重跑三个生成物 ——
// 那是一次契约变更，不该在这里用一个 any 悄悄绕过去。
func optCoord(v *float64) *float32 {
	if v == nil {
		return nil
	}
	f := float32(*v)
	return &f
}

// ---------------------------------------------------------------------------
// 围栏：GeoJSON 字符串 ⇄ 契约的 GeoPolygon
// ---------------------------------------------------------------------------
//
// 库里那一列是 GEOGRAPHY(POLYGON, 4326)，进出都走 GeoJSON 文本
// （ST_GeomFromGeoJSON / ST_AsGeoJSON，见 db/queries/stores.sql 的文件头）。
// 所以这两个函数是契约类型与那段文本之间唯一的桥。
//
// **不自己判合法性。** 环闭没闭、有没有自交、顶点够不够，一律交给
// PostGIS 的 ST_IsValid（repository.SetStoreFence 那一步）：自己写一遍的话，
// 两份判据会在「多边形跨过日期变更线」这类情况上分叉，而分叉的那一侧
// 会让一个 PostGIS 认为非法的围栏落库，之后每次 ST_Intersects 的行为都未定义。

// geoJSONPolygon 是 RFC 7946 的 Polygon，字段名逐字。
type geoJSONPolygon struct {
	Type        string        `json:"type"`
	Coordinates [][][]float64 `json:"coordinates"`
}

// encodeFence 把契约的 GeoPolygon 编成 ST_GeomFromGeoJSON 吃的那段文本。
func encodeFence(p *api.GeoPolygon) (*string, error) {
	if p == nil {
		return nil, nil
	}
	g := geoJSONPolygon{Type: string(p.Type), Coordinates: make([][][]float64, 0, len(p.Coordinates))}
	if g.Type == "" {
		// 契约把 type 定成 required + enum: ["Polygon"]，但 JSON 解析不校验
		// 枚举。空串传给 ST_GeomFromGeoJSON 会得到一句
		// "unknown GeoJSON type"，那句话对调用方没用。
		return nil, fmt.Errorf("%w: fence.type 必须是 \"Polygon\"", service.ErrCatalogBadRequest)
	}
	for _, ring := range p.Coordinates {
		r := make([][]float64, 0, len(ring))
		for _, pt := range ring {
			if len(pt) != 2 {
				return nil, fmt.Errorf("%w: 围栏的每个顶点必须是 [经度, 纬度] 两个数，实得 %d 个",
					service.ErrCatalogBadRequest, len(pt))
			}
			// 顺序是 [经度, 纬度]，GeoJSON 的规定。反了的话在中国境内会得到
			// 一个落在南极洲以南的点 —— 而 ST_IsValid 对它是满意的
			// （它只看拓扑），于是围栏合法落库、一个买家都命不中。
			r = append(r, []float64{float64(pt[0]), float64(pt[1])})
		}
		g.Coordinates = append(g.Coordinates, r)
	}
	b, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// decodeFence 把 ST_AsGeoJSON 的输出翻回契约类型。
//
// 解不开时返回 nil 而不是报错：这条路径是**读**，而一段解不开的 GeoJSON
// 只可能来自 PostGIS 自己的输出 —— 那是服务端的 bug，不该把整条列表打成 500，
// 让运营连别的门店都看不到。代价是那家店的围栏显示成「未配置」，
// 而 fence = null + is_default = false 在后台本来就要挂「未完成」提示。
func decodeFence(raw *string) *api.GeoPolygon {
	if raw == nil || *raw == "" {
		return nil
	}
	var g geoJSONPolygon
	if err := json.Unmarshal([]byte(*raw), &g); err != nil {
		return nil
	}
	out := api.GeoPolygon{
		Type:        api.GeoPolygonType(g.Type),
		Coordinates: make([][][]float32, 0, len(g.Coordinates)),
	}
	for _, ring := range g.Coordinates {
		r := make([][]float32, 0, len(ring))
		for _, pt := range ring {
			p := make([]float32, 0, len(pt))
			for _, v := range pt {
				p = append(p, float32(v))
			}
			r = append(r, p)
		}
		out.Coordinates = append(out.Coordinates, r)
	}
	return &out
}

// ---------------------------------------------------------------------------
// 大区：三条写接口
// ---------------------------------------------------------------------------

// CreateRegion 实现 POST /api/v1/admin/regions。
func (h *AdminStoreHandler) CreateRegion(c *gin.Context) {
	var req api.RegionCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	r, err := h.svc.CreateRegion(c.Request.Context(),
		repository.NewRegion{Code: req.Code, Name: req.Name})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, apiAdminRegion(r))
}

// UpdateRegion 实现 PATCH /api/v1/admin/regions/{region_id}。
func (h *AdminStoreHandler) UpdateRegion(c *gin.Context) {
	id, ok := pathID(c, "region_id")
	if !ok {
		return
	}
	var req api.RegionUpdateRequest
	if !bindJSON(c, &req) {
		return
	}
	p := repository.RegionPatch{Code: req.Code, Name: req.Name}
	if req.Status != nil {
		st := int16(*req.Status)
		p.Status = &st
	}
	r, err := h.svc.UpdateRegion(c.Request.Context(), id, p)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminRegion(r))
}

// DeleteRegion 实现 DELETE /api/v1/admin/regions/{region_id}。
func (h *AdminStoreHandler) DeleteRegion(c *gin.Context) {
	id, ok := pathID(c, "region_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteRegion(c.Request.Context(), id); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 门店：六条
// ---------------------------------------------------------------------------

// CreateStore 实现 POST /api/v1/admin/stores。
func (h *AdminStoreHandler) CreateStore(c *gin.Context) {
	var req api.StoreCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	n := repository.NewStore{
		RegionID: req.RegionId,
		Code:     req.Code,
		Name:     req.Name,
		Phone:    derefStr(req.Phone),
		Province: derefStr(req.Province),
		City:     derefStr(req.City),
		District: derefStr(req.District),
		Address:  derefStr(req.Address),
		Lat:      wideCoord(req.Lat),
		Lng:      wideCoord(req.Lng),
	}
	if req.IsDefault != nil {
		n.IsDefault = *req.IsDefault
	}
	st, err := h.svc.CreateStore(c.Request.Context(), n)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, apiAdminStore(st))
}

// Detail 实现 GET /api/v1/admin/stores/{store_id}（含围栏）。
func (h *AdminStoreHandler) Detail(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	st, err := h.svc.FindStore(c.Request.Context(), id)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminStore(st))
}

// UpdateStore 实现 PATCH /api/v1/admin/stores/{store_id}。
func (h *AdminStoreHandler) UpdateStore(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	var req api.StoreUpdateRequest
	if !bindJSON(c, &req) {
		return
	}
	p := repository.StorePatch{
		RegionID: req.RegionId,
		Code:     req.Code,
		Name:     req.Name,
		Phone:    req.Phone,
		Province: req.Province,
		City:     req.City,
		District: req.District,
		Address:  req.Address,
	}
	if req.Status != nil {
		st := int16(*req.Status)
		p.Status = &st
	}
	// SetLocation 是「这次请求要不要动坐标」，与 Lat / Lng 的值分开。
	// 少了它的话，一次不带坐标的 PATCH 会把 Lat / Lng 传成 nil，
	// 而 repository 分不清「不动」与「清空」—— 清空一家店的坐标
	// 会让它在按距离排序里变成「距离未知」，永远排在最后。
	if req.Lat != nil || req.Lng != nil {
		p.SetLocation = true
		p.Lat, p.Lng = wideCoord(req.Lat), wideCoord(req.Lng)
	}
	st, err := h.svc.UpdateStore(c.Request.Context(), id, p)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminStore(st))
}

// DeleteStore 实现 DELETE /api/v1/admin/stores/{store_id}。
func (h *AdminStoreHandler) DeleteStore(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteStore(c.Request.Context(), id); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// SetFence 实现 PUT /api/v1/admin/stores/{store_id}/fence。
//
// 请求体里 fence 是 required 且允许为 null —— **传 null 即清空**，
// 与「不传这个字段」是两件事。生成类型把它做成 *GeoPolygon，
// 两种输入在 Go 这一侧长得一样（都是 nil），而契约把不传定成 422。
// 这里不额外区分：bindJSON 之后一个缺席的 required 字段与显式 null 无法分辨，
// 而两者的意图（清空）在这条端点上恰好一致 —— 清空对非默认门店仍然是 409，
// 那道判据在 repository.SetStoreFence 里。
func (h *AdminStoreHandler) SetFence(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	var req api.StoreFenceRequest
	if !bindJSON(c, &req) {
		return
	}
	geojson, err := encodeFence(req.Fence)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	st, err := h.svc.SetFence(c.Request.Context(), id, geojson)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminStore(st))
}

// MakeDefault 实现 PUT /api/v1/admin/stores/{store_id}/default。
func (h *AdminStoreHandler) MakeDefault(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	st, err := h.svc.MakeDefault(c.Request.Context(), id)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminStore(st))
}

// ---------------------------------------------------------------------------
// 两个作用域下的上下架
// ---------------------------------------------------------------------------

// SetStoreListing 实现 PUT /api/v1/admin/stores/{store_id}/products/{product_id}/listing。
func (h *AdminStoreHandler) SetStoreListing(c *gin.Context) {
	storeID, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	productID, ok := pathID(c, "product_id")
	if !ok {
		return
	}
	var req api.ProductListingRequest
	if !bindJSON(c, &req) {
		return
	}
	l, err := h.svc.SetStoreListing(c.Request.Context(), storeID, productID, req.Listed)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiScopedListing(l))
}

// SetRegionListing 实现 PUT /api/v1/admin/regions/{region_id}/products/{product_id}/listing。
func (h *AdminStoreHandler) SetRegionListing(c *gin.Context) {
	regionID, ok := pathID(c, "region_id")
	if !ok {
		return
	}
	productID, ok := pathID(c, "product_id")
	if !ok {
		return
	}
	var req api.ProductListingRequest
	if !bindJSON(c, &req) {
		return
	}
	l, err := h.svc.SetRegionListing(c.Request.Context(), regionID, productID, req.Listed)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiScopedListing(l))
}

// ---------------------------------------------------------------------------
// 三层定价的内两层：四条
// ---------------------------------------------------------------------------

// SetStorePrice 实现 PUT /api/v1/admin/stores/{store_id}/skus/{sku_id}/price。
func (h *AdminStoreHandler) SetStorePrice(c *gin.Context) {
	storeID, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	skuID, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	var req api.SkuPriceSetRequest
	if !bindJSON(c, &req) {
		return
	}
	p, err := h.svc.SetStorePrice(c.Request.Context(), storeID, skuID, int64(req.PriceCents))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiScopedPrice(p))
}

// ClearStorePrice 实现 DELETE /api/v1/admin/stores/{store_id}/skus/{sku_id}/price。
func (h *AdminStoreHandler) ClearStorePrice(c *gin.Context) {
	storeID, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	skuID, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	if err := h.svc.ClearStorePrice(c.Request.Context(), storeID, skuID); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// SetRegionPrice 实现 PUT /api/v1/admin/regions/{region_id}/skus/{sku_id}/price。
func (h *AdminStoreHandler) SetRegionPrice(c *gin.Context) {
	regionID, ok := pathID(c, "region_id")
	if !ok {
		return
	}
	skuID, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	var req api.SkuPriceSetRequest
	if !bindJSON(c, &req) {
		return
	}
	p, err := h.svc.SetRegionPrice(c.Request.Context(), regionID, skuID, int64(req.PriceCents))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiScopedPrice(p))
}

// ClearRegionPrice 实现 DELETE /api/v1/admin/regions/{region_id}/skus/{sku_id}/price。
func (h *AdminStoreHandler) ClearRegionPrice(c *gin.Context) {
	regionID, ok := pathID(c, "region_id")
	if !ok {
		return
	}
	skuID, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	if err := h.svc.ClearRegionPrice(c.Request.Context(), regionID, skuID); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 按门店的库存写面
// ---------------------------------------------------------------------------

// SetStoreInventory 实现 PUT /api/v1/admin/stores/{store_id}/skus/{sku_id}/inventory。
func (h *AdminStoreHandler) SetStoreInventory(c *gin.Context) {
	storeID, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	skuID, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	var req api.InventorySetRequest
	if !bindJSON(c, &req) {
		return
	}
	in := repository.InventorySet{
		AvailableQty:         int32(req.AvailableQty),
		ExpectedAvailableQty: int32(req.ExpectedAvailableQty),
	}
	if req.WarningQty != nil {
		w := int32(*req.WarningQty)
		in.WarningQty = &w
	}
	inv, err := h.svc.SetStoreInventory(c.Request.Context(), storeID, skuID, in)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiStoreInventory(inv))
}

// AdjustStoreInventory 实现 POST /api/v1/admin/stores/{store_id}/skus/{sku_id}/inventory/adjustments
// （相对调整，Idempotency-Key 必填）。
func (h *AdminStoreHandler) AdjustStoreInventory(c *gin.Context) {
	storeID, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	skuID, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	var req api.InventoryAdjustRequest
	if !bindJSON(c, &req) {
		return
	}
	inv, replayed, err := h.svc.AdjustStoreInventory(c.Request.Context(), storeID, skuID,
		inventoryAdjustInput(req), idemKeyOf(c))
	if err != nil {
		writeInventoryAdjustError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusOK, apiStoreInventory(inv))
}

// inventoryAdjustInput 把契约类型收成业务层的形状。两条相对调整的路径共用。
func inventoryAdjustInput(req api.InventoryAdjustRequest) service.InventoryAdjustInput {
	return service.InventoryAdjustInput{Delta: req.Delta, Reason: req.Reason}
}

// writeInventoryAdjustError 是两条相对调整路径的错误映射：先接住这条接口独有的
// 两组（幂等、扣完会变负），其余交给 writeStoreError —— 404、判权、store-ambiguous
// 都与 PUT 那两条同一套翻法，不另抄一份。
//
// 两条路径用同一个映射（单店捷径也是），而不是各用自己文件的 writeCatalogError /
// writeStoreError：它们的业务是同一个函数（service.adjustInventory），
// 同一个错误在两条路径上翻出两种响应，调用方没法写一份处理逻辑。
func writeInventoryAdjustError(c *gin.Context, err error) {
	var short *repository.InventoryInsufficient
	switch {
	case writeInventoryUnavailable(c, err):
	case writeAdminIdempotencyError(c, err):
	case errors.As(err, &short):
		// 扣完会变负。与 CAS 那条共用 InventoryConflict 响应体（带 current），
		// type 不同：那一条重读重试会成功，这一条原样重试不会。
		detail := fmt.Sprintf("当前可售 %d，调整 %d 之后会变负", short.Current.AvailableQty, short.Delta)
		problem.WriteValue(c, http.StatusConflict, api.InventoryConflict{
			Type:    problem.TypeInventoryInsufficient,
			Title:   "库存不够扣，调整之后会变负",
			Status:  http.StatusConflict,
			Detail:  &detail,
			Current: apiStoreInventory(short.Current),
		})
	default:
		writeStoreError(c, err)
	}
}

// derefStr 把可选字符串摊平成裸串。repository 的 NewStore 用裸串表示
// 「没填」（空串），因为这几列在库里是 NOT NULL DEFAULT ”。
func derefStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// wideCoord 是 optCoord 的反向：契约的 float32 → repository 的 float64。
// 这个方向不掉精度。
func wideCoord(v *float32) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

// 这个文件里不该出现任何 c.Query —— 上面那 16 条在 routes 表里全登记着
// NoQueryParams，加一个 c.Query 会让**每一条**都被判成「读了一个契约里
// 没有的参数」。带 query 参数的四条在各自的文件里。
