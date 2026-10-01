package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/searcheval"
	"github.com/keel/keel/internal/tenant"
)

// SearchJudgeService 在后台给高频查询的向量路候选做相关度预判（00230），检索命中就按预判留或去
// （search.go 的 vectorTrusted），替代分不开相关度的余弦下限。
//
// # 为什么在后台而不是在检索请求里判
//
// 判别模型（Kev-4B，`/v1/systemone`）判得准但慢：实测约 25 ms + 14 ms/题，一条查询的向量独有候选常有
// 20–40 件，光判别就 0.3–0.6 s，一张卡每秒只扛几条搜索（语义检索层 §9.1「在线延迟」）。而搜索词高度集中
// （演示站头部一条查询就占 48 次），所以热词判一次、存起来、用很多次。没判过的查询照旧按余弦下限，
// 预判只会让结果更准，不会让任何一条检索变慢或失败。
//
// # 一轮做什么
//
//  1. 探测一次判别模型，拿到「名字@版本」（ProductsNeedingJudgment 要用它判断哪些是别的模型判的）。
//  2. 每家活跃店：取 Window 内搜过 ≥ MinHits 次的查询（按 search.NormQuery 合并），次数多的在前，至多 Queries 条。
//  3. 每条查询：算向量、取近邻前 Candidates 件，去掉关键词 AND 命中的（那些在检索里一律可信，不用判），
//     再去掉已经判过且还新鲜的（同一个模型、商品之后没改过、不早于 FreshFor），剩下的交给判别模型，写回表里。
//
// 选主（Leader）：判别模型是共享的稀缺资源，多实例各判一遍是纯浪费。
type SearchJudgeService struct {
	repo SearchJudgeRepository
	emb  inference.Embedder
	dec  searcheval.Decider
	cfg  SearchJudgeConfig
	log  *slog.Logger
	now  func() time.Time
}

// SearchJudgeRepository 是这个任务要的仓储能力。
type SearchJudgeRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
}

// SearchJudgeConfig 是可调项，零值取默认。
type SearchJudgeConfig struct {
	Interval   time.Duration // 一轮的间隔，默认 1 小时
	Window     time.Duration // 热词统计窗口，默认 7 天
	MinHits    int64         // 窗口内至少搜过几次才判，默认 3
	Queries    int32         // 每家店每轮至多判多少条查询，默认 200
	Candidates int32         // 每条查询判向量近邻前多少件，默认 60（size 20 × RecallMultiplier）
	FreshFor   time.Duration // 判过多久之后重判，默认 14 天（保留期默认 30 天，热词在过期前一定会被刷新）
	PerRequest int           // 一次请求至多几道题，默认 32
}

// SearchJudgeReport 是一轮的结果。
type SearchJudgeReport struct {
	Judge    string
	Queries  int // 看过的查询
	Judged   int // 这轮新判（或重判）的 (查询, 商品) 对
	Skipped  int // 已经判过、还新鲜的
	Failures int // 失败的查询（引擎错误等），不中断这一轮
}

func (c SearchJudgeConfig) withDefaults() SearchJudgeConfig {
	if c.Interval <= 0 {
		c.Interval = time.Hour
	}
	if c.Window <= 0 {
		c.Window = 7 * 24 * time.Hour
	}
	if c.MinHits <= 0 {
		c.MinHits = 3
	}
	if c.Queries <= 0 {
		c.Queries = 200
	}
	if c.Candidates <= 0 {
		c.Candidates = int32(DefaultSearchSize * RecallMultiplier)
	}
	if c.FreshFor <= 0 {
		c.FreshFor = 14 * 24 * time.Hour
	}
	if c.PerRequest <= 0 {
		c.PerRequest = 32
	}
	return c
}

// NewSearchJudgeService 建任务。emb 与 dec 都不许是 nil：没有引擎就别注册这个任务（app 里判）。
func NewSearchJudgeService(repo SearchJudgeRepository, emb inference.Embedder, dec searcheval.Decider,
	cfg SearchJudgeConfig, log *slog.Logger) (*SearchJudgeService, error) {
	if emb == nil || dec == nil {
		return nil, fmt.Errorf("相关度预判任务要 embedding 引擎与判别模型引擎，缺一个就不该构造它")
	}
	if log == nil {
		log = slog.Default()
	}
	return &SearchJudgeService{repo: repo, emb: emb, dec: dec, cfg: cfg.withDefaults(), log: log, now: time.Now}, nil
}

// WithClock 换时钟（测试用）。
func (s *SearchJudgeService) WithClock(now func() time.Time) *SearchJudgeService {
	s.now = now
	return s
}

// Run 每 Interval 跑一轮，直到 ctx 结束。
func (s *SearchJudgeService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		rep, err := s.RunOnce(ctx)
		if err != nil {
			s.log.WarnContext(ctx, "相关度预判这一轮没跑成，检索照旧按余弦下限", "err", err)
		} else if rep.Judged > 0 || rep.Failures > 0 {
			s.log.InfoContext(ctx, "相关度预判一轮", "judge", rep.Judge, "queries", rep.Queries,
				"judged", rep.Judged, "skipped", rep.Skipped, "failures", rep.Failures)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce 跑一轮。判别模型探测不通时整轮作废；单条查询失败记进 Failures、接着跑下一条。
func (s *SearchJudgeService) RunOnce(ctx context.Context) (SearchJudgeReport, error) {
	judge, err := s.probe(ctx)
	if err != nil {
		return SearchJudgeReport{}, err
	}
	rep := SearchJudgeReport{Judge: judge}
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return rep, err
	}
	for _, m := range merchants {
		if err := s.runMerchant(tenant.NewContext(ctx, m), judge, &rep); err != nil {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			s.log.WarnContext(ctx, "相关度预判：这家店这轮跳过", "merchant_id", m, "err", err)
		}
	}
	return rep, nil
}

// probe 发一道题，拿判别模型的「名字@版本」。
func (s *SearchJudgeService) probe(ctx context.Context) (string, error) {
	resp, err := s.dec.Decide(ctx, inference.SystemOneRequest{
		State: "探测",
		Questions: map[string]inference.SystemOneQuestion{
			"probe": {Type: inference.QuestionNoul, Instructions: "这是一条探测请求吗？"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("探测判别模型: %w", err)
	}
	return resp.Model + "@" + resp.ModelVersion, nil
}

type hotQuery struct {
	norm, display string
	hits          int64
}

// hotQueries 取本店的热词：原样分组的查询按 NormQuery 合并、次数相加，展示用合并前次数最多的那个写法。
func hotQueries(raw []repository.SearchEvalQuery, minHits int64, limit int) []hotQuery {
	byNorm := map[string]*hotQuery{}
	best := map[string]int64{}
	for _, q := range raw {
		n := search.NormQuery(q.Query)
		if n == "" {
			continue
		}
		h, ok := byNorm[n]
		if !ok {
			h = &hotQuery{norm: n}
			byNorm[n] = h
		}
		h.hits += q.Hits
		if q.Hits > best[n] {
			best[n], h.display = q.Hits, q.Query
		}
	}
	out := make([]hotQuery, 0, len(byNorm))
	for _, h := range byNorm {
		if h.hits >= minHits {
			out = append(out, *h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].hits != out[j].hits {
			return out[i].hits > out[j].hits
		}
		return out[i].norm < out[j].norm
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *SearchJudgeService) runMerchant(ctx context.Context, judge string, rep *SearchJudgeReport) error {
	var raw []repository.SearchEvalQuery
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		// 多取几倍：原样分组的查询合并之后条数会变少。
		raw, err = tx.SearchEvalQueries(ctx, s.now().Add(-s.cfg.Window), s.cfg.Queries*5)
		return err
	}); err != nil {
		return err
	}
	qs := hotQueries(raw, s.cfg.MinHits, int(s.cfg.Queries))
	const batch = 32
	for i := 0; i < len(qs); i += batch {
		chunk := qs[i:min(i+batch, len(qs))]
		texts := make([]string, len(chunk))
		for j, q := range chunk {
			texts[j] = q.display
		}
		// 算向量在事务外（search.go 文件头第三节）。
		res, err := s.emb.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("给查询算向量: %w", err)
		}
		for j, q := range chunk {
			rep.Queries++
			if err := s.judgeQuery(ctx, judge, q, res.Vectors[j], rep); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				rep.Failures++
				s.log.WarnContext(ctx, "相关度预判：这条查询这轮跳过", "query", q.display, "err", err)
			}
		}
	}
	return nil
}

func (s *SearchJudgeService) judgeQuery(ctx context.Context, judge string, q hotQuery, vec []float32,
	rep *SearchJudgeReport) error {
	var cands []repository.SearchEvalCandidate
	var need []int64
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		nb, err := tx.SearchEvalVectorNeighbors(ctx, vec, s.cfg.Candidates)
		if err != nil {
			return err
		}
		inAnd := map[int64]bool{}
		if and, _ := search.TSQueryAnd(q.display); and != "" {
			hits, err := tx.SearchEvalKeywordHits(ctx, and, s.cfg.Candidates)
			if err != nil {
				return err
			}
			for _, h := range hits {
				inAnd[h.ID] = true
			}
		}
		ids := make([]int64, 0, len(nb))
		for _, c := range nb {
			if !inAnd[c.ID] {
				cands = append(cands, c)
				ids = append(ids, c.ID)
			}
		}
		need, err = tx.ProductsNeedingJudgment(ctx, q.norm, ids, judge, s.now().Add(-s.cfg.FreshFor))
		return err
	})
	if err != nil {
		return err
	}
	rep.Skipped += len(cands) - len(need)
	if len(need) == 0 {
		return nil
	}
	want := map[int64]bool{}
	for _, id := range need {
		want[id] = true
	}
	pairs := make([]searcheval.Pair, 0, len(need))
	for _, c := range cands {
		if want[c.ID] {
			pairs = append(pairs, searcheval.Pair{Query: q.display, ProductID: c.ID,
				Title: c.Title, Subtitle: c.Subtitle, Category: c.CategoryName})
		}
	}
	labels, err := searcheval.LabelQuery(ctx, s.dec, "", pairs, s.cfg.PerRequest)
	if err != nil {
		return err
	}
	ids := make([]int64, len(labels))
	rels := make([]float32, len(labels))
	for i, l := range labels {
		ids[i], rels[i] = l.ProductID, float32(l.Relevance)
	}
	// 以这批答案里的模型为准（探测之后引擎换过模型的话，写进去的就是新模型的名字，下一轮不会误判成新鲜）。
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.UpsertSearchJudgments(ctx, q.norm, ids, rels, labels[0].Judge)
	}); err != nil {
		return err
	}
	rep.Judged += len(labels)
	return nil
}
