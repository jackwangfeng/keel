package handler_test

// 后台的渠道管理（handler/admin_channel.go、service/admin_channel.go）。

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/channel/channeltest"
)

func TestAdminChannelBindings(t *testing.T) {
	sh := newAdminShop(t)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, sh.MerchantID) })
	fx := &permFixture{sh: sh}
	opTok := staffSession(t, sh.Host, fx.createStaff(t, sh.Token, 2, nil, nil)).Token
	base := v1 + "/admin/channel-bindings"

	var b api.ChannelBinding
	key := freshIdemKey()
	body := fmt.Sprintf(`{"channel":%q,"external_account":"shop-%s","name":"测试渠道","roles":4}`, channeltest.Kind, sh.Suffix)

	t.Run("操作员不能建", func(t *testing.T) {
		w := postIdem(t, sh.Host, base, body, opTok)
		if w.Code != http.StatusForbidden {
			t.Fatalf("操作员建 binding：%d %s，期望 403", w.Code, w.Body.String())
		}
	})

	t.Run("管理员建_默认停用_幂等重放", func(t *testing.T) {
		decodeInto(t, postWithKey(t, sh.Host, base, body, sh.Token, key), http.StatusCreated, "建 binding", &b)
		if b.Status != 2 || b.WebhookPath != fmt.Sprintf("/api/v1/webhooks/channels/%d", b.Id) {
			t.Fatalf("新建的 binding = %+v，期望停用、回调路径带 id", b)
		}
		w := postWithKey(t, sh.Host, base, body, sh.Token, key)
		if w.Code != http.StatusCreated || w.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatalf("同一把钥匙再建：%d replayed=%q，期望 201 重放", w.Code, w.Header().Get("Idempotency-Replayed"))
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM channel_bindings WHERE merchant_id = $1`, sh.MerchantID); n != 1 {
			t.Fatalf("重放之后有 %d 个 binding，期望 1", n)
		}
	})

	t.Run("没有这个渠道_422", func(t *testing.T) {
		w := postIdem(t, sh.Host, base, `{"channel":"nope","external_account":"x","name":"x","roles":4}`, sh.Token)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("不存在的渠道：%d，期望 422", w.Code)
		}
	})

	path := fmt.Sprintf("%s/%d", base, b.Id)
	t.Run("凭据只写不读_改配置不丢凭据", func(t *testing.T) {
		if w := reqAs(t, http.MethodPut, sh.Host, path+"/secrets", `{"webhook_secret":"TOP-SECRET-1"}`, opTok); w.Code != http.StatusForbidden {
			t.Fatalf("操作员设凭据：%d，期望 403", w.Code)
		}
		if w := reqAs(t, http.MethodPut, sh.Host, path+"/secrets", `{"webhook_secret":"TOP-SECRET-1"}`, sh.Token); w.Code != http.StatusNoContent {
			t.Fatalf("设凭据：%d %s", w.Code, w.Body.String())
		}
		w := reqAs(t, http.MethodPatch, sh.Host, path, `{"config":{"auto_accept":true},"status":1}`, sh.Token)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "TOP-SECRET") {
			t.Fatalf("PATCH：%d %s（不得含凭据）", w.Code, w.Body.String())
		}
		for _, p := range []string{path, base} {
			if w := reqAs(t, http.MethodGet, sh.Host, p, "", opTok); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "TOP-SECRET") {
				t.Fatalf("GET %s：%d %s（操作员可读、不得含凭据）", p, w.Code, w.Body.String())
			}
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM channel_bindings WHERE id = $1 AND secrets->>'webhook_secret' = 'TOP-SECRET-1'`, b.Id); n != 1 {
			t.Fatal("只改 config 之后凭据丢了")
		}
	})

	t.Run("门店映射与规则", func(t *testing.T) {
		w := reqAs(t, http.MethodPut, sh.Host, fmt.Sprintf("%s/store-links/%d", path, sh.StoreID), `{"external_store_id":"loc-1"}`, sh.Token)
		if w.Code != http.StatusOK {
			t.Fatalf("映射门店：%d %s", w.Code, w.Body.String())
		}
		w = reqAs(t, http.MethodPut, sh.Host, path+"/stock-rules", `{"sku_id":1,"ratio_bp":5000}`, sh.Token)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("只给 SKU 不给门店：%d，期望 422", w.Code)
		}
		var r api.ChannelStockRule
		decodeInto(t, reqAs(t, http.MethodPut, sh.Host, path+"/stock-rules", `{"ratio_bp":8000,"safety_qty":2}`, sh.Token), http.StatusOK, "设渠道级规则", &r)
		decodeInto(t, reqAs(t, http.MethodPut, sh.Host, path+"/stock-rules", `{"ratio_bp":7000}`, sh.Token), http.StatusOK, "覆盖渠道级规则", &r)
		var list struct{ Items []api.ChannelStockRule }
		decodeInto(t, reqAs(t, http.MethodGet, sh.Host, path+"/stock-rules", "", opTok), http.StatusOK, "读规则", &list)
		if len(list.Items) != 1 || list.Items[0].RatioBp != 7000 {
			t.Fatalf("规则 = %+v，期望一条 7000（覆盖而不是新增）", list.Items)
		}
		if w := reqAs(t, http.MethodPut, sh.Host, path+"/price-rules", `{"fixed_cents":100}`, sh.Token); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("渠道级固定价：%d，期望 422", w.Code)
		}
		if w := reqAs(t, http.MethodDelete, sh.Host, fmt.Sprintf("%s/stock-rules/%d", path, r.Id), "", sh.Token); w.Code != http.StatusNoContent {
			t.Fatalf("删规则：%d", w.Code)
		}
	})

	t.Run("别家店读不到", func(t *testing.T) {
		other := newAdminShop(t)
		if w := reqAs(t, http.MethodGet, other.Host, path, "", other.Token); w.Code != http.StatusNotFound {
			t.Fatalf("别家店读：%d，期望 404", w.Code)
		}
		if w := reqAs(t, http.MethodPut, other.Host, path+"/secrets", `{"x":"y"}`, other.Token); w.Code != http.StatusNotFound {
			t.Fatalf("别家店设凭据：%d，期望 404", w.Code)
		}
	})
}
