package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 买家侧门店那两条（GET /stores、GET /stores/resolve）。数据模型 §4。
//
// 两条**不在同一个文件里**：contract_test.go 的参数对账按文件做
// （admin_region_list.go 的文件头写了完整推理），而这条读 {page, page_size}、
// resolve 读 {lat, lng, size}，两个集合不交。resolve 在 store_resolve.go。
//
// 两条在契约里都是 security: []（公开的）：还没登录的人要看得到「谁服务你」，
// 否则小程序首页在拿到门店之前什么都渲染不出来。**这不等于它们不校验租户** ——
// 租户由 res.Middleware() 从 Host 定出来，门店表上的 RLS 挡住跨店结果。

// StoreHandler 实现买家侧那两条。
type StoreHandler struct{ svc *service.StoreService }

func NewStoreHandler(s *service.StoreService) *StoreHandler { return &StoreHandler{svc: s} }

// writeStoreQueryError 是这两条读接口的错误映射。
//
// 与后台那张 writeStoreError 分开，因为**同一个业务错误在两侧的状态码不同**：
// 后台的「门店不存在」是路径参数，404；买家侧的 store_id 在 query 里，422
// （数据模型 §4 那条分界线）。共用一张表就得在表里分辨调用方是谁，
// 而那正是分成两张表要避免的东西。
func writeStoreQueryError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidCoord):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "lat 与 lng 必须同时给，且取值要在合法范围内")

	case errors.Is(err, service.ErrStoreNotFound),
		errors.Is(err, repository.ErrCatalogNotFound):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "store_id 指向的门店不存在或不属于当前店铺")

	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
	}
}

// apiStore 把买家侧的门店装成契约的 Store。
//
// 与后台的 AdminStore 分成两个类型：围栏、停业状态、软删标记是运营要管的，
// 塞进买家类型等于让生成出来的买家客户端带着一组它永远收不到、
// 也不该收到的字段 —— 而客户端会照着类型去渲染。
func apiStore(m repository.StoreMatch) api.Store {
	return api.Store{
		Id:        m.ID,
		Name:      m.Name,
		Phone:     optStr(m.Phone),
		Address:   optStr(m.Address),
		IsDefault: m.IsDefault,
		Lat:       optCoord(m.Lat),
		Lng:       optCoord(m.Lng),
	}
}

// List 实现 GET /api/v1/stores。
//
// 这条端点的存在理由是 resolve 的回落：拒绝定位的买家会被落到默认门店，
// 而他应该能自己改。没有这条，「回落」就成了一个用户无法纠正的结果。
func (h *StoreHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	out, err := h.svc.ListOpen(c.Request.Context(), page, pageSize)
	if err != nil {
		writeStoreQueryError(c, err)
		return
	}
	items := make([]api.Store, 0, len(out.Items))
	for _, m := range out.Items {
		items = append(items, apiStore(m))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.Store `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
