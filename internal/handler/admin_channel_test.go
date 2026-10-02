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
		if w := reqAs(t, http.MethodPut, sh.Host, path+"/sku-links/999999999", `{"external_id":"ghost"}`, sh.Token); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("映射不存在的 SKU：%d，期望 422", w.Code)
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

// 渠道管理页要的三处补口：binding 是否已配凭据、推送状态带 SKU 货号与商品名并可只看出错的。
func TestAdminChannelSecretsFlagAndListings(t *testing.T) {
	cs := newCouponShop(t)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	fx := &permFixture{sh: cs.adminShop}
	opTok := staffSession(t, cs.Host, fx.createStaff(t, cs.Token, 2, nil, nil)).Token
	base := v1 + "/admin/channel-bindings"

	var b api.ChannelBinding
	decodeInto(t, postIdem(t, cs.Host, base, fmt.Sprintf(`{"channel":%q,"external_account":"flag-%s","name":"假渠道","roles":4}`,
		channeltest.Kind, cs.Suffix), cs.Token), http.StatusCreated, "建 binding", &b)
	if b.HasSecrets {
		t.Fatal("刚建的 binding has_secrets = true，期望 false")
	}
	path := fmt.Sprintf("%s/%d", base, b.Id)
	if w := reqAs(t, http.MethodPut, cs.Host, path+"/secrets", `{"webhook_secret":"TOP-SECRET-FLAG"}`, cs.Token); w.Code != http.StatusNoContent {
		t.Fatalf("设凭据：%d %s", w.Code, w.Body.String())
	}
	var got api.ChannelBinding
	w := reqAs(t, http.MethodGet, cs.Host, path, "", opTok)
	decodeInto(t, w, http.StatusOK, "读 binding", &got)
	if !got.HasSecrets || strings.Contains(w.Body.String(), "TOP-SECRET") {
		t.Fatalf("配过凭据之后 GET：has_secrets=%v %s（期望 true、不含凭据）", got.HasSecrets, w.Body.String())
	}
	var list struct{ Items []api.ChannelBinding }
	w = reqAs(t, http.MethodGet, cs.Host, base, "", opTok)
	decodeInto(t, w, http.StatusOK, "binding 列表", &list)
	if len(list.Items) != 1 || !list.Items[0].HasSecrets || strings.Contains(w.Body.String(), "TOP-SECRET") {
		t.Fatalf("binding 列表 = %s，期望一条 has_secrets=true、不含凭据", w.Body.String())
	}

	adminExec(t, `INSERT INTO channel_listings (merchant_id, binding_id, store_id, sku_id, published_qty, published_cents, last_error)
		VALUES ($1, $2, $3, $4, 3, 6000, NULL), ($1, $2, $3, $5, 0, 5000, '平台说不行')`,
		cs.MerchantID, b.Id, cs.NorthStore, cs.DressSKU, cs.ShirtSKU)
	type row struct {
		SkuId        int64   `json:"sku_id"`
		SkuCode      string  `json:"sku_code"`
		ProductTitle string  `json:"product_title"`
		LastError    *string `json:"last_error"`
	}
	var ls struct{ Items []row }
	decodeInto(t, reqAs(t, http.MethodGet, cs.Host, path+"/listings", "", opTok), http.StatusOK, "推送状态", &ls)
	if len(ls.Items) != 2 {
		t.Fatalf("推送状态 %d 行，期望 2", len(ls.Items))
	}
	for _, it := range ls.Items {
		code := adminQueryString(t, `SELECT sku_code FROM skus WHERE id = $1`, it.SkuId)
		title := adminQueryString(t, `SELECT p.title FROM skus s JOIN products p ON p.id = s.product_id WHERE s.id = $1`, it.SkuId)
		if it.SkuCode != code || it.ProductTitle != title || code == "" || title == "" {
			t.Fatalf("推送状态一行 = %+v，期望货号 %q、商品名 %q", it, code, title)
		}
	}
	decodeInto(t, reqAs(t, http.MethodGet, cs.Host, path+"/listings?errors_only=true", "", opTok), http.StatusOK, "只看出错", &ls)
	if len(ls.Items) != 1 || ls.Items[0].SkuId != cs.ShirtSKU || ls.Items[0].LastError == nil {
		t.Fatalf("只看出错 = %+v，期望只有衬衫那一行", ls.Items)
	}
	decodeInto(t, reqAs(t, http.MethodGet, cs.Host, path+"/listings?errors_only=false", "", opTok), http.StatusOK, "errors_only=false", &ls)
	if len(ls.Items) != 2 {
		t.Fatalf("errors_only=false 回 %d 行，期望 2", len(ls.Items))
	}
	if w := reqAs(t, http.MethodGet, cs.Host, path+"/listings?errors_only=maybe", "", opTok); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("errors_only=maybe：%d，期望 422", w.Code)
	}

	t.Run("手动重拉_不是商品源_409_操作员_403_不存在_404", func(t *testing.T) {
		if w := postIdem(t, cs.Host, path+"/catalog-pulls", "", opTok); w.Code != http.StatusForbidden {
			t.Fatalf("操作员重拉：%d，期望 403", w.Code)
		}
		if w := postIdem(t, cs.Host, path+"/catalog-pulls", "", cs.Token); w.Code != http.StatusConflict {
			t.Fatalf("对只当销售渠道的 binding 重拉：%d %s，期望 409", w.Code, w.Body.String())
		}
		if w := postIdem(t, cs.Host, base+"/999999999/catalog-pulls", "", cs.Token); w.Code != http.StatusNotFound {
			t.Fatalf("对不存在的 binding 重拉：%d，期望 404", w.Code)
		}
	})
}

// 手动重拉商品：启用中的商品源 202 并入队，同一把钥匙重放；停用之后 409。
func TestAdminChannelCatalogPull(t *testing.T) {
	r := newShopifyRig(t, map[string]any{})
	r.activate(t)
	path := fmt.Sprintf("%s/admin/channel-bindings/%d/catalog-pulls", v1, r.b.ID)
	pending := func() int64 {
		return adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue = 'channel.catalog.pull' AND status IN (0, 1)`, r.cs.MerchantID)
	}
	if n := pending(); n != 0 {
		t.Fatalf("drain 之后还有 %d 个拉商品任务", n)
	}
	key := freshIdemKey()
	if w := postWithKey(t, r.cs.Host, path, "", r.cs.Token, key); w.Code != http.StatusAccepted {
		t.Fatalf("重拉：%d %s，期望 202", w.Code, w.Body.String())
	}
	if n := pending(); n != 1 {
		t.Fatalf("重拉之后有 %d 个待跑的拉商品任务，期望 1", n)
	}
	w := postWithKey(t, r.cs.Host, path, "", r.cs.Token, key)
	if w.Code != http.StatusAccepted || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("同一把钥匙再拉：%d replayed=%q，期望 202 重放", w.Code, w.Header().Get("Idempotency-Replayed"))
	}
	if w := postWithKey(t, r.cs.Host, path, "", r.cs.Token, ""); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("不带 Idempotency-Key：%d，期望 422", w.Code)
	}
	r.deactivate(t)
	if w := postIdem(t, r.cs.Host, path, "", r.cs.Token); w.Code != http.StatusConflict {
		t.Fatalf("停用之后重拉：%d %s，期望 409", w.Code, w.Body.String())
	}
}
