package handler_test

import (
	"fmt"
	"strings"
	"testing"
)

// 只读 SQL（00131）：全店 AI 员工能查自己店的视图；读不到底表（users 的手机号）、改不了数据、换不了租户；
// 另一家店的订单看不见；门店范围的 AI 员工用不了。
func TestMCPQuerySQLIsReadOnlyAndTenantBound(t *testing.T) {
	cs := newCouponShop(t)
	other := newCouponShop(t)
	ob := other.newBuyer(t, "sql-other")
	other.placePaid(t, ob, other.NorthStore, other.DressSKU, 1, nil)
	b := cs.newBuyer(t, "sql")
	cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 2, nil)

	a := createAgent(t, cs.adminShop, `{"name":"查数 AI","role":2}`)
	k := issueAgentKey(t, cs.adminShop, a.Id, `{"name":"test"}`)
	sess := mcpConnect(t, cs.Host, k.Secret)

	res, out := mcpCall(t, sess, "query_sql", map[string]any{"sql": "select count(*) as n, sum(quantity) as qty from order_items"})
	if res.IsError {
		t.Fatalf("查自己店的视图出错：%s", mcpText(res))
	}
	row := out["rows"].([]any)[0].([]any)
	if fmt.Sprint(row[0]) != "1" || fmt.Sprint(row[1]) != "2" {
		t.Fatalf("应只看到本店这一单两件（另一家店的看不见）：%v", row)
	}

	for _, q := range []string{
		"select phone from users",                                    // 底表：没权限
		"select phone from public.users",                             // 带 schema 前缀也不行
		"select set_config('app.merchant_id', '1', true)",            // 换租户：静态拒
		"select * from orders; delete from orders",                   // 多条语句
		"with d as (delete from orders returning id) select * from d", // 写
		"select pg_sleep(10)",                                        // 超时（3 秒）
	} {
		res, _ := mcpCall(t, sess, "query_sql", map[string]any{"sql": q})
		if !res.IsError {
			t.Errorf("%q 应被拒", q)
		}
	}
	res, _ = mcpCall(t, sess, "query_sql", map[string]any{"sql": "select receiver_snapshot from orders"})
	if !res.IsError || !strings.Contains(mcpText(res), "receiver_snapshot") {
		t.Errorf("视图里不该有收货地址快照：%s", mcpText(res))
	}

	sa := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"门店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	sk := issueAgentKey(t, cs.adminShop, sa.Id, `{"name":"test"}`)
	res, _ = mcpCall(t, mcpConnect(t, cs.Host, sk.Secret), "query_sql", map[string]any{"sql": "select 1"})
	if !res.IsError {
		t.Error("门店范围的 AI 员工不该能用 query_sql（视图是全店的）")
	}
}
