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

// 地址簿（契约 User tag）：
//
//	GET    /addresses                         地址簿
//	POST   /addresses                         新增（Idempotency-Key 必填）
//	GET    /addresses/{address_id}            详情
//	PUT    /addresses/{address_id}            整体替换
//	DELETE /addresses/{address_id}            软删
//	PUT    /addresses/{address_id}/default    设为默认
//
// 越权、默认地址互斥这些规则在 service 与 SQL 里；这一层只做绑定与渲染。
// 别人的地址与不存在的地址都是 404（契约 AddressId 参数的描述）。

type AddressHandler struct{ svc *service.AddressService }

func NewAddressHandler(s *service.AddressService) *AddressHandler { return &AddressHandler{svc: s} }

// List 实现 GET /api/v1/addresses。
func (h *AddressHandler) List(c *gin.Context) {
	list, err := h.svc.List(c.Request.Context())
	if err != nil {
		writeAddressError(c, err)
		return
	}
	out := make([]api.Address, 0, len(list))
	for _, a := range list {
		out = append(out, apiAddress(a))
	}
	c.JSON(http.StatusOK, out)
}

// Create 实现 POST /api/v1/addresses。
func (h *AddressHandler) Create(c *gin.Context) {
	var raw api.AddressInput
	if !bindJSON(c, &raw) {
		return
	}
	a, replayed, err := h.svc.Create(c.Request.Context(), addressInputOf(raw), idemKeyOf(c))
	if err != nil {
		writeAddressError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiAddress(a))
}

// Get 实现 GET /api/v1/addresses/{address_id}。
func (h *AddressHandler) Get(c *gin.Context) {
	id, ok := addressPathID(c)
	if !ok {
		return
	}
	a, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writeAddressError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAddress(a))
}

// Replace 实现 PUT /api/v1/addresses/{address_id}。
func (h *AddressHandler) Replace(c *gin.Context) {
	id, ok := addressPathID(c)
	if !ok {
		return
	}
	var raw api.AddressInput
	if !bindJSON(c, &raw) {
		return
	}
	a, err := h.svc.Replace(c.Request.Context(), id, addressInputOf(raw))
	if err != nil {
		writeAddressError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAddress(a))
}

// Delete 实现 DELETE /api/v1/addresses/{address_id}。
func (h *AddressHandler) Delete(c *gin.Context) {
	id, ok := addressPathID(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		writeAddressError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// SetDefault 实现 PUT /api/v1/addresses/{address_id}/default。
func (h *AddressHandler) SetDefault(c *gin.Context) {
	id, ok := addressPathID(c)
	if !ok {
		return
	}
	a, err := h.svc.SetDefault(c.Request.Context(), id)
	if err != nil {
		writeAddressError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAddress(a))
}

// addressPathID 取路径里的 address_id。
//
// 非正整数回 **404** 而不是 pathID 那个 422：这几条接口在契约里的错误集合是
// 404 + default，没有 422（PUT 的 422 说的是请求体）。`/addresses/abc` 对买家
// 就是「没有这条地址」，而它也确实不可能是任何一条地址。
func addressPathID(c *gin.Context) (int64, bool) {
	id, ok := parsePositiveID(c.Param("address_id"))
	if !ok {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "收货地址不存在")
		return 0, false
	}
	return id, true
}

// parsePositiveID 把一个路径段解析成正整数 id。
func parsePositiveID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func addressInputOf(raw api.AddressInput) service.AddressInput {
	in := service.AddressInput{
		ReceiverName: raw.ReceiverName,
		Phone:        raw.Phone,
		Province:     raw.Province,
		City:         raw.City,
		District:     raw.District,
		Street:       raw.Street,
		Detail:       raw.Detail,
		RegionCode:   raw.RegionCode,
		PostalCode:   raw.PostalCode,
		IsDefault:    raw.IsDefault,
	}
	if raw.Tag != nil {
		t := int(*raw.Tag)
		in.Tag = &t
	}
	return in
}

func apiAddress(a repository.SavedAddress) api.Address {
	street := a.Street
	tag := api.AddressTag(a.Tag)
	created, updated := a.CreatedAt, a.UpdatedAt
	return api.Address{
		Id:           a.ID,
		ReceiverName: a.ReceiverName,
		Phone:        a.Phone,
		Province:     a.Province,
		City:         a.City,
		District:     a.District,
		Street:       &street,
		Detail:       a.Detail,
		RegionCode:   a.RegionCode,
		PostalCode:   a.PostalCode,
		Tag:          &tag,
		IsDefault:    a.IsDefault,
		CreatedAt:    &created,
		UpdatedAt:    &updated,
	}
}

// writeFieldErrors 写 422 + Problem.errors（契约的 FieldError）。
// 地址簿与 PATCH /me 共用。
func writeFieldErrors(c *gin.Context, e *service.InvalidFieldsError) {
	items := make([]api.FieldError, 0, len(e.Fields))
	for _, f := range e.Fields {
		msg := f.Message
		items = append(items, api.FieldError{Field: &f.Field, Message: &msg})
	}
	detail := e.Error()
	problem.WriteValue(c, http.StatusUnprocessableEntity, api.Problem{
		Type:   problem.TypeInvalidRequest,
		Title:  "字段校验失败",
		Status: http.StatusUnprocessableEntity,
		Detail: &detail,
		Errors: &items,
	})
}

// writeBuyerAccountError 处理买家侧写接口共有的两种「人没了」：令牌还没过期，
// 账号已经注销（access_token 无状态，数据模型 §9 买家会话写着这个窗口）。
// 对客户端就是「请重新登录」，所以是 401 而不是 404。
func writeBuyerAccountError(c *gin.Context, err error) bool {
	if errors.Is(err, repository.ErrUserGone) || errors.Is(err, repository.ErrUserNotFound) {
		problem.Write(c, http.StatusUnauthorized, problem.TypeUnauthorized, "账号不存在或已注销，请重新登录")
		return true
	}
	return false
}

// writeAddressError 把地址簿的业务错误翻成契约里的响应。
func writeAddressError(c *gin.Context, err error) {
	var fields *service.InvalidFieldsError
	switch {
	case errors.As(err, &fields):
		writeFieldErrors(c, fields)
	case errors.Is(err, repository.ErrAddressNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "收货地址不存在")
	case errors.Is(err, service.ErrUseDefaultEndpoint):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeUseDefaultEndpoint,
			"切换默认地址请用 PUT /addresses/{address_id}/default")
	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")
	case writeBuyerAccountError(c, err):
	default:
		// 幂等的两种（409 处理中 / 422 钥匙复用）与兜底 500 都在那里，同一个出口。
		writeOrderError(c, err)
	}
}
