// Command keel-searcheval 做检索的离线评测集（语义检索层 §9.1），第一件用途是校准相关度下限
// （service.DefaultVectorFloor，现在拍的 0.40）。
//
// # 三个子命令
//
//	KEEL_EMBED_ENDPOINT=http://127.0.0.1:18081 go run ./cmd/keel-searcheval sample \
//	    -out pairs.jsonl [-merchant 3] [-since 2160h] [-queries 300] [-k 30] [-extra extra.txt]
//	KEEL_SYSTEMONE_ENDPOINT=http://127.0.0.1:18095 [KEEL_SYSTEMONE_TOKEN=...] go run ./cmd/keel-searcheval label \
//	    -pairs pairs.jsonl -out labels.jsonl [-model kev-latest] [-per-request 32] [-interval 2.1s]
//	go run ./cmd/keel-searcheval calibrate -pairs pairs.jsonl -labels labels.jsonl [-json]
//
// sample 连库（应用角色，RLS 照常生效）与推理引擎：从每家店的 search_logs 取搜得最多的查询
// （-extra 再补一批，一行一条，适合放「店里没有的东西」当反例），每条查询配上向量近邻前 k 件、
// 关键词 AND / OR 命中前 k 件，写成候选对（internal/searcheval.Pair）。
//
// label 用 System One 类判别模型（Kev / Jev，`/v1/systemone`）给候选对打标：一条查询一次请求，
// 每件候选一道是非题，「是」的概率记成 relevance（题面见 internal/searcheval/judge.go）。
// 人工标注也行：按 (merchant_id, query, product_id) 给每对写一行 internal/searcheval.Label，
// judge 写 "human"。两份标签可以拼在一个文件里，同一对多条标签 calibrate 取平均。
//
// calibrate 只读这两份文件，扫一遍下限，打印精确率 / 召回率 / F1 与「猜你想要」判对判错的比例，
// 给出 F1 最高的下限和「精确率 ≥ -precision 的最低下限」两个候选。**它不改任何配置**：
// 下限跟着模型走，改不改、改成多少，看完报告由人决定。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/searcheval"
	"github.com/keel/keel/internal/tenant"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "sample":
		err = sample(os.Args[2:])
	case "label":
		err = label(os.Args[2:])
	case "calibrate":
		err = calibrate(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, os.Args[1], "失败:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法：keel-searcheval sample|label|calibrate [参数]（-h 看各自的参数）")
	os.Exit(2)
}

func sample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	out := fs.String("out", "", "候选对写到这个 JSONL 文件（必填）")
	merchant := fs.Int64("merchant", 0, "只取这家商户；0 = 全部活跃商户")
	since := fs.Duration("since", 90*24*time.Hour, "只取这段时间内搜过的查询")
	nQueries := fs.Int("queries", 300, "每家店取搜得最多的多少条查询（§9.1：200–500）")
	k := fs.Int("k", 30, "每条查询取向量近邻、关键词 AND、关键词 OR 各前多少件")
	extra := fs.String("extra", "", "额外的查询，一行一条，每家店都跑（适合放店里没有的东西当反例）")
	_ = fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("要给 -out")
	}

	var extras []string
	if *extra != "" {
		b, err := os.ReadFile(*extra)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				extras = append(extras, l)
			}
		}
	}

	ctx := context.Background()
	// 与服务进程同一个池构造函数（带「不能绕过 RLS」的自检），理由见 cmd/keel-index。
	pool, err := db.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("建连接池失败: %w", err)
	}
	defer pool.Close()
	emb, err := inference.FromEnv()
	if err != nil {
		return err
	}
	repo := repository.New(pool)

	merchants := []int64{*merchant}
	if *merchant == 0 {
		if merchants, err = repo.ActiveMerchants(ctx); err != nil {
			return err
		}
	}

	var pairs []searcheval.Pair
	for _, m := range merchants {
		ps, err := sampleMerchant(tenant.NewContext(ctx, m), repo, emb, m, time.Now().Add(-*since),
			int32(*nQueries), int32(*k), extras)
		if err != nil {
			return fmt.Errorf("商户 %d: %w", m, err)
		}
		fmt.Fprintf(os.Stderr, "商户 %d：%d 个候选对\n", m, len(ps))
		pairs = append(pairs, ps...)
	}

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	if err := searcheval.WriteJSONL(f, pairs); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func sampleMerchant(ctx context.Context, repo *repository.Repo, emb *inference.Client, merchant int64,
	since time.Time, nQueries, k int32, extras []string) ([]searcheval.Pair, error) {

	var qs []repository.SearchEvalQuery
	if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		qs, err = tx.SearchEvalQueries(ctx, since, nQueries)
		return err
	}); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, q := range qs {
		seen[q.Query] = true
	}
	for _, e := range extras {
		if !seen[e] {
			qs = append(qs, repository.SearchEvalQuery{Query: e})
			seen[e] = true
		}
	}

	var out []searcheval.Pair
	const batch = 32
	for i := 0; i < len(qs); i += batch {
		chunk := qs[i:min(i+batch, len(qs))]
		texts := make([]string, len(chunk))
		for j, q := range chunk {
			texts[j] = q.Query
		}
		// 算向量在事务外：等引擎的那段时间不占连接（service/search.go 文件头第三节）。
		res, err := emb.Embed(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("给查询算向量: %w", err)
		}
		model := res.Model + "@" + res.ModelVersion
		for j, q := range chunk {
			ps, err := pairsForQuery(ctx, repo, merchant, q, res.Vectors[j], model, k)
			if err != nil {
				return nil, fmt.Errorf("查询 %q: %w", q.Query, err)
			}
			out = append(out, ps...)
		}
	}
	return out, nil
}

func pairsForQuery(ctx context.Context, repo *repository.Repo, merchant int64, q repository.SearchEvalQuery,
	vec []float32, model string, k int32) ([]searcheval.Pair, error) {

	byID := map[int64]*searcheval.Pair{}
	var order []int64
	add := func(c repository.SearchEvalCandidate) *searcheval.Pair {
		if p, ok := byID[c.ID]; ok {
			return p
		}
		p := &searcheval.Pair{MerchantID: merchant, Query: q.Query, QueryHits: q.Hits, ProductID: c.ID,
			Title: c.Title, Subtitle: c.Subtitle, Category: c.CategoryName, EmbedModel: model}
		byID[c.ID] = p
		order = append(order, c.ID)
		return p
	}

	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		nb, err := tx.SearchEvalVectorNeighbors(ctx, vec, k)
		if err != nil {
			return err
		}
		for i, c := range nb {
			p := add(c)
			s := 1 - c.Distance
			p.Similarity, p.VectorRank = &s, i+1
		}
		// 查询切不出任何词（纯符号）时两条 tsquery 都是空串，跳过关键词：to_tsquery('') 是语法错。
		if and, _ := search.TSQueryAnd(q.Query); and != "" {
			hits, err := tx.SearchEvalKeywordHits(ctx, and, k)
			if err != nil {
				return err
			}
			for _, c := range hits {
				p := add(c)
				p.KeywordAnd, p.KeywordOr = true, true
			}
		}
		if or := search.TSQueryOr(q.Query); or != "" {
			hits, err := tx.SearchEvalKeywordHits(ctx, or, k)
			if err != nil {
				return err
			}
			for _, c := range hits {
				add(c).KeywordOr = true
			}
		}
		// 只被关键词捞到的也补上相似度，校准时才看得到它们落在哪。
		var missing []int64
		for _, id := range order {
			if byID[id].Similarity == nil {
				missing = append(missing, id)
			}
		}
		d, err := tx.SearchEvalDistances(ctx, vec, missing)
		if err != nil {
			return err
		}
		for id, dist := range d {
			s := 1 - dist
			byID[id].Similarity = &s
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]searcheval.Pair, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

func label(args []string) error {
	fs := flag.NewFlagSet("label", flag.ExitOnError)
	pairsPath := fs.String("pairs", "", "sample 写出的候选对（必填）")
	out := fs.String("out", "", "标签写到这个 JSONL 文件（必填）")
	endpoint := fs.String("endpoint", os.Getenv(inference.EnvSystemOneEndpoint), "判别模型引擎地址（默认取 "+inference.EnvSystemOneEndpoint+"）")
	model := fs.String("model", "", "请求里的 model 字段；空着用引擎的默认")
	perRequest := fs.Int("per-request", 32, "一次请求最多几道题（同一条查询的候选）")
	interval := fs.Duration("interval", 0, "两次请求之间至少隔多久（共享实例限流 30 次/分钟时给 2.1s）")
	_ = fs.Parse(args)
	if *pairsPath == "" || *out == "" {
		return fmt.Errorf("要给 -pairs 与 -out")
	}
	pairs, err := readFile[searcheval.Pair](*pairsPath)
	if err != nil {
		return err
	}
	// 令牌只从环境变量读（inference.EnvSystemOneToken 上写了为什么）。
	c, err := inference.NewSystemOne(*endpoint, os.Getenv(inference.EnvSystemOneToken), 0, nil)
	if err != nil {
		return err
	}
	ctx := context.Background()
	groups := searcheval.GroupByQuery(pairs)
	var labels []searcheval.Label
	started := time.Now()
	dec := throttled{c, *interval, &time.Time{}}
	for i, g := range groups {
		ls, err := searcheval.LabelQuery(ctx, dec, *model, g, *perRequest)
		if err != nil {
			return fmt.Errorf("商户 %d 查询 %q: %w", g[0].MerchantID, g[0].Query, err)
		}
		labels = append(labels, ls...)
		if (i+1)%20 == 0 || i+1 == len(groups) {
			fmt.Fprintf(os.Stderr, "已打标 %d/%d 条查询、%d 对（%s）\n", i+1, len(groups), len(labels),
				time.Since(started).Round(time.Second))
		}
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	if err := searcheval.WriteJSONL(f, labels); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// throttled 让两次请求之间至少隔 interval（一条查询的候选多于 per-request 时一次 LabelQuery 会发多次）。
type throttled struct {
	d        searcheval.Decider
	interval time.Duration
	last     *time.Time
}

func (t throttled) Decide(ctx context.Context, r inference.SystemOneRequest) (*inference.SystemOneResponse, error) {
	if wait := t.interval - time.Since(*t.last); !t.last.IsZero() && wait > 0 {
		time.Sleep(wait)
	}
	// 共享实例的限流不只算我们一家：被 429 了就退避重试（10s、20s、40s……封顶 2 分钟），
	// 不让一次限流把已经打完的几十条查询白扔。
	backoff := 10 * time.Second
	for attempt := 1; ; attempt++ {
		*t.last = time.Now()
		resp, err := t.d.Decide(ctx, r)
		if !errors.Is(err, inference.ErrRateLimited) || attempt == 8 {
			return resp, err
		}
		fmt.Fprintf(os.Stderr, "被限流，%s 后重试（第 %d 次）\n", backoff, attempt)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff = min(2*backoff, 2*time.Minute)
	}
}

func calibrate(args []string) error {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	pairsPath := fs.String("pairs", "", "sample 写出的候选对（必填）")
	labelsPath := fs.String("labels", "", "标签文件（必填）")
	relevantAt := fs.Float64("relevant-at", 0.5, "relevance ≥ 它算相关")
	precision := fs.Float64("precision", 0.9, "推荐「精确率不低于它的最低下限」")
	asJSON := fs.Bool("json", false, "输出 JSON 而不是表格")
	_ = fs.Parse(args)
	if *pairsPath == "" || *labelsPath == "" {
		return fmt.Errorf("要给 -pairs 与 -labels")
	}
	pairs, err := readFile[searcheval.Pair](*pairsPath)
	if err != nil {
		return err
	}
	labels, err := readFile[searcheval.Label](*labelsPath)
	if err != nil {
		return err
	}
	rep := searcheval.Calibrate(pairs, labels, searcheval.Options{RelevantAt: *relevantAt, TargetPrecision: *precision})
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReport(os.Stdout, rep, *precision)
	return nil
}

func readFile[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	v, err := searcheval.ReadJSONL[T](f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

func printReport(w io.Writer, rep searcheval.Report, precision float64) {
	fmt.Fprintf(w, "候选对 %d（未打标 %d），查询 %d（全部不相关的 %d），只被向量路捞到的 %d（其中相关 %d），标签来源 %v\n\n",
		rep.Pairs, rep.Unlabeled, rep.Queries, rep.NothingRelevant, rep.VectorOnly, rep.Relevant, rep.Judges)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "下限\tTP\tFP\tFN\t精确率\t召回率\tF1\t猜你想要·判对\t猜你想要·误判\t")
	for i, r := range rep.Rows {
		// 0.05 一档，外加两个推荐点，表不至于 70 行。
		if i%5 != 0 && &rep.Rows[i] != rep.BestF1 && &rep.Rows[i] != rep.AtPrecision {
			continue
		}
		fmt.Fprintf(tw, "%.2f\t%d\t%d\t%d\t%.3f\t%.3f\t%.3f\t%.1f%%\t%.1f%%\t\n", r.Floor, r.TP, r.FP, r.FN,
			r.Precision, r.Recall, r.F1, 100*r.FallbackRight, 100*r.FallbackWrong)
	}
	tw.Flush()
	fmt.Fprintln(w)
	if rep.BestF1 != nil {
		fmt.Fprintf(w, "F1 最高：%.2f（F1 %.3f）\n", rep.BestF1.Floor, rep.BestF1.F1)
	} else {
		fmt.Fprintln(w, "没有一个下限的 F1 大于 0（只被向量路捞到的候选里没有相关的，或标签对不上）")
	}
	if rep.AtPrecision != nil {
		fmt.Fprintf(w, "精确率 ≥ %.2f 的最低下限：%.2f（召回率 %.3f）\n", precision, rep.AtPrecision.Floor, rep.AtPrecision.Recall)
	} else {
		fmt.Fprintf(w, "扫描区间内没有一个下限的精确率达到 %.2f\n", precision)
	}
}
