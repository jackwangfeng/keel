package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 买家侧的两条读接口：GET /orders（我的订单）与 GET /orders/{order_no}（订单详情）。
//
// # 这两条接口的全部难点是一句话：订单只能看见自己的
//
// 租户由 RLS 挡住，而**同一家店的 A 买家不能读到 B 买家的订单**是 RLS 管不到的
// 那一层：策略里只有 current_merchant()，它认不出买家。所以这一层的每一次读都
// 必须带上 auth.FromContext 取出来的 user_id，并且那个值**只能**从上下文来 ——
// 从请求参数来的话，「读谁的订单」就成了调用方自己声明的事。
//
// 取不到身份时返回 auth.ErrNoUser 而不是回落到「全部订单」：契约里这两条接口
// 继承全局 bearerAuth，取不到只可能是中间件没挂，而那时回落的后果是匿名请求
// 读到全店的订单。

// ErrOrderNotFound：这个单号在当前买家名下查不到。契约：404。
//
// 它同时覆盖「没有这一单」「这一单是别人的」「这一单还是 status = 0 创建中」，
// 与 repository.ErrOrderNotFound 的理由一致：order_no 不可枚举，分开报会把这条
// 接口变成一个单号存在性判定器。
var ErrOrderNotFound = errors.New("订单不存在")

// OrderFilter 是「我的订单」上两个正交的筛选条件。
type OrderFilter struct {
	Status       *int16
	RefundStatus *int16
}

// OrderList 是一页订单，带上**钳制之后**的分页参数。
//
// 形状与 ProductList 一样，理由也一样（写在那里）：回显原始输入会让客户端按一个
// 它其实没拿到的 page_size 去算总页数。Total 是全部条数，不是本页条数。
type OrderList struct {
	Items    []repository.Order
	Page     int
	PageSize int
	Total    int64
}

// OrderDetail 是一笔订单的全部（契约的 OrderDetail）。
type OrderDetail struct {
	Order    repository.Order
	Receiver ReceiverSnapshot
	Store    StoreSnapshot
	Items    []repository.OrderItem
	Payments []repository.Payment
	// Refunds 是这一单的全部退款单，按申请时间倒序（契约 OrderDetail.refunds）。
	Refunds []repository.Refund
}

// StoreSnapshot 是从 orders.store_snapshot 里读回来的门店 / 大区展示信息
// （契约的 OrderStoreSnapshot）。
//
// 字段的 json tag 逐字对着写快照那条 SQL 的 jsonb_build_object
// （db/queries/orders.sql 的 CreateOrderDraft）。两边任何一处改名，
// 结果都是这里静默地全是空值 —— 详情页显示一家没有名字的门店，
// 而不是一次报错。钉住它的是 order_query_test.go 里那条「下单再读回来，
// 门店名逐字相同」的断言。
//
// **快照里刻意没有 id**：放了就会有人去 GROUP BY 它，而聚合该走
// orders.store_id 那一列（真外键、有索引）。
type StoreSnapshot struct {
	StoreName  string `json:"store_name"`
	RegionName string `json:"region_name"`
	Address    string `json:"address"`
	Phone      string `json:"phone"`
}

// ReceiverSnapshot 是从 orders.receiver_snapshot 里读回来的收货信息。
//
// 它与写入时用的那个 receiverSnapshot 是**同一个形状的两个方向**，
// 所以这里直接复用那个类型（下面的 OrderDetail 用的是它的导出别名）——
// 两份定义意味着哪天写进去的字段名改了，读出来的那一份会静默地全是空值。
type ReceiverSnapshot = receiverSnapshot

// ListMine 返回当前买家的一页订单。
//
// page / pageSize 走与 /products 同一个 clampPaging：越界回空页而不是报错，
// page_size 上限 100。共用那个函数而不是抄一份，是因为「分页语义两条接口一致」
// 这件事如果靠两份实现去维持，它们迟早会在某一次改动里分叉，而分叉的症状是
// 某一条接口上 page_size=100000 忽然变成一次全表扫描。
func (s *OrderService) ListMine(ctx context.Context, page, pageSize int,
	f OrderFilter) (OrderList, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return OrderList{}, err
	}
	page, pageSize = clampPaging(page, pageSize)

	out := OrderList{Items: []repository.Order{}, Page: page, PageSize: pageSize}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 计数与取页在同一个事务里 —— 同 ProductService.List：分开两次访问的话，
		// 中间的一次下单会让「total=21 但第二页是空的」偶发出现。
		total, err := tx.CountUserOrders(ctx, id.UserID, repository.OrderFilter(f))
		if err != nil {
			return err
		}
		out.Total = total

		rows, err := tx.ListUserOrders(ctx, id.UserID, repository.OrderFilter(f),
			int64(pageSize), offsetOf(page, pageSize))
		if err != nil {
			return err
		}
		out.Items = rows
		return nil
	})
	if err != nil {
		return OrderList{}, err
	}
	return out, nil
}

// Detail 返回当前买家自己的一笔订单，含收货快照、订单行与支付记录。
//
// 四次读在**同一个事务**里。这里它不只是一致性洁癖：支付记录与订单状态必须
// 来自同一个快照，否则「订单还是 10 待支付、下面却列着一笔成功的支付单」
// 这种自相矛盾的详情页会在支付回调落地的那一瞬间偶发出现 —— 而那正是用户
// 最可能刷新这个页面的时刻。
func (s *OrderService) Detail(ctx context.Context, orderNo string) (OrderDetail, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return OrderDetail{}, err
	}

	var out OrderDetail
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		order, err := tx.FindUserOrderByNo(ctx, orderNo, id.UserID)
		if errors.Is(err, repository.ErrOrderNotFound) {
			return fmt.Errorf("%w: order_no=%s", ErrOrderNotFound, orderNo)
		}
		if err != nil {
			return err
		}
		out.Order = order

		raw, err := tx.FindOrderReceiver(ctx, order.ID)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &out.Receiver); err != nil {
			// 快照解不开是**我们自己写坏了**（这一列由 placeDraft 写，
			// 格式由 receiverSnapshot 定）。报出来，不要回一个空收货人 ——
			// 那会让详情页显示一张寄给「」的单。
			return fmt.Errorf("订单 %s 的收货快照解不开: %w", orderNo, err)
		}

		rawStore, err := tx.FindOrderStoreSnapshot(ctx, order.ID)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(rawStore, &out.Store); err != nil {
			// 与收货快照同一条理由：这一列由 CreateOrderDraft 那条
			// INSERT ... SELECT FROM stores 写，解不开是我们自己写坏了。
			// 报出来，不要回一家没有名字的门店。
			return fmt.Errorf("订单 %s 的门店快照解不开: %w", orderNo, err)
		}

		if out.Items, err = tx.ListOrderItems(ctx, order.ID); err != nil {
			return err
		}
		if out.Payments, err = tx.ListOrderPayments(ctx, order.ID); err != nil {
			return err
		}
		// 售后（00034）：退款单与每一行的在途件数，与订单同一个事务、同一个快照 ——
		// 分两次读的话，「还可退 = 购买 - 已退 - 在途」三个数可能来自两个时刻。
		inflight, err := tx.RefundingQtyByItem(ctx, order.ID)
		if err != nil {
			return err
		}
		for i := range out.Items {
			out.Items[i].RefundingQty = inflight[out.Items[i].ID]
		}
		out.Refunds, err = tx.ListOrderRefunds(ctx, order.ID)
		return err
	})
	if err != nil {
		return OrderDetail{}, err
	}
	return out, nil
}
