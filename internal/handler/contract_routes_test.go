package handler_test

import (
	"testing"
)

func TestEveryRegisteredRouteIsAccountedFor(t *testing.T) {
	registered := map[string]bool{}
	for _, ri := range testEngine.Routes() {
		registered[ri.Method+" "+ri.Path] = true
	}
	if len(registered) == 0 {
		t.Fatal("gin 路由表是空的 —— 这条测试没在检查任何东西")
	}

	inTable := map[string]bool{}
	for _, r := range routes {
		inTable[r.key()] = true
	}

	for key := range registered {
		if inTable[key] {
			continue
		}
		if why, ok := nonContractRoutes[key]; ok {
			t.Logf("%s 刻意不在契约里：%s", key, why)
			continue
		}
		t.Errorf("路由 %q 注册了，但 contract_test.go 的 routes 表里没有它 —— "+
			"于是参数清账、契约对齐这些检查一条也覆盖不到它。"+
			"请在表里加一行，或者（如果它刻意不属于契约）加进 nonContractRoutes 并写明理由", key)
	}

	for key := range inTable {
		if !registered[key] {
			t.Errorf("routes 表里登记了 %q，但它没有被注册 —— "+
				"路径改了、或者这条接口被删了，表烂了", key)
		}
	}

	for key := range nonContractRoutes {
		if !registered[key] {
			t.Errorf("nonContractRoutes 里挂着 %q，但它没有被注册 —— 请删掉这一行", key)
		}
	}
}

// 契约声明的 query 参数，要么被 handler 读了，要么在 NotYetImplemented 里挂着账；
// 反过来，handler 读的每一个 query 参数也必须是契约声明过的。
//
// 这条测试读的是契约本身（YAML）与 handler 的源码（AST），两边都不是人手写的
// 清单 —— 手写清单会和它描述的东西各走各的，而那种漂移不会有任何症状。
