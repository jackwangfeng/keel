package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/service"
)

// 我的订单：GET /api/v1/orders。
//
// 单独一个文件，理由同 product_detail.go 的文件头：contract_test.go 按文件做
// query 参数的双向对账，而同一个文件里放一条「有四个 query 参数」的接口和一条
// 「一个都没有」的接口（POST /orders），那条对账就失去区分力了。

// orderListResponse 是契约里 `allOf: [PageMeta, {items}]` 的 Go 形状。
// 内嵌 api.PageMeta 的理由见 listResponse（product.go）。
type orderListResponse struct {
	api.PageMeta
	Items []api.Order `json:"items"`
}

// List 实现 GET /api/v1/orders。
//
// **它返回的永远是当前 bearer 令牌那个买家的订单。** 这条接口上没有任何一个
// 参数能指定「读谁的订单」—— 买家身份只从令牌来（service.OrderService.ListMine
// 里 auth.FromContext 那一句）。这不是省了一个参数，是这条接口唯一的越权面：
// 租户由 RLS 挡住，而同店买家之间只有那个 user_id 条件挡着。
//
// 错误路径走 writeOrderError 的兜底 500，而不是某个 4xx：契约里这条接口的
// 响应集合只有 200，连 default 都没有，所以任何 4xx 都在契约之外
// （这处缺口已在报告里列为 defer：给 GET /orders 补 default: Problem）。
func (h *OrderHandler) List(c *gin.Context) {
	// 解析失败就当没传，同 /products：`?page=abc` 与不带 page 对客户端是同一件事。
	// 越界值的钳制在 service（那里与 /products 共用同一个 clampPaging）。
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	// 两个筛选参数的名字必须以**字符串字面量**的形式出现在 c.Query 里，
	// 不能包进一个 smallintQuery(c, name) 那样的小助手。
	// contract_test.go 的参数对账是用 AST 读这些字面量的，包进去之后它读到的是
	// 一个变量名，那条测试会当场说「跟不过去」—— 而它那么做是对的：
	// 拼出来的参数名让「handler 到底读了哪些参数」不可静态判定。
	list, err := h.svc.ListMine(c.Request.Context(), page, pageSize, service.OrderFilter{
		Status:       parseSmallint(c.Query("status")),
		RefundStatus: parseSmallint(c.Query("refund_status")),
	})
	if err != nil {
		writeOrderError(c, err)
		return
	}

	items := make([]api.Order, 0, len(list.Items))
	for _, o := range list.Items {
		items = append(items, apiOrder(o))
	}
	c.JSON(http.StatusOK, orderListResponse{
		PageMeta: api.PageMeta{
			Page:     list.Page,
			PageSize: list.PageSize,
			Total:    int(list.Total),
		},
		Items: items,
	})
}

// parseSmallint 把一个可选的 smallint 筛选参数解出来。空串或读不动就返回 nil
// （= 不筛）。
//
// # 为什么读不动是「不筛」而不是 400
//
// 契约里这条接口的响应集合**只有 200**：它连 default 都没有，所以一个 4xx
// 在契约里根本说不出来（同 /orders/preview 那处缺口，已在报告里列为 defer）。
// 在这个前提下，把 `?status=abc` 变成一个契约外的错误响应，比忽略它更糟 ——
// 按契约生成的客户端会在一个它不认识的响应上解析失败。
//
// # 为什么不筛，而不是「筛出空列表」
//
// 两者的差别只在无效输入上，但方向相反：忽略等于「你没说要筛」，
// 而空列表等于「按你说的筛，没有匹配的」。前者对一个打错字的客户端是可恢复的
// （它会看到自己的订单，然后发现参数名写错了），后者会让它以为这个买家没有订单。
//
// # 合法但不在枚举里的值（比如 status=99）会怎样
//
// 它会照直进 WHERE，匹配不到任何行，返回一个 total = 0 的空页。那是对的：
// 客户端要求的确实是一批不存在的订单。**不需要**在这里对着契约的枚举再校验
// 一遍 —— 那份枚举会随契约演进，而一份抄在 Go 里的副本会在演进之后把
// 新合法的状态判成非法。
func parseSmallint(raw string) *int16 {
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseInt(raw, 10, 16)
	if err != nil {
		return nil
	}
	n := int16(v)
	return &n
}
