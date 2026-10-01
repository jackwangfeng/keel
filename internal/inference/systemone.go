package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// System One 判别模型（Kev / Jev）的客户端：给一段共享的状态和几道结构化的题，回每道题的概率，
// 不生成文字。第一个用途是离线给检索评测集打标（cmd/keel-searcheval label）。
//
// 线上格式与 TypeSafe 的 `/v1/systemone` 逐字段一致（infero docs/keel-integration.md 第 5 节）：
// Jev 云端、Kev 自带的服务、infero 都认这一份，所以换后端只换地址。
//
// 与 Embedder 分开配地址：判别模型和 embedding 模型不一定在同一个引擎进程里。

// EnvSystemOneEndpoint 是判别模型引擎的地址，形如 http://127.0.0.1:18095。
const EnvSystemOneEndpoint = "KEEL_SYSTEMONE_ENDPOINT"

// EnvSystemOneToken 是引擎要的 Bearer 令牌（共享的演示实例、Jev 云端要；本机引擎不要）。
// 只从环境变量读，不进命令行参数与配置文件：参数会留在 shell 历史和进程列表里。
const EnvSystemOneToken = "KEEL_SYSTEMONE_TOKEN"

// ErrRateLimited 是引擎回了 429。它同时也是 ErrUnavailable（errors.Is 两个都成立）：
// 只想降级的调用方不用认识它，离线批量的调用方可以认出它来退避重试。
var ErrRateLimited = errors.New("推理引擎限流")

// SystemOnePath 是引擎上那条接口的路径。
const SystemOnePath = "/v1/systemone"

// DefaultSystemOneTimeout 是单次请求的默认上限：一次请求里可以有几十道题（离线打标一条查询配几十件候选）。
const DefaultSystemOneTimeout = 60 * time.Second

// 题型（SystemOneQuestion.Type）。
const (
	QuestionNoul   = "noul"   // 是非题：criteria 是 {"true": 说明, "false": 说明}
	QuestionChoice = "choice" // 选择题：criteria 是 {选项名: 说明}
	QuestionScore  = "score"  // 打分题：criteria 是从低到高的各档说明
)

// SystemOneQuestion 是一道题。Instructions 与 Criteria 原样序列化（字符串、对象、数组都行，引擎把它们渲染成文本）。
type SystemOneQuestion struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// SystemOneRequest 是一次请求：一段状态 + 若干道题（键是调用方起的题号，答案按同一个键回来）。
type SystemOneRequest struct {
	State     any                          `json:"state"`
	Model     string                       `json:"model,omitempty"`
	Questions map[string]SystemOneQuestion `json:"questions"`
}

// SystemOneAnswer 是一道题的答案。按 Type 读对应的字段。
type SystemOneAnswer struct {
	Type string `json:"type"`
	// Noul 是是非题答「是」的概率。
	Noul *float64 `json:"noul,omitempty"`
	// Choice / Confidence：选择题选中的选项与它的概率；打分题的 Confidence 是最可能那一档的概率。
	Choice     string  `json:"choice,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// Score 是打分题的期望值（0–1）。
	Score *float64 `json:"score,omitempty"`
	// Probabilities 是选择题各选项 / 打分题各档的概率。
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// SystemOneResponse 是一次请求的答案，Answers 的键与请求的 Questions 一一对应。
type SystemOneResponse struct {
	Model        string                     `json:"model"`
	ModelVersion string                     `json:"model_version"`
	Answers      map[string]SystemOneAnswer `json:"answers"`
}

// SystemOneClient 是 SystemOnePath 的客户端。零值不可用，走 NewSystemOne。
type SystemOneClient struct {
	endpoint string
	token    string
	timeout  time.Duration
	hc       *http.Client
}

// NewSystemOne 建客户端。endpoint 为空时拒绝（与 New 同一个理由：地址必须显式指定）。
// token 非空时每次请求带 `Authorization: Bearer <token>`。
// timeout <= 0 用 DefaultSystemOneTimeout；hc 为 nil 时自己建一个。
func NewSystemOne(endpoint, token string, timeout time.Duration, hc *http.Client) (*SystemOneClient, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("没有配置判别模型引擎地址（%s），拒绝构造客户端", EnvSystemOneEndpoint)
	}
	if timeout <= 0 {
		timeout = DefaultSystemOneTimeout
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &SystemOneClient{endpoint: endpoint, token: token, timeout: timeout, hc: hc}, nil
}

// SystemOneFromEnv 按 EnvSystemOneEndpoint 建客户端。
func SystemOneFromEnv() (*SystemOneClient, error) {
	return NewSystemOne(os.Getenv(EnvSystemOneEndpoint), os.Getenv(EnvSystemOneToken), 0, nil)
}

// Decide 发一次请求。题号一个都没有、答案缺题、题型对不上、概率不在 [0, 1] 都报错 ——
// 一个缺了的答案当成 0 写进标签，会被校准当成「不相关」，悄悄把下限往下拉。
func (c *SystemOneClient) Decide(ctx context.Context, r SystemOneRequest) (*SystemOneResponse, error) {
	if len(r.Questions) == 0 {
		return nil, fmt.Errorf("%w: 一道题都没有", ErrRejected)
	}
	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("%w: 序列化请求失败: %v", ErrProtocol, err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint+SystemOnePath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: 构造请求失败: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	httpResp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: 打 %s 失败（%d 道题，单次上限 %s）: %w",
			ErrUnavailable, c.endpoint, len(r.Questions), c.timeout, err)
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: 读响应失败: %v", ErrUnavailable, err)
	}
	switch {
	case httpResp.StatusCode == http.StatusOK:
	case httpResp.StatusCode == http.StatusTooManyRequests:
		// 限流是会自己好的一类：调用方按 ErrUnavailable 等一会儿再来，不当成请求本身有错。
		return nil, fmt.Errorf("%w: %w（429）: %s", ErrUnavailable, ErrRateLimited, snippet(raw))
	case httpResp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: 引擎回了 %d: %s", ErrUnavailable, httpResp.StatusCode, snippet(raw))
	default:
		return nil, fmt.Errorf("%w: 引擎回了 %d: %s", ErrRejected, httpResp.StatusCode, snippet(raw))
	}
	var resp SystemOneResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: 解析响应失败: %v（%s）", ErrProtocol, err, snippet(raw))
	}
	if resp.ModelVersion == "" {
		return nil, fmt.Errorf("%w: 引擎没有返回 model_version：标签记不了来源，换模型后分不清哪些要重打", ErrProtocol)
	}
	for id, q := range r.Questions {
		a, ok := resp.Answers[id]
		if !ok {
			return nil, fmt.Errorf("%w: 题 %q 没有答案", ErrProtocol, id)
		}
		if a.Type != q.Type {
			return nil, fmt.Errorf("%w: 题 %q 问的是 %s，答的是 %s", ErrProtocol, id, q.Type, a.Type)
		}
		if q.Type == QuestionNoul && (a.Noul == nil || *a.Noul < 0 || *a.Noul > 1) {
			return nil, fmt.Errorf("%w: 题 %q 的 noul 缺失或不在 [0, 1]", ErrProtocol, id)
		}
	}
	return &resp, nil
}
