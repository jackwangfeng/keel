package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 后台订单与售后的读（契约 GET /admin/orders、GET /admin/orders/{order_no}、
// GET /admin/refunds、GET /admin/refunds/{refund_no}；迁移 00035）。
//
// # 范围
//
// 契约 StaffRole 矩阵新增的「订单与售后」那一行，判据与「门店库存」相同：
// 管理员、操作员全店；大区管理员只看当前挂在他大区下的门店的单；门店管理员
// 只看自己门店的单。列表用 orderListScope 收窄（只返回范围内的），详情用
// authorizeOrderStore 判（范围外 403 out-of-scope，与门店详情、发货、审核一致）。
// 两个函数挨着写在 authz.go 里，判据是同一个 —— 列表里看得见的，详情就放行。
//
// # 为什么每条都在一个事务里
//
// 详情要同时读订单、订单行、在途件数、支付、包裹、退款单；列表要同时数总数与取一页。
// 分开读的话，一次支付回调或退款入账落在两次读之间，详情页就会自相矛盾
// （订单还是 20、下面列着一笔已退款），列表会出现「total = 21 但第二页是空的」。

// ErrAdminListBadRequest：列表的筛选参数自相矛盾（时间下界不早于上界）。契约：422。
var ErrAdminListBadRequest = errors.New("筛选参数不合法")

// AdminOrderSummary 是后台列表里的一笔订单（契约 AdminOrderSummary）：
// 订单本身 + 解开的两份下单时快照。
type AdminOrderSummary struct {
	Order    repository.AdminOrder
	Receiver ReceiverSnapshot
	Store    StoreSnapshot
	// Channel 是渠道单（Source = 1）的来源说明，自营单为 nil。
	Channel *repository.ChannelOrderRef
}

// attachChannelRefs 给这一页的渠道单补上来源说明：有 source = 1 的行时按 channel_order_id 补查一次
// （订单列表不 JOIN 渠道表，第三期计划 Task 1 的执行中修正）；全是自营单时一条语句都不多发。
func attachChannelRefs(ctx context.Context, tx repository.Tx, items []AdminOrderSummary) error {
	var ids []int64
	for _, it := range items {
		if it.Order.Source == 1 && it.Order.ChannelOrderID != nil {
			ids = append(ids, *it.Order.ChannelOrderID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	refs, err := tx.ChannelOrderRefs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range items {
		if id := items[i].Order.ChannelOrderID; items[i].Order.Source == 1 && id != nil {
			if r, ok := refs[*id]; ok {
				items[i].Channel = &r
			}
		}
	}
	return nil
}

// AdminOrderPage 是后台订单列表的一页，带钳制之后的分页参数（同 OrderList 的理由）。
type AdminOrderPage struct {
	Items    []AdminOrderSummary
	Page     int
	PageSize int
	Total    int64
}

// AdminOrderDetail 是后台订单详情（契约 AdminOrderDetail）。
type AdminOrderDetail struct {
	AdminOrderSummary
	// Freight 是下单那一刻的运费明细快照（00056）；那之前的订单为 nil。
	Freight   *FreightBreakdown
	Items     []repository.OrderItem
	Payments  []repository.Payment
	Shipments []repository.Shipment
	Refunds   []AdminRefundView
}

// AdminRefundView 是后台视角的一张退款单（契约 AdminRefund）：退款单 + 解开的门店快照。
type AdminRefundView struct {
	Refund repository.AdminRefund
	Store  StoreSnapshot
}

// AdminRefundPage 是后台退款单列表的一页。
type AdminRefundPage struct {
	Items    []AdminRefundView
	Page     int
	PageSize int
	Total    int64
}

// AdminRefundDetail 是后台退款单详情（契约 AdminRefundDetail）：退款单 + 所属订单摘要。
type AdminRefundDetail struct {
	AdminRefundView
	Order AdminOrderSummary
}

// ListOrders 实现 GET /admin/orders。
func (s *AdminOrderService) ListOrders(ctx context.Context, f repository.AdminOrderFilter,
	page, pageSize int) (AdminOrderPage, error) {

	if err := checkTimeRange(f.CreatedFrom != nil && f.CreatedTo != nil && !f.CreatedFrom.Before(*f.CreatedTo)); err != nil {
		return AdminOrderPage{}, err
	}
	f.OrderNo, f.Phone = trimmedOrNil(f.OrderNo), trimmedOrNil(f.Phone)
	page, pageSize = clampPaging(page, pageSize)
	out := AdminOrderPage{Items: []AdminOrderSummary{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		if out.Total, err = tx.AdminCountOrders(ctx, f, only); err != nil {
			return err
		}
		rows, err := tx.AdminListOrders(ctx, f, only, int64(pageSize), offsetOf(page, pageSize))
		if err != nil {
			return err
		}
		for _, o := range rows {
			sum, err := summarizeOrder(o)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, sum)
		}
		return attachChannelRefs(ctx, tx, out.Items)
	})
	if err != nil {
		return AdminOrderPage{}, err
	}
	return out, nil
}

// OrderDetail 实现 GET /admin/orders/{order_no}。
func (s *AdminOrderService) OrderDetail(ctx context.Context, orderNo string) (AdminOrderDetail, error) {
	var out AdminOrderDetail
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		o, err := tx.AdminFindOrderByNo(ctx, orderNo)
		if errors.Is(err, repository.ErrOrderNotFound) {
			return fmt.Errorf("%w: order_no=%s", ErrOrderNotFound, orderNo)
		}
		if err != nil {
			return err
		}
		if _, err := authorizeOrderStore(ctx, tx, o.StoreID); err != nil {
			return err
		}
		if out.AdminOrderSummary, err = summarizeOrder(o); err != nil {
			return err
		}
		one := []AdminOrderSummary{out.AdminOrderSummary}
		if err := attachChannelRefs(ctx, tx, one); err != nil {
			return err
		}
		out.AdminOrderSummary = one[0]
		if out.Freight, err = loadFreightSnapshot(ctx, tx, o.ID, o.OrderNo); err != nil {
			return err
		}
		if out.Items, err = tx.ListOrderItems(ctx, o.ID); err != nil {
			return err
		}
		inflight, err := tx.RefundingQtyByItem(ctx, o.ID)
		if err != nil {
			return err
		}
		for i := range out.Items {
			out.Items[i].RefundingQty = inflight[out.Items[i].ID]
		}
		if out.Payments, err = tx.ListOrderPayments(ctx, o.ID); err != nil {
			return err
		}
		if out.Shipments, err = tx.ListOrderShipments(ctx, o.ID); err != nil {
			return err
		}
		refunds, err := tx.AdminListOrderRefunds(ctx, o.ID)
		if err != nil {
			return err
		}
		out.Refunds, err = viewRefunds(refunds)
		return err
	})
	if err != nil {
		return AdminOrderDetail{}, err
	}
	return out, nil
}

// ListRefunds 实现 GET /admin/refunds。
func (s *AdminOrderService) ListRefunds(ctx context.Context, f repository.AdminRefundFilter,
	page, pageSize int) (AdminRefundPage, error) {

	if err := checkTimeRange(f.CreatedFrom != nil && f.CreatedTo != nil && !f.CreatedFrom.Before(*f.CreatedTo)); err != nil {
		return AdminRefundPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	out := AdminRefundPage{Items: []AdminRefundView{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		if out.Total, err = tx.AdminCountRefunds(ctx, f, only); err != nil {
			return err
		}
		rows, err := tx.AdminListRefunds(ctx, f, only, int64(pageSize), offsetOf(page, pageSize))
		if err != nil {
			return err
		}
		out.Items, err = viewRefunds(rows)
		return err
	})
	if err != nil {
		return AdminRefundPage{}, err
	}
	return out, nil
}

// RefundDetail 实现 GET /admin/refunds/{refund_no}。
func (s *AdminOrderService) RefundDetail(ctx context.Context, refundNo string) (AdminRefundDetail, error) {
	var out AdminRefundDetail
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		r, err := tx.AdminFindRefundByNo(ctx, refundNo)
		if errors.Is(err, repository.ErrRefundNotFound) {
			return fmt.Errorf("%w: refund_no=%s", ErrRefundNotFound, refundNo)
		}
		if err != nil {
			return err
		}
		if _, err := authorizeOrderStore(ctx, tx, r.StoreID); err != nil {
			return err
		}
		views, err := viewRefunds([]repository.AdminRefund{r})
		if err != nil {
			return err
		}
		out.AdminRefundView = views[0]
		o, err := tx.AdminFindOrderByNo(ctx, r.OrderNo)
		if err != nil {
			// 退款单挂着的订单查不到：外键保证它存在，这里查不到只可能是它还是草稿
			// （而草稿不可能有退款单）。按内部错误报，不要伪装成 404。
			return fmt.Errorf("退款单 %s 所属的订单 %s 读不出来: %w", refundNo, r.OrderNo, err)
		}
		if out.Order, err = summarizeOrder(o); err != nil {
			return err
		}
		one := []AdminOrderSummary{out.Order}
		if err := attachChannelRefs(ctx, tx, one); err != nil {
			return err
		}
		out.Order = one[0]
		return nil
	})
	if err != nil {
		return AdminRefundDetail{}, err
	}
	return out, nil
}

// summarizeOrder 解开订单上的两份快照。解不开是我们自己写坏了（这两列由下单那条
// INSERT 写），报出来，不要回一张寄给「」的单 —— 与买家详情同一个处置。
func summarizeOrder(o repository.AdminOrder) (AdminOrderSummary, error) {
	out := AdminOrderSummary{Order: o}
	if err := json.Unmarshal(o.ReceiverSnapshot, &out.Receiver); err != nil {
		return AdminOrderSummary{}, fmt.Errorf("订单 %s 的收货快照解不开: %w", o.OrderNo, err)
	}
	if err := json.Unmarshal(o.StoreSnapshot, &out.Store); err != nil {
		return AdminOrderSummary{}, fmt.Errorf("订单 %s 的门店快照解不开: %w", o.OrderNo, err)
	}
	return out, nil
}

func viewRefunds(rows []repository.AdminRefund) ([]AdminRefundView, error) {
	out := make([]AdminRefundView, 0, len(rows))
	for _, r := range rows {
		v := AdminRefundView{Refund: r}
		if err := json.Unmarshal(r.StoreSnapshot, &v.Store); err != nil {
			return nil, fmt.Errorf("退款单 %s 所属订单的门店快照解不开: %w", r.RefundNo, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// checkTimeRange 在下界不早于上界时报 ErrAdminListBadRequest。
//
// 不当成「没传」：一个写反了的时间范围回一页不带筛选的全量结果，
// 会让人以为那段时间就这么多单（契约的 422 那一条）。
// ShopDayRange 把后台列表「按天选」的一对日期（契约 created_date_from / created_date_to，两端都含）
// 按**店铺时区**换成半开区间 [起始日零点, 结束日次日零点)，与经营报表同一个切天口径
// （reportLocation：时区名不合法时回落 Asia/Shanghai）。
//
// 日期只取年月日（handler 按 YYYY-MM-DD 解析，时刻是 UTC 零点，这里不看它）。
// 两个都没传时不读店铺设置，返回两个 nil。
func (s *AdminOrderService) ShopDayRange(ctx context.Context, fromDay, toDay *time.Time) (*time.Time, *time.Time, error) {
	if fromDay == nil && toDay == nil {
		return nil, nil, nil
	}
	name, err := s.repo.ShopTimezone(ctx)
	if err != nil {
		return nil, nil, err
	}
	_, loc := reportLocation(name)
	at := func(d *time.Time, plusDays int) *time.Time {
		if d == nil {
			return nil
		}
		t := time.Date(d.Year(), d.Month(), d.Day()+plusDays, 0, 0, 0, 0, loc)
		return &t
	}
	return at(fromDay, 0), at(toDay, 1), nil
}

func checkTimeRange(inverted bool) error {
	if inverted {
		return fmt.Errorf("%w: created_from 必须早于 created_to（区间是半开的 [from, to)）", ErrAdminListBadRequest)
	}
	return nil
}

// trimmedOrNil 去掉首尾空白；剩下空串就当没传（nil = 不筛）。
// 从客服那里复制过来的单号、手机号常带着空格，而精确匹配对一个空格也不宽容。
func trimmedOrNil(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}
