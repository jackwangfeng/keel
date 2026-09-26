package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 买家侧读接口（GET /orders 与 GET /orders/{order_no}）在 repository 边界上的那一面。
//
// 单独一个文件，惯例同 order.go 的文件头：Tx 那份组合定义被好几条并发任务碰到，
// 平铺加方法等于所有人改同一批行。
//
// # 这一面与写路径那一面的唯一实质差别：user_id
//
// 下单那一面的每条查询都跑在 SAGA 分支或渠道回调里，那里**没有买家身份**
// （协调器重放时进程对原请求毫无记忆）。读这一面全部跑在一次带 bearer 的请求里，
// 所以它们每一条都按 user_id 过滤。
//
// 那个过滤是**越权过滤，不是租户过滤**。RLS 只回答「这一行属于本店吗」，
// 它回答不了「这一行属于这个买家吗」—— 策略里只有 current_merchant()。
// 去掉 user_id 之后跨租户依然安全，而同一家店的任意买家可以翻遍全店的订单，
// 并且这类失效**不报错、不变慢**，只是响应里多了几笔不属于他的单。
// 守它的是 internal/handler/order_query_test.go 里那两条用同一租户下两个真实买家
// 写的测试。

// OrderFilter 是「我的订单」上那两个正交的筛选条件（契约的 status / refund_status）。
//
// 两个都是可空指针而不是「0 表示不筛」：refund_status = 0 是一个**合法且常用**
// 的筛选值（「没有任何售后的订单」），把它和「不筛」压进同一个零值，
// 「我的订单-正常」这个入口就再也做不出来了。契约的注释里专门为这件事叮嘱过
// 一句（OrderRefundStatus 上「这里不能放 default」那段）。
type OrderFilter struct {
	Status       *int16
	RefundStatus *int16
}

// OrderItem 是订单详情里的一行（下单时的快照）。
//
// 与 NewOrderItem 分开：那个描述「要写进去什么」，这个描述「读出来是什么」。
// 两者今天的列几乎一样，但读这一侧多一个 ID 与 RefundedQty，而写那一侧多一个
// OrderID —— 合成一个结构体之后，每一边都得对另一边的字段视而不见。
type OrderItem struct {
	ID            int64
	SKUID         int64
	ProductID     int64
	TitleSnapshot string
	SpecSnapshot  []byte
	ImageSnapshot *string
	PriceCents    int64
	Quantity      int32
	AmountCents   int64
	DiscountCents int64
	RefundedQty   int32
}

// Payment 是订单详情里的一笔支付记录（契约的 PaymentRecord）。
type Payment struct {
	PaymentNo   string
	Channel     int16
	AmountCents int64
	Status      int16
	PaidAt      *time.Time
}

// OrderQueryTx 是买家侧读这一面。
type OrderQueryTx interface {
	// ListUserOrders 取这个买家的一页订单（不含 status = 0 创建中）。
	ListUserOrders(ctx context.Context, userID int64, f OrderFilter,
		limit, offset int64) ([]Order, error)

	// CountUserOrders 在**同一套谓词**下数总条数，用于填契约里必填的 total。
	CountUserOrders(ctx context.Context, userID int64, f OrderFilter) (int64, error)

	// FindUserOrderByNo 按单号取这个买家自己的订单。
	// 查不到、或者这一单不是他的，都返回 ErrOrderNotFound。
	FindUserOrderByNo(ctx context.Context, orderNo string, userID int64) (Order, error)

	// ListOrderItems 取一笔订单的全部行（展示用的完整快照）。
	ListOrderItems(ctx context.Context, orderID int64) ([]OrderItem, error)

	// FindOrderReceiver 取下单时拍下的收货信息快照（JSONB 原样的字节）。
	FindOrderReceiver(ctx context.Context, orderID int64) ([]byte, error)

	// FindOrderStoreSnapshot 取下单时拍下的门店 / 大区展示快照。
	//
	// **读快照，不 JOIN stores**：门店会改名、会搬家、会被调到另一个大区，
	// 而三个月前那一单的详情页要显示当时那个名字（数据模型 §5）。
	// JOIN 出来的是今天的名字，而那正是快照存在要避免的东西。
	FindOrderStoreSnapshot(ctx context.Context, orderID int64) ([]byte, error)

	// ListOrderPayments 取这一单的全部支付尝试。
	ListOrderPayments(ctx context.Context, orderID int64) ([]Payment, error)
}

func (t tenantTx) ListUserOrders(ctx context.Context, userID int64, f OrderFilter,
	limit, offset int64) ([]Order, error) {
	// 到这里还越界只可能是上游的钳制没生效。理由与 ListProducts 那处一字不差：
	// 截断会把「第 1 亿页」悄悄变成某一页真实数据。
	if limit < 0 || limit > math.MaxInt32 {
		return nil, fmt.Errorf("limit %d 超出范围 [0, %d]", limit, math.MaxInt32)
	}
	if offset < 0 || offset > math.MaxInt32 {
		return nil, fmt.Errorf("offset %d 超出范围 [0, %d]", offset, math.MaxInt32)
	}

	rows, err := t.q.ListUserOrders(ctx, db.ListUserOrdersParams{
		UserID:       userID,
		Status:       f.Status,
		RefundStatus: f.RefundStatus,
		PageLimit:    int32(limit),
		PageOffset:   int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Order, 0, len(rows))
	for _, r := range rows {
		out = append(out, Order{
			ID:               r.ID,
			OrderNo:          r.OrderNo,
			UserID:           r.UserID,
			StoreID:          r.StoreID,
			RegionID:         r.RegionID,
			Status:           r.Status,
			GoodsAmountCents: r.GoodsAmountCents,
			FreightCents:     r.FreightCents,
			DiscountCents:    r.DiscountCents,
			PayableCents:     r.PayableCents,
			PaidCents:        r.PaidCents,
			RefundedCents:    r.RefundedCents,
			RefundStatus:     r.RefundStatus,
			ExpireAt:         r.ExpireAt.Time,
			CreatedAt:        r.CreatedAt.Time,
			PaidAt:           optTime(r.PaidAt),
			ShippedAt:        optTime(r.ShippedAt),
			FinishedAt:       optTime(r.FinishedAt),
			UserCouponID:     r.UserCouponID,
			CouponName:       r.CouponName,
		})
	}
	return out, nil
}

func (t tenantTx) CountUserOrders(ctx context.Context, userID int64, f OrderFilter) (int64, error) {
	return t.q.CountUserOrders(ctx, db.CountUserOrdersParams{
		UserID:       userID,
		Status:       f.Status,
		RefundStatus: f.RefundStatus,
	})
}

func (t tenantTx) FindUserOrderByNo(ctx context.Context, orderNo string, userID int64) (Order, error) {
	r, err := t.q.GetUserOrderByNo(ctx, db.GetUserOrderByNoParams{OrderNo: orderNo, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		// 刻意不区分「没有这一单」与「这一单是别人的」，理由写在查询上：
		// 分开报会把这个接口变成一个单号存在性判定器。
		return Order{}, fmt.Errorf("order %s: %w", orderNo, ErrOrderNotFound)
	}
	if err != nil {
		return Order{}, err
	}
	return Order{
		ID:               r.ID,
		OrderNo:          r.OrderNo,
		UserID:           r.UserID,
		StoreID:          r.StoreID,
		RegionID:         r.RegionID,
		Status:           r.Status,
		GoodsAmountCents: r.GoodsAmountCents,
		FreightCents:     r.FreightCents,
		DiscountCents:    r.DiscountCents,
		PayableCents:     r.PayableCents,
		PaidCents:        r.PaidCents,
		RefundedCents:    r.RefundedCents,
		RefundStatus:     r.RefundStatus,
		ExpireAt:         r.ExpireAt.Time,
		CreatedAt:        r.CreatedAt.Time,
		PaidAt:           optTime(r.PaidAt),
		ShippedAt:        optTime(r.ShippedAt),
		FinishedAt:       optTime(r.FinishedAt),
		UserCouponID:     r.UserCouponID,
		CouponName:       r.CouponName,
	}, nil
}

func (t tenantTx) ListOrderItems(ctx context.Context, orderID int64) ([]OrderItem, error) {
	rows, err := t.q.ListOrderItemsForDetail(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]OrderItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, OrderItem{
			ID:            r.ID,
			SKUID:         r.SkuID,
			ProductID:     r.ProductID,
			TitleSnapshot: r.TitleSnapshot,
			SpecSnapshot:  r.SpecSnapshot,
			ImageSnapshot: r.ImageSnapshot,
			PriceCents:    r.PriceCents,
			Quantity:      r.Quantity,
			AmountCents:   r.AmountCents,
			DiscountCents: r.DiscountCents,
			RefundedQty:   r.RefundedQty,
		})
	}
	return out, nil
}

func (t tenantTx) FindOrderReceiver(ctx context.Context, orderID int64) ([]byte, error) {
	raw, err := t.q.GetOrderReceiver(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("order %d: %w", orderID, ErrOrderNotFound)
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (t tenantTx) FindOrderStoreSnapshot(ctx context.Context, orderID int64) ([]byte, error) {
	raw, err := t.q.GetOrderStoreSnapshot(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("order %d: %w", orderID, ErrOrderNotFound)
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (t tenantTx) ListOrderPayments(ctx context.Context, orderID int64) ([]Payment, error) {
	rows, err := t.q.ListPaymentsForOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]Payment, 0, len(rows))
	for _, r := range rows {
		out = append(out, Payment{
			PaymentNo:   r.PaymentNo,
			Channel:     r.Channel,
			AmountCents: r.AmountCents,
			Status:      r.Status,
			PaidAt:      optTime(r.PaidAt),
		})
	}
	return out, nil
}
