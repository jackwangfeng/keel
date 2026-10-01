package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 搜索行为回传：POST /api/v1/search/events。
//
// 规则（首次为准、只认 ranked_ids、租户隔离）都在 service/search_event.go 的文件头，
// 这里只做契约的形状：解请求体、把 service 的三种错误翻成 404 / 422、成功回 204。
//
// 与 /search 分在两个文件里，是 contract_test.go 那张路由表的要求：
// query 参数的对账按 HandlerFile 解析整份源码，两条接口同文件会互相牵连。
// 两条都一个 query 参数都没有，但各自登记，各管各的。

// MaxSearchEventBodyBytes 是 /search/events 请求体的大小上限。
//
// 1 KiB。一个合法请求是 32 字符的 trace_id、一个不超过 8 字符的 event、
// 一个 int64，加上 JSON 骨架不到 100 字节。理由同 MaxSearchBodyBytes：
// 这是一条公开无鉴权的接口，读多少字节必须有上限，而且要在解析之前。
const MaxSearchEventBodyBytes = 1 << 10

// Event 实现 POST /api/v1/search/events。
func (h *SearchHandler) Event(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxSearchEventBodyBytes)

	var req api.PostSearchEventsJSONRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem.Write(c, http.StatusRequestEntityTooLarge, problem.TypeInvalidRequest,
				fmt.Sprintf("请求体最大 %d 字节", MaxSearchEventBodyBytes))
			return
		}
		problem.WriteBindError(c, err)
		return
	}

	_, err := h.svc.RecordSearchEvent(c.Request.Context(), req.TraceId,
		service.SearchEvent(req.Event), req.ProductId)
	switch {
	case err == nil:
		// 写下了与「那一列此前已有值、这次没覆盖」回同一个 204：契约写明不区分，
		// 客户端对两者该做的事一样 —— 什么都不用做。
		c.Status(http.StatusNoContent)
	case errors.Is(err, service.ErrSearchTraceNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound,
			"trace_id 不存在或不属于当前店铺")
	case errors.Is(err, service.ErrProductNotInSearchResults):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"product_id 不是这次检索返回过的商品")
	case errors.Is(err, service.ErrInvalidSearchEvent):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, err.Error())
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
	}
}
