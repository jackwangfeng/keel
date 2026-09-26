package understanding

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/keel/keel/internal/inference"
)

// 类目推荐离线评测的**数据与算分**那一半。它不带编译标签：数据集的完整性
// （标注的类目在不在树里、每个类目有没有够数的样本）与算分逻辑本身，
// 在每一次 `make test-db` 里都跑 —— 一份悄悄坏掉的评测集比没有评测集更糟，
// 它会让「阈值是评测定出来的」这句话变成假的。
//
// 真引擎那一半在 category_eval_realengine_test.go（keel_category_eval 标签，
// `make category-eval` 手动跑）。

type evalCase struct {
	Title, Subtitle, Want string
}

type evalSet struct {
	Name    string
	Options []CategoryOption
	Cases   []evalCase
}

func evalDataPath(name string) string { return filepath.Join("testdata", "category_eval", name) }

func readEvalLines(t testing.TB, name string) []string {
	t.Helper()
	f, err := os.Open(evalDataPath(name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func parseEvalCase(t testing.TB, line string) evalCase {
	t.Helper()
	parts := strings.Split(line, "|")
	if len(parts) != 3 {
		t.Fatalf("评测集这一行不是三列：%q", line)
	}
	return evalCase{
		Title:    strings.TrimSpace(parts[0]),
		Subtitle: strings.TrimSpace(parts[1]),
		Want:     strings.TrimSpace(parts[2]),
	}
}

// loadTreeSet 是「综合店类目树 + 人工标注标题」那一组。
func loadTreeSet(t testing.TB) evalSet {
	s := evalSet{Name: "综合店类目树（41 个叶子）"}
	for i, p := range readEvalLines(t, "tree.txt") {
		s.Options = append(s.Options, CategoryOption{ID: int64(i + 1), PathName: p})
	}
	for _, line := range readEvalLines(t, "titles.txt") {
		s.Cases = append(s.Cases, parseEvalCase(t, line))
	}
	return s
}

// loadDemoSet 是演示栈种子那一组；dropDefault 为 true 时去掉「默认分类」
// （类目与它名下的商品一起去掉）。
func loadDemoSet(t testing.TB, dropDefault bool) evalSet {
	s := evalSet{Name: "演示栈种子（5 个类目，含「默认分类」）"}
	if dropDefault {
		s.Name = "演示栈种子（去掉「默认分类」后 4 个类目）"
	}
	id := int64(0)
	for _, line := range readEvalLines(t, "demo_seed.txt") {
		if strings.HasPrefix(line, "@") {
			name := strings.TrimPrefix(line, "@")
			if dropDefault && name == "默认分类" {
				continue
			}
			id++
			s.Options = append(s.Options, CategoryOption{ID: id, PathName: name})
			continue
		}
		c := parseEvalCase(t, line)
		if dropDefault && c.Want == "默认分类" {
			continue
		}
		s.Cases = append(s.Cases, c)
	}
	return s
}

// evalScore 是一条样本的结果。
type evalScore struct {
	Case     evalCase
	Top      []CategoryCandidate
	Hit1     bool
	Hit3     bool
	TopScore float64
	Margin   float64 // Top-1 与 Top-2 的分差
}

// evalReport 是一组的结果。
type evalReport struct {
	Set    string
	Scores []evalScore
}

func (r evalReport) top1() float64 { return r.rate(func(s evalScore) bool { return s.Hit1 }) }
func (r evalReport) top3() float64 { return r.rate(func(s evalScore) bool { return s.Hit3 }) }

func (r evalReport) rate(ok func(evalScore) bool) float64 {
	if len(r.Scores) == 0 {
		return 0
	}
	n := 0
	for _, s := range r.Scores {
		if ok(s) {
			n++
		}
	}
	return float64(n) / float64(len(r.Scores))
}

// atThreshold 是「Top-1 分数 ≥ th 才自动选」时的两个数：
// coverage = 自动选了的占比；precision = 自动选了的里面选对的占比。
// 阈值就是在这两个数之间取舍：precision 是「替商家选错类目」的反面，
// coverage 是「商家要手选的有多少」的反面。
func (r evalReport) atThreshold(th float64) (coverage, precision float64, auto, right int) {
	for _, s := range r.Scores {
		if s.TopScore >= th {
			auto++
			if s.Hit1 {
				right++
			}
		}
	}
	if len(r.Scores) > 0 {
		coverage = float64(auto) / float64(len(r.Scores))
	}
	if auto > 0 {
		precision = float64(right) / float64(auto)
	}
	return coverage, precision, auto, right
}

// atGate 是组合判据：Top-1 分数 ≥ minScore **且** 与 Top-2 的分差 ≥ minMargin 才自动选。
func (r evalReport) atGate(minScore, minMargin float64) (coverage, precision float64, auto, right int) {
	for _, s := range r.Scores {
		if s.TopScore >= minScore && s.Margin >= minMargin {
			auto++
			if s.Hit1 {
				right++
			}
		}
	}
	if len(r.Scores) > 0 {
		coverage = float64(auto) / float64(len(r.Scores))
	}
	if auto > 0 {
		precision = float64(right) / float64(auto)
	}
	return coverage, precision, auto, right
}

// runEval 跑一组。queryText 决定商品标题怎么拼成查询文本（评测里比较
// 「带指令」与「不带指令」两种）。
func runEval(ctx context.Context, rec *CategoryRecommender, set evalSet,
	queryText func(title, subtitle string) string) (evalReport, error) {

	queries := make([]string, len(set.Cases))
	for i, c := range set.Cases {
		queries[i] = queryText(c.Title, c.Subtitle)
	}
	res, err := rec.Recommend(ctx, queries, set.Options)
	if err != nil {
		return evalReport{}, err
	}
	rep := evalReport{Set: set.Name}
	for i, c := range set.Cases {
		s := evalScore{Case: c, Top: res[i]}
		if len(res[i]) > 0 {
			s.TopScore = res[i][0].Score
			s.Hit1 = res[i][0].PathName == c.Want
		}
		if len(res[i]) > 1 {
			s.Margin = res[i][0].Score - res[i][1].Score
		}
		for _, cand := range res[i] {
			if cand.PathName == c.Want {
				s.Hit3 = true
			}
		}
		rep.Scores = append(rep.Scores, s)
	}
	return rep, nil
}

// String 是给人读的报告：总数、Top-1 / Top-3，阈值表，以及 Top-1 推错的样本。
func (r evalReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "== %s：%d 条\n", r.Set, len(r.Scores))
	fmt.Fprintf(&b, "   Top-1 命中率 %.1f%%   Top-3 命中率 %.1f%%\n", r.top1()*100, r.top3()*100)
	fmt.Fprintf(&b, "   阈值    自动选中   其中选对   precision\n")
	for th := 0.30; th <= 0.801; th += 0.05 {
		cov, prec, auto, right := r.atThreshold(th)
		fmt.Fprintf(&b, "   %.2f   %3d (%5.1f%%)   %3d       %5.1f%%\n", th, auto, cov*100, right, prec*100)
	}
	fmt.Fprintf(&b, "   分差    自动选中   其中选对   precision（按 Top-1 − Top-2 的分差判）\n")
	for m := 0.00; m <= 0.101; m += 0.01 {
		auto, right := 0, 0
		for _, s := range r.Scores {
			if s.Margin >= m {
				auto++
				if s.Hit1 {
					right++
				}
			}
		}
		prec := 0.0
		if auto > 0 {
			prec = float64(right) / float64(auto)
		}
		fmt.Fprintf(&b, "   %.2f   %3d (%5.1f%%)   %3d       %5.1f%%\n", m, auto,
			float64(auto)/float64(max(len(r.Scores), 1))*100, right, prec*100)
	}
	fmt.Fprintf(&b, "   组合判据（分数 ≥ a 且分差 ≥ m）：自动选中占比 / precision\n")
	margins := []float64{0.02, 0.03, 0.04, 0.05}
	b.WriteString("   a / m ")
	for _, m := range margins {
		fmt.Fprintf(&b, "      %.2f       ", m)
	}
	b.WriteString("\n")
	for _, a := range []float64{0, 0.50, 0.55, 0.60, 0.65} {
		fmt.Fprintf(&b, "   %.2f  ", a)
		for _, m := range margins {
			cov, prec, _, _ := r.atGate(a, m)
			fmt.Fprintf(&b, "  %5.1f%% / %5.1f%%", cov*100, prec*100)
		}
		b.WriteString("\n")
	}
	var wrong []evalScore
	for _, s := range r.Scores {
		if !s.Hit1 {
			wrong = append(wrong, s)
		}
	}
	sort.Slice(wrong, func(i, j int) bool { return wrong[i].TopScore > wrong[j].TopScore })
	if len(wrong) > 0 {
		fmt.Fprintf(&b, "   Top-1 推错的 %d 条（按分数降序）：\n", len(wrong))
		for _, s := range wrong {
			got := ""
			if len(s.Top) > 0 {
				got = fmt.Sprintf("%s（%.3f）", s.Top[0].PathName, s.Top[0].Score)
			}
			mark := " "
			if s.Hit3 {
				mark = "3"
			}
			fmt.Fprintf(&b, "   %s %s %s → %s 分差 %.3f，应为 %s\n", mark, s.Case.Title, s.Case.Subtitle, got, s.Margin, s.Case.Want)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 不需要引擎的那几条
// ---------------------------------------------------------------------------

// TestCategoryEvalSetIsWellFormed 守评测集本身：每条标注的类目都在树里、
// 每个叶子至少 3 条样本、标题不重复。
func TestCategoryEvalSetIsWellFormed(t *testing.T) {
	for _, set := range []evalSet{loadTreeSet(t), loadDemoSet(t, false)} {
		paths := map[string]bool{}
		for _, o := range set.Options {
			if paths[o.PathName] {
				t.Errorf("%s：类目 %q 出现了两次", set.Name, o.PathName)
			}
			paths[o.PathName] = true
		}
		per := map[string]int{}
		seen := map[string]bool{}
		for _, c := range set.Cases {
			if !paths[c.Want] {
				t.Errorf("%s：%q 标注的类目 %q 不在类目表里", set.Name, c.Title, c.Want)
			}
			if seen[c.Title] {
				t.Errorf("%s：标题 %q 重复", set.Name, c.Title)
			}
			seen[c.Title] = true
			per[c.Want]++
		}
		if set.Name == loadTreeSet(t).Name {
			for p := range paths {
				if per[p] < 3 {
					t.Errorf("%s：类目 %q 只有 %d 条样本，至少 3 条", set.Name, p, per[p])
				}
			}
		}
	}
	if n := len(loadDemoSet(t, true).Cases); n != 16 {
		t.Errorf("去掉「默认分类」之后应当剩 16 件，得到 %d", n)
	}
}

// oracleEmbedder 知道每条文本的「正确类目」，把查询与类目都映射到那个类目的
// 基向量上 —— 它让 Top-1 必然 100%，于是算分逻辑的任何一处写错都会显形。
type oracleEmbedder struct {
	axis map[string]int // 文本 → 轴
}

func (e oracleEmbedder) Embed(_ context.Context, texts []string) (*inference.Result, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, inference.Dim)
		if a, ok := e.axis[t]; ok {
			v[a] = 1
		} else {
			v[inference.Dim-1] = 1
		}
		out[i] = v
	}
	return &inference.Result{Vectors: out, Model: inference.ModelName, ModelVersion: "oracle"}, nil
}

func TestCategoryEvalScoringWithOracle(t *testing.T) {
	set := loadTreeSet(t)
	axis := map[string]int{}
	for i, o := range set.Options {
		axis[CategoryDocText(o.PathName)] = i
	}
	for _, c := range set.Cases {
		for i, o := range set.Options {
			if o.PathName == c.Want {
				axis[CategoryQueryText(c.Title, c.Subtitle)] = i
			}
		}
	}
	rec := NewCategoryRecommender(oracleEmbedder{axis: axis})
	rep, err := runEval(context.Background(), rec, set, CategoryQueryText)
	if err != nil {
		t.Fatal(err)
	}
	if rep.top1() != 1 || rep.top3() != 1 || len(rep.Scores) != len(set.Cases) {
		t.Fatalf("神谕嵌入器下应当全中：%s", rep)
	}
	if cov, prec, _, _ := rep.atThreshold(0.99); cov != 1 || prec != 1 {
		t.Errorf("神谕嵌入器下 0.99 的阈值应当全部自动选中且全对：%v %v", cov, prec)
	}
	if !strings.Contains(rep.String(), "Top-1 命中率 100.0%") {
		t.Errorf("报告格式变了：%s", rep)
	}
}
