package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/problem"
)

// writeInventoryUnavailable 把「库存服务没回答」写成 503 inventory-unavailable，
// 返回是否写了（微服务拆分阶段 1a）。
//
// 只在拆分部署下会命中：单体形态的库存是进程内调用，没有「没回答」这回事。
// 挂在每一个会调库存服务的错误映射最前面 —— 它不能落进默认分支变成 500：500 告诉客户端
// 「服务端有 bug」，而这里的真相是「一个依赖暂时不在，过一会儿原样重试」。
//
// 写接口上这一条的含义要说清楚（detail 里也写着）：库存那一笔可能已经生效。客户端带着
// 同一个 Idempotency-Key 重试即可 —— 相对调整按 biz_id 幂等、比较并设置有自己的前提条件、
// 建 SKU / 导入的初始库存可以放心重建（见 service/inventory_admin.go）。
//
// Retry-After 给一个保守的秒数：库存服务的恢复是编排系统拉起一个容器的量级。
func writeInventoryUnavailable(c *gin.Context, err error) bool {
	if !inventory.IsUnavailable(err) {
		return false
	}
	_ = c.Error(err)
	c.Header("Retry-After", "5")
	// detail 是固定文案，不带错误原文：原文里有内网地址。
	detail := "库存服务此刻没有回答。写操作可能已经生效，也可能没有：稍后用同一个 Idempotency-Key 原样重试即可，不会重复生效"
	problem.WriteValue(c, http.StatusServiceUnavailable, api.Problem{
		Type: problem.TypeInventoryUnavailable, Title: "库存服务暂时不可用",
		Status: http.StatusServiceUnavailable, Detail: &detail,
	})
	return true
}
