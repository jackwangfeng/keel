//go:build keel_category_eval

package understanding

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/keel/keel/internal/inference"
)

// 类目推荐的离线评测，对**真的跑着的**推理引擎（infero）。
//
//	KEEL_EMBED_ENDPOINT=http://127.0.0.1:18081 make category-eval
//
// 它不在 test-db / CI 里：要一块 GPU 和一个另外起着的 infero 进程
// （与 make test-engine 同一个理由）。评测集与算分在 category_evalset_test.go，
// 那一半每次 test-db 都跑。
//
// 它打印三组数（综合店类目树、演示栈种子含 / 不含「默认分类」）× 两种查询写法
// （带 / 不带 Qwen3 的任务指令），每组给 Top-1、Top-3 与阈值表。
// DefaultCategoryGate 就是从「综合店类目树 × 带指令」那张组合判据表里取的：
// 在 precision 不低于 95% 的组合里取自动选中最多、且分差不小于 0.03 的那一格
// （宁可让商家多手选几件，也不替他选错；0.02 那一格样本余量太薄）。
//
// 断言两条底线：带指令时综合店那组 Top-3 ≥ 85%、当前判据下 precision ≥ 95%。守的是「换了模型或改了
// 拼法之后推荐没有塌掉」，不是一个要去刷高的分数。
func TestCategoryEval(t *testing.T) {
	endpoint := os.Getenv(inference.EnvEndpoint)
	if endpoint == "" {
		t.Fatalf("要设 %s（例如 http://127.0.0.1:18081）指向一个跑着的 infero", inference.EnvEndpoint)
	}
	c, err := inference.New(inference.Config{Endpoint: endpoint, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	plain := func(title, subtitle string) string {
		q := CategoryQueryText(title, subtitle)
		return q[len(categoryQueryInstruct):]
	}
	variants := []struct {
		name string
		fn   func(title, subtitle string) string
	}{
		{"带指令（线上用的这一种）", CategoryQueryText},
		{"不带指令", plain},
	}

	var main evalReport
	for _, set := range []evalSet{loadTreeSet(t), loadDemoSet(t, false), loadDemoSet(t, true)} {
		for vi, v := range variants {
			rec := NewCategoryRecommender(c)
			rep, err := runEval(ctx, rec, set, v.fn)
			if err != nil {
				t.Fatalf("%s / %s: %v", set.Name, v.name, err)
			}
			rep.Set = set.Name + " × " + v.name
			t.Log("\n" + rep.String())
			if vi == 0 && set.Name == loadTreeSet(t).Name {
				main = rep
			}
		}
	}
	g := DefaultCategoryGate
	cov, prec, auto, right := main.atGate(g.MinScore, g.MinMargin)
	t.Logf("当前判据（分数 ≥ %.2f 且分差 ≥ %.2f）：综合店那组自动选中 %d 条（%.1f%%），其中选对 %d 条（precision %.1f%%）",
		g.MinScore, g.MinMargin, auto, cov*100, right, prec*100)
	if prec < 0.95 {
		t.Errorf("当前判据下 precision 只有 %.1f%%，低于 95%%：阈值要按上面的表重定", prec*100)
	}
	if main.top3() < 0.85 {
		t.Errorf("综合店类目树 × 带指令的 Top-3 只有 %.1f%%，低于 85%% 的底线", main.top3()*100)
	}
}
