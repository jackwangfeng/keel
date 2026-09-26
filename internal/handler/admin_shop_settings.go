package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 店铺设置（契约 /admin/shop-settings，00059）：
//
//	GET /admin/shop-settings   读
//	PUT /admin/shop-settings   整体替换（天然幂等，不收 Idempotency-Key）
//
// 判权（只有管理员）在 service/shop_settings.go，这里一个 if 都不写。

// AdminShopSettingsHandler 是店铺设置的两条接口。
type AdminShopSettingsHandler struct{ svc *service.ShopSettingsService }

// NewAdminShopSettingsHandler 建店铺设置 handler。
func NewAdminShopSettingsHandler(s *service.ShopSettingsService) *AdminShopSettingsHandler {
	return &AdminShopSettingsHandler{svc: s}
}

// Get 实现 GET /api/v1/admin/shop-settings。
func (h *AdminShopSettingsHandler) Get(c *gin.Context) {
	out, err := h.svc.Get(c.Request.Context())
	if err != nil {
		writeShopSettingsError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiShopSettings(out))
}

// Replace 实现 PUT /api/v1/admin/shop-settings。
func (h *AdminShopSettingsHandler) Replace(c *gin.Context) {
	var raw api.ShopSettingsInput
	if !bindJSON(c, &raw) {
		return
	}
	out, err := h.svc.Replace(c.Request.Context(), service.ShopSettingsInput{
		ServicePhone:    raw.ServicePhone,
		Timezone:        raw.Timezone,
		AutoConfirmDays: raw.AutoConfirmDays,
		ReturnShipDays:  raw.ReturnShipDays,
	})
	if err != nil {
		writeShopSettingsError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiShopSettings(out))
}

func apiShopSettings(s service.ShopSettings) api.ShopSettings {
	return api.ShopSettings{
		ShopName:        s.ShopName,
		ServicePhone:    s.ServicePhone,
		Timezone:        s.Timezone,
		AutoConfirmDays: s.AutoConfirmDays,
		ReturnShipDays:  s.ReturnShipDays,
		UpdatedAt:       s.UpdatedAt,
	}
}

func writeShopSettingsError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	var fields *service.InvalidFieldsError
	if errors.As(err, &fields) {
		writeFieldErrors(c, fields)
		return
	}
	_ = c.Error(err)
	problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
}
