package handler_test

import (
	"strings"
	"testing"
)

func TestNotYetImplementedStagesAreNamedInContract(t *testing.T) {
	checked := 0
	for _, r := range routes {
		if len(r.NotYetImplementedStage) == 0 {
			continue
		}
		t.Run(r.HTTPMethod+" "+r.ContractPath, func(t *testing.T) {
			desc := contractOperationDescription(t, r)
			for name, why := range r.NotYetImplementedStage {
				if !strings.Contains(desc, name) {
					t.Errorf("NotYetImplementedStage 里挂着 %q（%s），"+
						"但契约里 %s %s 的描述已经不提它了 —— 清单烂了，"+
						"请对着新的描述改这一行或删掉它。当前描述：%q",
						name, why, r.ContractMethod, r.ContractPath, desc)
				}
				checked++
			}
			t.Logf("契约描述 %q；挂账 %v", desc, sorted(r.NotYetImplementedStage))
		})
	}
	if checked == 0 {
		t.Fatal("一笔流水线阶段挂账都没查到 —— 挂账清空了就该把这条测试一起删掉，" +
			"留着一条恒绿的测试比没有更糟")
	}
}

// contractOperationDescription 取出该接口的 description。
// 取不到或者是空串就 Fatal：一条对着空串做 strings.Contains 的测试恒绿。
func contractOperationDescription(t *testing.T, r route) string {
	t.Helper()

	doc := loadContract(t)
	op, ok := doc.Paths[r.ContractPath][r.ContractMethod].(map[string]any)
	if !ok {
		t.Fatalf("契约里没有 %s %s", r.ContractMethod, r.ContractPath)
	}
	desc, _ := op["description"].(string)
	if strings.TrimSpace(desc) == "" {
		t.Fatalf("契约里 %s %s 没有 description —— 这条测试没在检查任何东西",
			r.ContractMethod, r.ContractPath)
	}
	return desc
}
