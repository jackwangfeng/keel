package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// MCP 服务：AI 员工的工具入口（AI 经营 M9 任务 2，docs/AI经营-M9设计.md §3）。
//
//	POST/GET /api/v1/mcp   streamable HTTP，挂 auth.AgentBearer（kagt_ 密钥，租户按 Host）
//
// # 为什么跑在 API 进程里、放在 handler 包里
//
// 工具就是现有能力的另一个入口：它调用与后台接口**同一批 service 函数**（判权逐字相同），
// 返回值用后台接口**同一批 api 转换函数**（字段名、单位与契约一致），错误也借后台接口的错误出口翻成
// 同一个 problem type（mcpError）。分开部署或另起一个包，就要把这三样各抄一份，而抄出来的迟早会分叉。
//
// # 无状态（Stateless）
//
// 每个 HTTP 请求都重新过 AgentBearer：tool 处理函数拿到的 ctx 就是这一次请求的（SDK 在无状态模式下
// 用 req.Context() 建会话），于是吊销密钥、停用 AI 员工、收窄范围都在下一次调用立即生效。
// 有状态模式下 ctx 来自建会话的那一次请求，一个长会话会一直带着旧身份 —— 不能要。
//
// # 审计与限流
//
// 每次工具调用在返回之后写一行 agent_tool_calls（写失败只记日志）；每把密钥每分钟至多 mcpRateLimit 次
// HTTP 请求，超了回 429 + Retry-After（harness 都会按它退避）。

// mcpRateLimit 是每把密钥每分钟的请求上限。
const mcpRateLimit = 120

// MCPDeps 是 MCP 工具要用到的 service。
type MCPDeps struct {
	Staff   *service.StaffService
	Reports *service.ReportService
	Stores  *service.AdminStoreService
	Catalog *service.AdminCatalogService
	Orders  *service.AdminOrderService
	Restock *service.RestockService
	// Proposals 是提案（任务 4）：AI 员工只能提、看自己的；批准 / 驳回在后台接口。
	Proposals *service.AgentProposalService
	// Briefs 是简报（任务 5）。
	Briefs *service.AgentBriefService
	// SlowMovers 与 PromotionReview 是 M10 的计算工具（docs/AI经营-M10M11设计.md §2），
	// 注册在 mcp_tools_compute.go。
	SlowMovers      *service.SlowMoversService
	PromotionReview *service.PromotionReviewService
	Log     *slog.Logger
	// Version 进 MCP 的 serverInfo，agent 能看到连的是哪个版本。
	Version string
}

// NewMCPHandler 建 MCP 服务并返回挂在 gin 上的处理函数（前面要先挂 auth.AgentBearer）。
func NewMCPHandler(d MCPDeps) gin.HandlerFunc {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "keel", Title: "Keel 电商 · AI 员工工具", Version: d.Version},
		&mcp.ServerOptions{Instructions: mcpInstructions})
	registerMCPTools(srv, &d)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: d.Log,
			// SDK 的 DNS 重绑定防护：进程监听在回环地址、Host 却不是 localhost 就 403。它防的是「本机 MCP 服务
			// 靠浏览器自动带的凭据被恶意网页调用」。这里不适用：每个请求都要显式带 kagt_ 密钥（没有 cookie 之类
			// 会被自动带上的凭据），而且本服务本来就经反向代理按店铺域名访问（Host 必然不是 localhost，租户靠它解析）。
			DisableLocalhostProtection: true})
	lim := newKeyLimiter(mcpRateLimit, time.Minute)
	return func(c *gin.Context) {
		id, err := auth.StaffFromContext(c.Request.Context())
		if err != nil || !id.IsAgent() {
			problem.Write(c, http.StatusUnauthorized, problem.TypeUnauthorized, "需要接入密钥")
			return
		}
		if wait, ok := lim.allow(id.AgentKeyID, time.Now()); !ok {
			c.Header("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			problem.Write(c, http.StatusTooManyRequests, problem.TypeRateLimited, "这把接入密钥每分钟的调用次数用完了，稍后再试")
			return
		}
		h.ServeHTTP(c.Writer, c.Request)
	}
}

// mcpInstructions 进 initialize 的 instructions：agent 连上时看到的第一段话。完整的纪律在仓库的 agent/AGENTS.md。
const mcpInstructions = `你是这家店的一名 AI 员工，权限与你的角色和管辖范围一致，越权会被拒绝。
数字只引用工具返回的，不要自己估算；金额单位是「分」，同时附有人读的元。
M9 阶段你只能读数据、计算、写简报、提出提案；提案要人在后台批准后才会执行。提之前先用 list_my_proposals 看有没有同样的待处理提案。
完整的工作纪律与手册见仓库 agent/AGENTS.md 与 agent/skills/。`

// mcpTool 注册一个工具：统一做审计与错误翻译。fn 返回的 Out 会作为结构化结果给 agent。
func mcpTool[In, Out any](srv *mcp.Server, d *MCPDeps, name, desc string, writeErr func(*gin.Context, error),
	fn func(ctx context.Context, in In) (Out, error)) {
	// outputSchema 由 Out 推出（mcpOutputSchema）并显式给出：接入方据此知道返回的形状，SDK 每次返回前按它校验。
	// 处理函数的输出类型仍声明成 any，只是为了让 SDK 不再自己推一遍（它推不出 openapi 的日期类型）。
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: desc, OutputSchema: mcpOutputSchema[Out](name)},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			out, err := fn(ctx, in)
			errType := ""
			var res *mcp.CallToolResult
			if err != nil {
				var text string
				var pr mcpProblem
				pr, text = mcpError(err, writeErr)
				errType = pr.Type
				if errType == problem.TypeInternal {
					d.Log.ErrorContext(ctx, "MCP 工具调用出错", "tool", name, "err", err)
				}
				// 文字给模型读；_meta["keel/problem"] 给接入方的程序判断（type 与后台接口的 problem type 逐字相同）。
				res = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}},
					Meta: mcp.Meta{"keel/problem": pr}}
			}
			mcpAudit(ctx, d, name, in, err == nil, errType, time.Since(start))
			if err != nil {
				return res, nil, nil
			}
			return nil, out, nil
		})
}

// mcpSchemaTypes 是推 outputSchema 时要特殊处理的类型：openapi 的 Date 内嵌 time.Time，jsonschema 推不出。
var mcpSchemaTypes = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[openapi_types.Date](): {Type: "string", Format: "date"},
}

// mcpOutputSchema 从 Go 类型推出工具的 outputSchema。推不出是装配错误（新工具用了没登记的类型），启动即 panic。
func mcpOutputSchema[Out any](tool string) *jsonschema.Schema {
	s, err := jsonschema.ForType(reflect.TypeFor[Out](), &jsonschema.ForOptions{TypeSchemas: mcpSchemaTypes})
	if err != nil {
		panic(fmt.Sprintf("MCP 工具 %s 的 outputSchema 推不出来：%v", tool, err))
	}
	return s
}

// mcpProblem 是工具出错时放进 _meta["keel/problem"] 的形状（docs/AI接口.md「错误」）。
type mcpProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// mcpError 借后台接口的错误出口把 err 翻成 problem：写进一个内存里的响应，再把 type / title / detail 读回来。
// 这样 agent 拿到的错误类型与后台接口逐字相同，不必再维护一份映射。内部错误不带 detail（不外泄）。
func mcpError(err error, writeErr func(*gin.Context, error)) (mcpProblem, string) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	writeErr(c, err)
	var p struct {
		Type   string  `json:"type"`
		Title  string  `json:"title"`
		Detail *string `json:"detail"`
	}
	if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Type == "" {
		return mcpProblem{Type: problem.TypeInternal, Title: "服务内部错误", Status: http.StatusInternalServerError},
			"服务内部错误"
	}
	out := mcpProblem{Type: p.Type, Title: p.Title, Status: w.Code}
	text := p.Title + "（" + p.Type + "）"
	if p.Detail != nil && *p.Detail != "" && p.Type != problem.TypeInternal {
		out.Detail = *p.Detail
		text += "：" + *p.Detail
	}
	return out, text
}

func mcpAudit(ctx context.Context, d *MCPDeps, tool string, args any, ok bool, errType string, dur time.Duration) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return
	}
	raw, _ := json.Marshal(args)
	if err := d.Staff.RecordAgentToolCall(ctx, repository.AgentToolCall{StaffID: id.StaffID, KeyID: id.AgentKeyID,
		Tool: tool, Args: raw, OK: ok, ErrorType: errType, DurationMS: int32(dur.Milliseconds())}); err != nil {
		d.Log.ErrorContext(ctx, "AI 员工工具调用的审计没写进去", "tool", tool, "err", err)
	}
}

// keyLimiter 是每把密钥的固定窗口计数器（进程内；多实例部署下是每实例一份，与 /search 的限流同一个已知口径）。
type keyLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[int64]*keyWindow
}

type keyWindow struct {
	start time.Time
	n     int
}

func newKeyLimiter(limit int, window time.Duration) *keyLimiter {
	return &keyLimiter{limit: limit, window: window, hits: map[int64]*keyWindow{}}
}

func (l *keyLimiter) allow(key int64, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= l.window {
		if len(l.hits) > 10000 { // 密钥数量有限，这只是防一个坏客户端撑爆 map
			l.hits = map[int64]*keyWindow{}
		}
		l.hits[key] = &keyWindow{start: now, n: 1}
		return 0, true
	}
	if w.n >= l.limit {
		return l.window - now.Sub(w.start), false
	}
	w.n++
	return 0, true
}
