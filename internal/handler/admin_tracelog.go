package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 访问日志的运行时开关（internal/traceid/access.go、service/tracelog.go）：
//
//	GET  /api/v1/admin/trace-log当前采样率、白名单、累计计数
//	POST /api/v1/admin/trace-log        改采样率 / 开关某个 trace_id
//
// **存在的理由**：排障时最该做的那一步（「把这几个请求的全过程打出来」）不该
// 需要重启进程，也不该需要改环境变量。有了它，运维在后台点一下或者curl 一下，
// 几秒后就能在日志里搜到那条号；查完关掉，噪声不留在那里。
//
// 采样率是**百分之一为单位**（25 = 25%，100 = 全量）。用整数而不是 "0.25"
// 是为了让「开关」这件事在任何客户端里都是一串字符，不需要处理浮点。
//
// 权限：staffAuth + service 层再判一次「必须是管理员」。**两道都要**——
// staffAuth 只验「这个人是谁」，不验「这个人能不能做这件事」；而采样率与
// 白名单是进程级状态，不按租户分，运营点了能把生产的日志量放大几十倍。
// 那一道判据在 service/tracelog.go 的 requireTraceLogAdmin。

type AdminTraceLogHandler struct{ svc *service.TraceLogService }

func NewAdminTraceLogHandler(s *service.TraceLogService) *AdminTraceLogHandler {
	return &AdminTraceLogHandler{svc: s}
}

// Get 实现 GET /admin/trace-log。
func (h *AdminTraceLogHandler) Get(c *gin.Context) {
	st, err := h.svc.Get(c.Request.Context())
	if err != nil {
		writePermissionError(c, err)
		return
	}
	// 生成器把 YAML 的 `integer` 映成Go 的 int，而 traceid 那边为了读它不落
	// 数据竞争用的是 atomic.Int64。两边都要 int64 的话就在这里转一次 ——
	// 转换点收在一个地方，换生成器类型时只有这一处要改。
	out := api.TraceLogState{
		SampleRate: int(st.SampleRate),
		Kept:       int(st.Kept),
		Dropped:    int(st.Dropped),
		ForcedHit:  int(st.ForcedHit),
		Forced:     st.Forced,
		SlowMillis: st.SlowMillis,
	}
	if st.SampleRateHint != "" {
		out.SampleRateHint = &st.SampleRateHint
	}
	if st.ForcedHint != "" {
		out.ForcedHint = &st.ForcedHint
	}
	c.JSON(http.StatusOK, out)
}

// traceLogPatch 是 POST 的请求体。三个字段都是可选的，只有带了的那个生效。
type traceLogPatch struct {
	// SampleRate *int64：nil 表示不改。要「关回去」得显式传 0 而不是省略。
	SampleRate *int64 `json:"sample_rate"`
	// TraceID 与 Force 一起用：Force=true 加进白名单，false 移出。
	TraceID string `json:"trace_id"`
	Force   *bool  `json:"force"`
}

// Set 实现 POST /admin/trace-log。
func (h *AdminTraceLogHandler) Set(c *gin.Context) {
	var p traceLogPatch
	if err := c.ShouldBindJSON(&p); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "请求体读不出来")
		return
	}
	if _, err := h.svc.Set(c.Request.Context(), p.SampleRate, p.TraceID, p.Force); err != nil {
		// 角色不够与参数不合法都是调用方能自己改正的 4xx，但**说错的原因不同**：
		// 前者「你这个身份不行」（403 role-forbidden），后者「你写错了」（422）。
		// writePermissionError 只翻前一种，它翻了就不该再往下走。
		if writePermissionError(c, err) {
			return
		}
		if errors.Is(err, service.ErrStaffBadRequest) {
			detail := err.Error()
			problem.WriteValue(c, http.StatusUnprocessableEntity, api.Problem{
				Type:   problem.TypeInvalidRequest,
				Title:  "请求参数不合法",
				Status: http.StatusUnprocessableEntity,
				Detail: &detail,
			})
			return
		}
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
		return
	}
	// 改完（或者本来一个字段都没带，是空操作）都回当前状态，
	// 让调用方拿到真实数字而不必再 GET 一次。
	h.Get(c)
}