package app

import (
	"log/slog"

	"github.com/gin-gonic/gin"
)

// logHandlerErrors 把 gin 攒在 c.Errors 里的错误写进日志。
//
// 没有它的时候，全仓库 12 处 `_ = c.Error(err)` 是**黑洞**：gin 的 c.Error 只是
// 往 c.Errors 里 append，没有任何人 drain，等于丢弃。而好几处代码的注释是照着
// 「这条会留一条 Error 日志」写的：
//
//   - handler/webhook.go「除验签失败外一律 200，每一种不正常都留一条 Error 级日志」
//   - service/payment.go（订单查不到）「钱可能是真的到账了，所以这条要响」
//
// 那不是设计取舍，是事实陈述，而在挂上这道中间件之前它是假的。验收用一次真撞车
// 量出了代价：300 笔已过期订单的回调掐在补偿轮次中间发出，事后库里 44 笔处于
// 「支付单已入账 / 订单被关到 90 / 库存已回补」，合计约 ¥2205，而日志里
// 一条都没有。支付回调把「钱到了货没扣」从一条不变量降级成了「支付单留在库里 +
// 一条日志给人看」这个运维约定 —— 支付单确实留下了，那条日志不存在。
//
// 级别一律 Error：能走到 c.Error 的都是「有人得知道」的事。真正的正常路径
// （空补偿、重复回调、幂等重放）在各自的 handler 里根本不会调 c.Error。
//
// 刻意不记请求体：webhook 的报文里有金额与流水号，登录的报文里有口令。
// 需要报文的那一处（payments.notify_payload）已经落了库，那里有 RLS 管着。
func logHandlerErrors() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if len(c.Errors) == 0 {
			return
		}
		log := slog.Default()
		for _, e := range c.Errors {
			log.ErrorContext(c.Request.Context(), "请求出错",
				slog.String("method", c.Request.Method),
				slog.String("path", c.FullPath()),
				slog.Int("status", c.Writer.Status()),
				slog.String("error", e.Error()))
		}
	}
}
