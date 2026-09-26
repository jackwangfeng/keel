package handler

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 混合检索：POST /api/v1/search。
//
// # 契约写的是四层，这条 handler 只跑得出前两层 —— 而它不假装
//
// 契约的描述是「双路召回 → RRF 融合 → Reranker 精排 → 业务重排」，
// 后两层是 M5。两处不改契约就能把这件事说清楚，论证在
// service/search.go 的文件头：
//
//	· strategy 回显的是**真的跑过的**那条流水线（"rrf-v1"），不是回显请求值；
//	· explain=true 时 scores 里只出现算过的那几项，rerank / business
//	  **整个不出现**（契约里它们是可选字段，缺席才是实话）。
//
// contract_test.go 的 NotYetImplementedStage 挂着这两笔账，
// search_test.go 的 TestExplainOmitsStagesThatDidNotRun 从另一个方向锁住。
//
// # 为什么 explain 不是 query 参数
//
// 契约把 explain / size / strategy / filters 全放在请求体里（这条接口是 POST）。
// 所以 contract_test.go 那条「handler 读的 query 参数必须与契约对得上」
// 对这条路由的期望是**一个都没有**，routes 表里它带着 NoQueryParams。

// SearchHandler 实现混合检索。
type SearchHandler struct{ svc *service.SearchService }

func NewSearchHandler(s *service.SearchService) *SearchHandler {
	return &SearchHandler{svc: s}
}

// searchResponse 是契约里那个内联的 200 响应。
//
// items 用生成的 api.SearchHit，理由是 package handler 文件头那条硬规矩；
// 外层这四个字段契约里是内联 schema（没有 $ref），生成器没有为它出类型 ——
// 与 intentRequest 那里是同一处够不着的地方，同样记在 defer 里。
//
// trace_id 不在这里：它是可选字段，而它唯一的用处是把一次检索和后续的
// /search/events 回传串起来（契约原话），而 search_logs 那张表本轮没有建、
// /search/events 也没有实现。回一个谁也存不进去的 id 不是「先占个位」，
// 是让客户端以为它拿到的东西有下文。contract_test.go 里挂着这笔账。
type searchResponse struct {
	Items     []api.SearchHit `json:"items"`
	LatencyMs int             `json:"latency_ms"`
	Total     int             `json:"total"`
	Strategy  string          `json:"strategy"`
}

// searchRequest 是契约里那个内联请求体。
type searchRequest struct {
	Query    string             `json:"query"`
	Size     *int               `json:"size"`
	Strategy *string            `json:"strategy"`
	Explain  *bool              `json:"explain"`
	Filters  *api.SearchFilters `json:"filters"`
}

// MaxSearchBodyBytes 是 /search 请求体的大小上限。
//
// 8 KiB。契约允许的最长 query 是 200 个字（UTF-8 下最多 800 字节），
// 加上 filters / size / strategy / explain 与 JSON 的骨架，一个合法请求
// 远在 1 KiB 以内；8 KiB 是留给将来多几个筛选字段的余量。
//
// 为什么这条路由**必须**有它：/search 在契约里是 security: []（公开无鉴权，
// TestSearchIsPublic 钉着），而在此之前全仓库唯一一处请求体大小限制是
// handler/webhook.go 那一处。一个公开的、每次请求都要在 CPU 上跑模型推理的
// 接口，连读多少字节都不限。
//
// 顺序与 webhook.go 那一处相同：**先限大小，再解析**。那边的理由是验签要读
// 完全部字节，这边没有验签，但 ShouldBindJSON 同样会把整个 body 读进内存。
const MaxSearchBodyBytes = 8 << 10

// Search 实现 POST /api/v1/search。
func (h *SearchHandler) Search(c *gin.Context) {
	// latency_ms 是契约里的必填字段，量的是**服务端这一侧**的耗时。
	// 计时从解请求体之前开始：解析、钳制、两路召回、融合、组装都算在里面，
	// 只有网络往返不算（契约里 §8 的预算写着「不含网络」）。
	start := time.Now()

	// 大小闸门在解析之前，形状同 webhook.go。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxSearchBodyBytes)

	var req searchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// 超长与「不是合法 JSON」分开回。两者对调用方是两件事：
		// 前者要它把请求改小（而且它多半是个 bug 或者一次探测），
		// 后者要它去看序列化。都回 422 的话，一个被闸门截断的请求
		// 读起来像「我的 JSON 写错了」。
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem.Write(c, http.StatusRequestEntityTooLarge,
				problem.TypeInvalidRequest,
				fmt.Sprintf("请求体最大 %d 字节", MaxSearchBodyBytes))
			return
		}
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return
	}

	sr := service.SearchRequest{
		Query:   req.Query,
		Filters: defaultSearchFilters(),
	}
	if req.Size != nil {
		sr.Size = *req.Size
	}
	if req.Strategy != nil {
		sr.Strategy = *req.Strategy
	}
	if req.Explain != nil {
		sr.Explain = *req.Explain
	}
	if f := req.Filters; f != nil {
		sr.Filters.CategoryID = f.CategoryId
		sr.Filters.MinPriceCents = (*int64)(f.MinPriceCents)
		sr.Filters.MaxPriceCents = (*int64)(f.MaxPriceCents)
		if f.InStockOnly != nil {
			sr.Filters.InStockOnly = *f.InStockOnly
		}
	}

	res, err := h.svc.Search(c.Request.Context(), sr)
	if errors.Is(err, service.ErrEmptyQuery) {
		// 422 而不是 200 + 空列表：「这串东西里没有可检索的词」与
		// 「搜到了 0 条」对用户是两件事 —— 前者该提示换个词，
		// 后者该提示这家店没有。回空列表会把前者伪装成后者。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "查询词里没有可检索的内容")
		return
	}
	if errors.Is(err, service.ErrQueryTooLong) {
		// 契约给 query 写了 maxLength: 200，而此前这里一个字都没校验。
		//
		// 不校验不是「宽容一点」：向量那一路给引擎的时间是按字数算的
		// （service.DefaultQueryEmbedTimeout 上那段实测），没有上限它就没有
		// 上界 —— 一条请求体闸门之内放得下的超长 query 会让一个**公开无鉴权**
		// 的接口占着引擎跑很久。同形于上面那条：这不是「没搜到」，
		// 是「这串东西搜不了」。
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			fmt.Sprintf("查询词最长 %d 个字", service.MaxQueryRunes))
		return
	}
	if err != nil {
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
		return
	}

	items := make([]api.SearchHit, 0, len(res.Items))
	for _, it := range res.Items {
		max := api.Money(it.MaxPriceCents)
		sales := int(it.SalesCount)
		inStock := it.InStock
		hit := api.SearchHit{
			Id:            it.ID,
			Title:         it.Title,
			Subtitle:      it.Subtitle,
			MinPriceCents: api.Money(it.MinPriceCents),
			MaxPriceCents: &max,
			SalesCount:    &sales,
			InStock:       &inStock,
			Status:        api.SearchHitStatus(it.Status),
		}
		if sr.Explain {
			src := api.SearchHitRecallSource(it.Source)
			hit.RecallSource = &src
			hit.Scores = explainScores(it)
		}
		items = append(items, hit)
	}

	c.JSON(http.StatusOK, searchResponse{
		Items: items,
		// 契约：「本次召回并排序后的结果总数（上限即 size）」。
		// 一期不支持翻页，所以它就是 len(items) —— 不是「库里有多少件匹配」。
		Total:     len(items),
		Strategy:  res.Strategy,
		LatencyMs: int(time.Since(start).Milliseconds()),
	})
}

// searchScores 是 api.SearchHit.Scores 那个**匿名**结构体的别名。
//
// 契约里 SearchHit.scores 是内联对象，没有 $ref，所以生成器没给它名字。
// 用 `type ... =` 别名（不是 defined type）是必须的：别名与匿名结构体是
// 同一个类型，赋得进 hit.Scores；定义成新类型就赋不进去了，
// 而那时唯一的出路是把这六行 tag 在赋值处再抄一遍 —— 抄错一个 json tag
// 不会有编译错误，只会让 explain 少一个字段。
//
// 本轮在报告里把「给 SearchHit.scores 起个名字（$ref 到 components.schemas）」
// 列为 defer，与 intentRequest 那处是同一笔账。
type searchScores = struct {
	Business *float32 `json:"business,omitempty"`
	Final    *float32 `json:"final,omitempty"`
	Keyword  *float32 `json:"keyword,omitempty"`
	Rerank   *float32 `json:"rerank,omitempty"`
	Rrf      *float32 `json:"rrf,omitempty"`
	Vector   *float32 `json:"vector,omitempty"`
}

// explainScores 填 explain=true 时的各阶段得分。
//
// **rerank 与 business 刻意不填。** 它们在契约里是可选字段，而本轮这两层
// 根本没跑（M5）。填 0 会让「这一条被业务规则扣到 0 分」与「没算过」
// 长得一模一样，而 explain 存在的全部意义就是让人看出排序是怎么来的。
// 与下单那边 freight_cents「缺席而不是 0」是同一条纪律。
//
// final 等于 rrf：本轮 RRF 之后没有别的层了。它仍然单独填一份，
// 因为「最终分」这个位置的语义是稳定的 —— 精排上线后填的是精排分，
// 而调用方读的一直是 final。
func explainScores(it service.SearchHit) *searchScores {
	vec := float32(it.VectorScore)
	kw := float32(it.KeywordScore)
	rrf := float32(it.RRFScore)
	final := rrf
	return &searchScores{Vector: &vec, Keyword: &kw, Rrf: &rrf, Final: &final}
}

// defaultSearchFilters 是请求里一个 filters 都没带时生效的那一份。
//
// in_stock_only 默认 **true**，这是契约里写的（SearchFilters.in_stock_only
// 的 default）。它不是一个无关紧要的默认值：它意味着默认搜不到缺货商品。
// 把它写在一个有名字的函数里而不是散在解析代码里，是为了让这条默认值
// 有一个能被直接测的落点 —— 漏掉它的症状是「搜索结果里混进一堆买不到的东西」，
// 而那看上去只是「排序有点怪」。
func defaultSearchFilters() service.SearchFilters {
	return service.SearchFilters{InStockOnly: true}
}
