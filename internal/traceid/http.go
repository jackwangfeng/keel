package traceid

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Middleware 给每个请求一个跟踪号：调用方带了合法的就沿用，没有就生成。
// 号放进请求的 context，并写回响应头 X-Trace-Id。
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := Incoming(c.GetHeader(Header), c.GetHeader("traceparent"))
		c.Request = c.Request.WithContext(With(c.Request.Context(), id))
		c.Header(Header, id)
		c.Next()
	}
}

// Query 是分支 URL 上那个参数的名字，给 HTTP 分支入口读。
func Query(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return Normalize(r.URL.Query().Get(queryKey))
}
