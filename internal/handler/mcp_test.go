package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP 服务（AI 经营 M9 任务 2）：用官方 SDK 的客户端经真实 HTTP 连上来 ——
// 列工具、调读工具、判权与人一致（门店管理员身份只看得到自己的店）、每次调用留审计、吊销即刻失效。

// hostTransport 把每个请求的 Host 与 Authorization 设好：租户按 Host 解析，密钥在请求头。
type hostTransport struct {
	host, key string
	base      http.RoundTripper
}

func (h hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = h.host
	r.Header.Set("Authorization", "Bearer "+h.key)
	return h.base.RoundTrip(r)
}

func mcpConnect(t *testing.T, host, key string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(testEngine)
	t.Cleanup(srv.Close)
	tr := &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + "/api/v1/mcp",
		HTTPClient:           &http.Client{Transport: hostTransport{host: host, key: key, base: http.DefaultTransport}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "keel-test", Version: "0"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("连 MCP 失败：%v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func mcpCall(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("调 %s 出错：%v", tool, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(raw, &out)
	}
	return res, out
}

func mcpText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestMCPReadToolsAuthorizeLikeHumans(t *testing.T) {
	cs := newCouponShop(t)
	// 门店管理员身份的 AI 员工：只管北京门店。
	a := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	k := issueAgentKey(t, cs.adminShop, a.Id, `{"name":"test"}`)
	sess := mcpConnect(t, cs.Host, k.Secret)

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"shop_overview", "sales_trend", "product_ranking", "store_comparison",
		"inventory_alerts", "search_insights", "list_stores", "list_products", "get_product", "list_refunds"} {
		if !names[want] {
			t.Errorf("工具清单里没有 %s", want)
		}
	}

	res, out := mcpCall(t, sess, "shop_overview", map[string]any{"period": "last_7_days"})
	if res.IsError || out["window"] == nil || out["current"] == nil {
		t.Fatalf("shop_overview：isError=%v %s out=%v", res.IsError, mcpText(res), out)
	}
	// 管辖范围：门店列表只有北京门店。
	_, stores := mcpCall(t, sess, "list_stores", nil)
	items, _ := stores["items"].([]any)
	if len(items) != 1 || int64(items[0].(map[string]any)["id"].(float64)) != cs.NorthStore {
		t.Fatalf("门店管理员身份的 AI 员工看到的门店：%v", items)
	}
	// 管辖范围：两家店都有低库存，AI 员工只看得到北京那条；点名要广州的，与人一样收窄成空（不泄露）。
	setStoreStock(t, cs.adminShop, cs.NorthStore, cs.DressSKU, 0)
	setStoreStock(t, cs.adminShop, cs.SouthStore, cs.DressSKU, 0)
	_, alerts := mcpCall(t, sess, "inventory_alerts", nil)
	rows, _ := alerts["items"].([]any)
	if len(rows) == 0 {
		t.Fatal("北京门店连衣裙卖空了，inventory_alerts 却是空的")
	}
	for _, r := range rows {
		if int64(r.(map[string]any)["store_id"].(float64)) != cs.NorthStore {
			t.Fatalf("门店管理员身份的 AI 员工看到了别的门店的库存预警：%v", r)
		}
	}
	res, alerts = mcpCall(t, sess, "inventory_alerts", map[string]any{"store_id": cs.SouthStore})
	if rows, _ := alerts["items"].([]any); res.IsError || len(rows) != 0 {
		t.Fatalf("点名要广州门店的预警应收窄成空：isError=%v %v", res.IsError, alerts)
	}
	// 参数错：period 不认识 → invalid-request，带说明。
	res, _ = mcpCall(t, sess, "shop_overview", map[string]any{"period": "last_year"})
	if !res.IsError || !strings.Contains(mcpText(res), "invalid-request") {
		t.Fatalf("错的 period 应返回 invalid-request：%q", mcpText(res))
	}

	// 审计：上面每一次调用都有一行，越权那次记着 out-of-scope。
	var total, denied int // 5 次调用：overview、list_stores、两次 inventory_alerts、错 period 的 overview
	if err := admin(t).QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE NOT ok AND error_type LIKE '%invalid-request')
		  FROM agent_tool_calls WHERE agent_staff_id = $1`, a.Id).Scan(&total, &denied); err != nil {
		t.Fatal(err)
	}
	if total != 5 || denied != 1 {
		t.Fatalf("审计行数 %d（参数错 %d），期望 5（1）", total, denied)
	}

	// 吊销：下一次调用就不行了。
	wantStatus(t, reqAs(t, http.MethodDelete, cs.Host, fmt.Sprintf("/api/v1/admin/agents/%d/keys/%d", a.Id, k.Id), "", cs.Token),
		http.StatusNoContent, "吊销")
	if _, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "shop_overview"}); err == nil {
		t.Fatal("吊销之后还能调工具")
	}
}
