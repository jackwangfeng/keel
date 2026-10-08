package handler_test

import (
	"strings"
	"testing"
)

func TestAdminContractOperationsAreRoutedOrListed(t *testing.T) {
	doc := loadContract(t)

	registered := map[string]bool{}
	for _, ri := range testEngine.Routes() {
		registered[ri.Method+" "+ri.Path] = true
	}
	if len(registered) == 0 {
		t.Fatal("gin 路由表是空的 —— 这条测试没在检查任何东西")
	}

	implemented := map[string]bool{}
	for _, r := range routes {
		implemented[strings.ToUpper(r.ContractMethod)+" "+r.ContractPath] = true
	}

	listed := map[string]bool{}
	for _, op := range notYetRouted {
		key := strings.ToUpper(op.ContractMethod) + " " + op.ContractPath
		if listed[key] {
			t.Errorf("notYetRouted 里 %s 挂了两次", key)
		}
		listed[key] = true
		if strings.TrimSpace(op.Why) == "" {
			t.Errorf("notYetRouted 里 %s 没写理由 —— 一笔不写理由的欠账，"+
				"下一个人只会把它当成一行豁免", key)
		}
	}

	// 方向一：契约 → 清单。漏登记就红。
	seen, done := 0, 0
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/admin/") {
			continue
		}
		for method := range item {
			if !contractHTTPMethods[method] {
				continue
			}
			seen++
			key := strings.ToUpper(method) + " " + path
			if implemented[key] {
				done++
				continue
			}
			if listed[key] {
				continue
			}
			t.Errorf("契约里有 %s，但它既没有在 routes 表里（没实现），"+
				"也没有在 notYetRouted 里挂账 —— 后台接口的缺口会这样静默地长出来，"+
				"请二选一", key)
		}
	}
	// 阳性对照：一条 /admin/ 操作都没解析出来，说明契约的形状变了或路径改了名，
	// 上面那个循环一次也不执行，整条测试恒绿。
	if seen == 0 {
		t.Fatal("从契约里一条 /admin/ 操作都没解析出来 —— 这条测试没在检查任何东西")
	}

	for _, op := range notYetRouted {
		key := strings.ToUpper(op.ContractMethod) + " " + op.ContractPath

		// 方向二：清单 → 契约。清单不能描述一个不存在的东西。
		item, ok := doc.Paths[op.ContractPath]
		if !ok {
			t.Errorf("notYetRouted 里挂着 %s（%s），但契约里已经没有这个路径了 —— "+
				"清单烂了，请删掉这一行", key, op.Why)
			continue
		}
		if _, ok := item[op.ContractMethod]; !ok {
			t.Errorf("notYetRouted 里挂着 %s（%s），但契约里那个路径没有这个方法了 —— "+
				"清单烂了，请删掉这一行", key, op.Why)
			continue
		}

		// 方向三：实现了就必须从这里划掉。
		if registered[strings.ToUpper(op.ContractMethod)+" "+ginPathOf(op.ContractPath)] {
			t.Errorf("%s 已经注册了路由，但它还挂在 notYetRouted 里（%s）—— "+
				"实现完请删掉这一行，并在 routes 表里加一行", key, op.Why)
		}
	}

	// 「已实现」是数出来的，不是 seen - len(listed) 减出来的：清单出问题时
	// 减法会打印出负数或虚高的「已实现」，而这行日志正是给排查的人看的。
	t.Logf("契约里 %d 条 /admin/ 操作：已实现 %d，挂账 %d", seen, done, len(listed))
}

// 注册的路由与 routes 表必须一一对应。
//
// 这条是表驱动的意义所在：没有它，第二条路由可以悄悄挂上去，而参数清账、
// 契约对齐这些检查一条也不会覆盖到它 —— 测试全绿，覆盖面却在缩水。
