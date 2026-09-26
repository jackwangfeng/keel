package handler_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// search_logs（数据模型 §8「第一天就要埋」，迁移 00027）的行为闸门。
//
// 两条各管一半：每次成功的检索都写一行、而且写的是**这一次真的发生了什么**；
// 写失败不让检索失败。

// searchLogRow 是读回来的一行。
type searchLogRow struct {
	Query        string
	Strategy     string
	Stages       []string
	RecallIDs    []int64
	RankedIDs    []int64
	LatencyMs    *int32
	TraceID      string
	ModelName    *string
	ModelVersion *string
	UserID       *int64
	ClickedID    *int64
}

// searchLogsOf 用管理员连接读某个商家的全部检索日志，按写入顺序。
//
// 管理员连接绕过 RLS：这里要断言的正是「这一行落在了哪个商家名下」，
// 用租户连接去读就只能看见自己想看见的那一半。
func searchLogsOf(t *testing.T, merchant int64) []searchLogRow {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	rows, err := admin.Query(ctx, `
		SELECT query, strategy, stages, recall_ids, ranked_ids, latency_ms, trace_id,
		       model_name, model_version, user_id, clicked_id
		  FROM search_logs WHERE merchant_id = $1 ORDER BY id`, merchant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []searchLogRow
	for rows.Next() {
		var r searchLogRow
		if err := rows.Scan(&r.Query, &r.Strategy, &r.Stages, &r.RecallIDs, &r.RankedIDs,
			&r.LatencyMs, &r.TraceID, &r.ModelName, &r.ModelVersion, &r.UserID, &r.ClickedID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func responseIDs(r searchResp) []int64 {
	out := make([]int64, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, int64(it["id"].(float64)))
	}
	return out
}

var traceIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// 每次成功的检索写一行，而且 stages / strategy / model 记的是**这一次**真的
// 跑成了什么 —— 三种形态各打一次：默认、rrf-v1、引擎挂了（降级）。
//
// 降级那一次是这张表多出 stages 一列的全部理由（迁移 00027 文件头 ①）：
// strategy 不变，向量那一路却没跑。只看 strategy 的话，那一行会被算进
// 「双路召回」那一桶。
func TestEverySearchWritesALogOfWhatActuallyRan(t *testing.T) {
	fx := newSearchFixture(t)

	type shot struct {
		name       string
		engine     http.Handler
		body       string
		strategy   string
		stages     []string
		withModel  bool
		query      string
		responseOf searchResp
	}
	shots := []*shot{
		{name: "默认", engine: testEngine, body: `{"query":"连衣裙"}`, query: "连衣裙",
			strategy: service.DefaultStrategy, withModel: true,
			stages: []string{"vector", "keyword", "rrf", "business"}},
		{name: "rrf-v1", engine: testEngine, body: `{"query":"碎花 连衣裙","strategy":"rrf-v1"}`,
			query: "碎花 连衣裙", strategy: service.StrategyRRFOnly, withModel: true,
			stages: []string{"vector", "keyword", "rrf"}},
		{name: "引擎挂了", engine: routerWithDeadEngine(t), body: `{"query":"长裙"}`, query: "长裙",
			strategy: service.DefaultStrategy, withModel: false,
			stages: []string{"keyword", "rrf", "business"}},
	}
	for _, s := range shots {
		w, resp := searchOn(t, s.engine, fx.HostA, s.body)
		if w.Code != http.StatusOK {
			t.Fatalf("[%s] 检索返回 %d：%s", s.name, w.Code, w.Body.String())
		}
		s.responseOf = resp
	}

	logs := searchLogsOf(t, fx.MerchantA)
	if len(logs) != len(shots) {
		t.Fatalf("A 店打了 %d 次检索，search_logs 里有 %d 行 —— 每次成功的检索都该写一行",
			len(shots), len(logs))
	}
	seen := map[string]bool{}
	for i, s := range shots {
		got := logs[i]
		if got.Query != s.query {
			t.Errorf("[%s] query = %q，期望 %q", s.name, got.Query, s.query)
		}
		if got.Strategy != s.strategy || got.Strategy != s.responseOf.Strategy {
			t.Errorf("[%s] strategy = %q，期望 %q（且与响应回显 %q 一致）",
				s.name, got.Strategy, s.strategy, s.responseOf.Strategy)
		}
		if !reflect.DeepEqual(got.Stages, s.stages) {
			t.Errorf("[%s] stages = %v，期望 %v", s.name, got.Stages, s.stages)
		}
		if want := responseIDs(s.responseOf); !reflect.DeepEqual(got.RankedIDs, want) {
			t.Errorf("[%s] ranked_ids = %v，期望与响应里的顺序逐个一致 %v", s.name, got.RankedIDs, want)
		}
		if len(got.RankedIDs) == 0 {
			t.Errorf("[%s] ranked_ids 是空的，下面那条「recall 覆盖 ranked」没有对象", s.name)
		}
		recall := map[int64]bool{}
		for _, id := range got.RecallIDs {
			recall[id] = true
		}
		for _, id := range got.RankedIDs {
			if !recall[id] {
				t.Errorf("[%s] ranked_ids 里的 %d 不在 recall_ids %v 里", s.name, id, got.RecallIDs)
			}
		}
		if got.LatencyMs == nil || *got.LatencyMs < 0 {
			t.Errorf("[%s] latency_ms = %v", s.name, got.LatencyMs)
		}
		if !traceIDShape.MatchString(got.TraceID) {
			t.Errorf("[%s] trace_id = %q，期望 32 个十六进制字符", s.name, got.TraceID)
		}
		if seen[got.TraceID] {
			t.Errorf("[%s] trace_id %q 重复", s.name, got.TraceID)
		}
		seen[got.TraceID] = true
		if s.withModel {
			if got.ModelName == nil || *got.ModelName != inference.ModelName ||
				got.ModelVersion == nil || *got.ModelVersion != "concept-fixture" {
				t.Errorf("[%s] model = %v / %v，期望 %q / %q —— 归因要的是这一次 query "+
					"embedding 实际用的模型", s.name, got.ModelName, got.ModelVersion,
					inference.ModelName, "concept-fixture")
			}
		} else if got.ModelName != nil || got.ModelVersion != nil {
			t.Errorf("[%s] 向量路没跑成，model 却是 %v / %v —— 应当是 NULL",
				s.name, got.ModelName, got.ModelVersion)
		}
		if got.UserID != nil || got.ClickedID != nil {
			t.Errorf("[%s] user_id / clicked_id 本轮不该有值：%v / %v", s.name, got.UserID, got.ClickedID)
		}
	}

	// 落在对的租户名下：B 店一次都没搜，就一行都不该有。
	if b := searchLogsOf(t, fx.MerchantB); len(b) != 0 {
		t.Errorf("B 店没有任何检索，search_logs 里却有 %d 行", len(b))
	}

	// 失败的请求（422）不写：日志记的是「一次检索的结果」，没有结果就没有这一行。
	if w, _ := doSearch(t, fx.HostA, `{"query":"！？"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("纯标点查询返回 %d，期望 422", w.Code)
	}
	if n := len(searchLogsOf(t, fx.MerchantA)); n != len(shots) {
		t.Errorf("一次 422 之后 search_logs 从 %d 行变成了 %d 行", len(shots), n)
	}
}

// 写日志失败，检索照常返回，并留下一条 ERROR。
//
// 故障注入在仓储那一层（InsertSearchLog 报错），与 TestSearchStillReturnsWhenKeywordRouteFails
// 同一个形状。断言分两半：检索真的成功了（有结果、没报错）；故障真的被走到了
// （日志里有 SearchLogFailed 那句）—— 少了后一半，「检索成功」也可能只是
// 这条路径根本没去写日志。
func TestSearchLogFailureDoesNotFailTheSearch(t *testing.T) {
	fx := newSearchFixture(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	svc := service.NewSearchService(logBrokenRepo{inner: repository.New(testPool)},
		conceptEmbedder{}, service.SearchConfig{}, nil)
	res, err := svc.Search(tenant.NewContext(t.Context(), fx.MerchantA),
		service.SearchRequest{Query: "连衣裙"})
	if err != nil {
		t.Fatalf("写检索日志失败让整次检索失败了：%v —— 日志是辅助数据，"+
			"§8「任何一环故障，搜索都必须仍能返回结果」", err)
	}
	if len(res.Items) == 0 {
		t.Fatal("写日志失败之后检索返回了空列表")
	}
	if !strings.Contains(buf.String(), service.SearchLogFailed) {
		t.Fatalf("注入的日志故障没有留下 ERROR（期望含 %q）。日志：%s",
			service.SearchLogFailed, buf.String())
	}
	if n := len(searchLogsOf(t, fx.MerchantA)); n != 0 {
		t.Errorf("注入了写入故障，search_logs 里却有 %d 行 —— 故障没注入到写日志那条路径上", n)
	}
}

// logBrokenRepo 让写检索日志那一条报错，别的照常。
type logBrokenRepo struct{ inner *repository.Repo }

func (r logBrokenRepo) WithTenant(ctx context.Context, fn func(repository.Tx) error) error {
	return r.inner.WithTenant(ctx, func(tx repository.Tx) error {
		return fn(brokenLogTx{Tx: tx})
	})
}

type brokenLogTx struct{ repository.Tx }

func (brokenLogTx) InsertSearchLog(context.Context, repository.SearchLog) error {
	return errors.New("注入的故障：search_logs 写不进去")
}
