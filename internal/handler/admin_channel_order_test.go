package handler_test

// 后台的渠道订单（handler/admin_channel_order*.go、service/admin_channel_order.go，第三期 Task 7）：
// 列表筛选与分页、详情含申请、接单 / 拒单 / 重试的 409 与 422、申请决定，以及后台订单标来源。
// 权限（读全店范围、动作按门店）在 permission_test.go 的矩阵里。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

type channelOrderPage struct {
	api.PageMeta
	Items []api.ChannelOrder `json:"items"`
}

func TestAdminChannelOrders(t *testing.T) {
	r := newFakeOrderRig(t, needsAccept, nil)
	// 后台走这个 rig 的渠道编排（假适配器带着 AcceptRequired 与上面放的单）。
	useEngine(t, app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithChannels(r.svc)))
	host, tok := r.cs.Host, r.cs.Token
	base := v1 + "/admin/channel-orders"

	co1 := r.putAwaiting(t, "a-1", 2, 5*time.Minute)
	co2 := r.putAwaiting(t, "a-2", 1, 5*time.Minute)
	co3 := r.putAwaiting(t, "a-3", 100, 5*time.Minute) // 北店没这么多货

	list := func(query string) channelOrderPage {
		t.Helper()
		var p channelOrderPage
		decodeInto(t, getAs(t, host, base+query, tok), http.StatusOK, "渠道单列表"+query, &p)
		return p
	}
	typeOf := func(body []byte) string {
		var p api.Problem
		_ = json.Unmarshal(body, &p)
		return p.Type
	}

	t.Run("列表_筛选与分页", func(t *testing.T) {
		p := list(fmt.Sprintf("?binding_id=%d", r.b.ID))
		if p.Total != 3 || len(p.Items) != 3 || p.Items[0].Id != co3 {
			t.Fatalf("按 binding 列：total=%d items=%d，期望 3 张、新的在前", p.Total, len(p.Items))
		}
		it := p.Items[2]
		if it.ExternalOrderName != "F-a-1" || it.Status != 2 || it.StoreId == nil || *it.StoreId != r.cs.NorthStore ||
			len(it.Lines) != 1 || it.Lines[0].Qty != 2 || it.Amounts.Goods != 12000 || it.OrderNo != nil || it.AcceptDeadline == nil {
			t.Fatalf("渠道单 a-1 = %+v", it)
		}
		if p := list(fmt.Sprintf("?store_id=%d&page_size=2", r.cs.NorthStore)); p.Total != 3 || len(p.Items) != 2 {
			t.Fatalf("按门店、每页 2：total=%d items=%d", p.Total, len(p.Items))
		}
		if p := list(fmt.Sprintf("?store_id=%d&page=2&page_size=2", r.cs.NorthStore)); len(p.Items) != 1 || p.Items[0].Id != co1 {
			t.Fatalf("第 2 页：%+v", p.Items)
		}
		if p := list("?status=3"); p.Total != 0 {
			t.Fatalf("还没接单就有 %d 张已接单", p.Total)
		}
		if w := getAs(t, host, base+"?binding_id=999999999", tok); w.Code != http.StatusNotFound {
			t.Fatalf("不存在的 binding：%d，期望 404", w.Code)
		}
		for _, q := range []string{"?status=9", "?store_id=x", "?exception_only=maybe"} {
			if w := getAs(t, host, base+q, tok); w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s：%d，期望 422", q, w.Code)
			}
		}
	})

	t.Run("接单_成单_再接409_重试409", func(t *testing.T) {
		var d api.ChannelOrderDetail
		decodeInto(t, reqAs(t, http.MethodPost, host, fmt.Sprintf("%s/%d/accept", base, co1), "", tok), http.StatusOK, "接单", &d)
		r.drain(t)
		decodeInto(t, getAs(t, host, fmt.Sprintf("%s/%d", base, co1), tok), http.StatusOK, "详情", &d)
		if d.Status != 3 || d.OrderNo == nil || d.Exception != nil || len(d.Requests) != 0 {
			t.Fatalf("接单之后 = status %d order_no %v exception %v", d.Status, d.OrderNo, d.Exception)
		}
		w := reqAs(t, http.MethodPost, host, fmt.Sprintf("%s/%d/accept", base, co1), "", tok)
		if w.Code != http.StatusConflict || typeOf(w.Body.Bytes()) != problem.TypeChannelOrderState {
			t.Fatalf("再接一次：%d %s，期望 409 channel-order-state", w.Code, w.Body.String())
		}
		if w := reqAs(t, http.MethodPost, host, fmt.Sprintf("%s/%d/retry", base, co1), "", tok); w.Code != http.StatusConflict {
			t.Fatalf("没有异常的单重试：%d %s，期望 409", w.Code, w.Body.String())
		}
		if w := getAs(t, host, base+"/999999999", tok); w.Code != http.StatusNotFound {
			t.Fatalf("不存在的渠道单：%d，期望 404", w.Code)
		}
	})

	t.Run("缺货接单_422带原因_只看异常", func(t *testing.T) {
		w := reqAs(t, http.MethodPost, host, fmt.Sprintf("%s/%d/accept", base, co3), "", tok)
		if w.Code != http.StatusUnprocessableEntity || typeOf(w.Body.Bytes()) != problem.TypeChannelOrderAcceptFailed {
			t.Fatalf("缺货接单：%d %s，期望 422 channel-order-accept-failed", w.Code, w.Body.String())
		}
		r.drain(t)
		p := list("?exception_only=true")
		if p.Total != 1 || p.Items[0].Id != co3 || p.Items[0].Exception == nil {
			t.Fatalf("只看异常：%+v", p.Items)
		}
	})

	t.Run("拒单", func(t *testing.T) {
		var d api.ChannelOrderDetail
		decodeInto(t, reqAs(t, http.MethodPost, host, fmt.Sprintf("%s/%d/reject", base, co2), `{"reason":"门店打烊"}`, tok),
			http.StatusOK, "拒单", &d)
		if d.Status != 7 {
			t.Fatalf("拒单之后状态 %d，期望 7", d.Status)
		}
		r.drain(t)
		if acts := r.fake.ActsOf(channel.ActReject); len(acts) == 0 || acts[len(acts)-1].Action.Reason != "门店打烊" {
			t.Fatalf("拒单动作 %+v", acts)
		}
		if w := reqAs(t, http.MethodPost, host, fmt.Sprintf("%s/%d/reject", base, co2), "", tok); w.Code != http.StatusConflict {
			t.Fatalf("拒过的再拒（不带请求体）：%d %s，期望 409", w.Code, w.Body.String())
		}
	})

	t.Run("申请决定", func(t *testing.T) {
		dl := time.Now().Add(10 * time.Minute)
		r.request(t, "a-1", channel.OrderRequest{ExternalRequestID: "rq-1", Kind: channel.RequestCancel, Reason: "顾客不想要了",
			AmountCents: 12000, Deadline: &dl})
		var d api.ChannelOrderDetail
		decodeInto(t, getAs(t, host, fmt.Sprintf("%s/%d", base, co1), tok), http.StatusOK, "详情", &d)
		if len(d.Requests) != 1 || d.Requests[0].Status != 1 || d.Requests[0].Kind != 1 || d.Requests[0].Reason != "顾客不想要了" {
			t.Fatalf("详情里的申请 %+v", d.Requests)
		}
		path := fmt.Sprintf("%s/admin/channel-order-requests/%d/decision", v1, d.Requests[0].Id)
		if w := reqAs(t, http.MethodPost, host, path, `{}`, tok); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("不带 agree：%d，期望 422", w.Code)
		}
		decodeInto(t, reqAs(t, http.MethodPost, host, path, `{"agree":true}`, tok), http.StatusOK, "同意申请", &d)
		if d.Requests[0].Status != 2 || d.Requests[0].DecidedBy == nil || *d.Requests[0].DecidedBy != r.cs.StaffID {
			t.Fatalf("同意之后的申请 %+v", d.Requests[0])
		}
		if d.Status != 3 {
			t.Fatalf("同意申请动了渠道单状态：%d", d.Status)
		}
		w := reqAs(t, http.MethodPost, host, path, `{"agree":false}`, tok)
		if w.Code != http.StatusConflict || typeOf(w.Body.Bytes()) != problem.TypeChannelOrderState {
			t.Fatalf("再决定一次：%d %s，期望 409", w.Code, w.Body.String())
		}
	})

	t.Run("后台订单标来源", func(t *testing.T) {
		no := adminQueryString(t, `SELECT order_no FROM channel_orders WHERE id = $1`, co1)
		var page struct {
			Items []api.AdminOrderSummary `json:"items"`
		}
		decodeInto(t, getAs(t, host, v1+"/admin/orders?order_no="+no, tok), http.StatusOK, "订单列表", &page)
		if len(page.Items) != 1 || page.Items[0].Source != 1 || page.Items[0].Channel == nil ||
			page.Items[0].Channel.ExternalOrderName != "F-a-1" || page.Items[0].Channel.BindingName != "假渠道" ||
			page.Items[0].Channel.Kind != r.b.Channel {
			t.Fatalf("订单列表里的渠道单 %+v", page.Items)
		}
		var d api.AdminOrderDetail
		decodeInto(t, getAs(t, host, v1+"/admin/orders/"+no, tok), http.StatusOK, "订单详情", &d)
		if d.Source != 1 || d.Channel == nil || d.Channel.ExternalOrderName != "F-a-1" {
			t.Fatalf("订单详情 source=%d channel=%+v", d.Source, d.Channel)
		}
	})
}
