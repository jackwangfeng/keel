package service

// 后台的渠道订单（/admin/channel-orders*，第三期 Task 7）：ChannelService 外面那一层权限。
//
// 读与渠道页同一个判据（全店范围：管理员、操作员）。重试 / 接单 / 拒单 / 申请决定是「订单处理」：
// 与发货同一个门店范围判据（authorizeOrderStore，按渠道单映射到的 keel 门店）；渠道单没映射到门店时
// 没有门店可判，要全店范围。

import (
	"context"

	"github.com/keel/keel/internal/repository"
)

// ChannelOrderView 是后台看到的一张渠道单。金额 / 行 / 收货人是 JSONB 原样（形状见 channel_order.go 的
// channelOrderAmounts 等，与契约 ChannelOrderAmounts 等逐字段一致），由 handler 解开。Requests 只有详情才填。
type ChannelOrderView struct {
	Order    repository.ChannelOrder
	Requests []repository.ChannelOrderRequest
}

func channelOrderView(co repository.ChannelOrder) ChannelOrderView {
	return ChannelOrderView{Order: co}
}

// ChannelOrderPage 是后台渠道单列表的一页。
type ChannelOrderPage struct {
	Items    []ChannelOrderView
	Page     int
	PageSize int
	Total    int64
}

// ListChannelOrders 是后台渠道单列表。f.BindingID 非空时 binding 要存在（404）。
func (s *AdminChannelService) ListChannelOrders(ctx context.Context, f repository.ChannelOrderFilter, page, pageSize int) (ChannelOrderPage, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return ChannelOrderPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	f.Limit, f.Offset = int32(pageSize), int32(offsetOf(page, pageSize))
	out := ChannelOrderPage{Items: []ChannelOrderView{}, Page: page, PageSize: pageSize}
	err := s.ch.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if f.BindingID != nil {
			if _, err := tx.GetChannelBinding(ctx, *f.BindingID); err != nil {
				return err
			}
		}
		var err error
		if out.Total, err = tx.CountChannelOrders(ctx, f); err != nil {
			return err
		}
		rows, err := tx.ListChannelOrders(ctx, f)
		if err != nil {
			return err
		}
		for _, co := range rows {
			out.Items = append(out.Items, channelOrderView(co))
		}
		return nil
	})
	return out, err
}

// GetChannelOrder 是后台渠道单详情（含平台申请，新的在前）。
func (s *AdminChannelService) GetChannelOrder(ctx context.Context, id int64) (ChannelOrderView, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return ChannelOrderView{}, err
	}
	var v ChannelOrderView
	err := s.ch.repo.WithTenant(ctx, func(tx repository.Tx) error {
		co, err := tx.GetChannelOrder(ctx, id)
		if err != nil {
			return err
		}
		v = channelOrderView(co)
		v.Requests, err = tx.ListChannelOrderRequests(ctx, id)
		return err
	})
	if v.Requests == nil {
		v.Requests = []repository.ChannelOrderRequest{}
	}
	return v, err
}

// authorizeChannelOrder 判调用者能不能处理这张渠道单（门店范围同发货）。
func (s *AdminChannelService) authorizeChannelOrder(ctx context.Context, tx repository.Tx, co repository.ChannelOrder) error {
	if co.StoreID == nil {
		_, err := requireMerchantWide(ctx)
		return err
	}
	_, err := authorizeOrderStore(ctx, tx, *co.StoreID)
	return err
}

func (s *AdminChannelService) authorizeChannelOrderID(ctx context.Context, id int64) error {
	if _, err := requireStaff(ctx); err != nil {
		return err
	}
	return s.ch.repo.WithTenant(ctx, func(tx repository.Tx) error {
		co, err := tx.GetChannelOrder(ctx, id)
		if err != nil {
			return err
		}
		return s.authorizeChannelOrder(ctx, tx, co)
	})
}

// RetryChannelOrder 是「重试」（ErrChannelOrderNotRetryable → 409）。返回处理之后的渠道单。
func (s *AdminChannelService) RetryChannelOrder(ctx context.Context, id int64) (ChannelOrderView, error) {
	if err := s.authorizeChannelOrderID(ctx, id); err != nil {
		return ChannelOrderView{}, err
	}
	if err := s.ch.RetryChannelOrder(ctx, id); err != nil {
		return ChannelOrderView{}, err
	}
	return s.GetChannelOrder(ctx, id)
}

// AcceptChannelOrder 是「接单」（ErrChannelOrderNotAcceptable → 409，ErrChannelOrderAcceptFailed → 422）。
func (s *AdminChannelService) AcceptChannelOrder(ctx context.Context, id int64) (ChannelOrderView, error) {
	if err := s.authorizeChannelOrderID(ctx, id); err != nil {
		return ChannelOrderView{}, err
	}
	if err := s.ch.AcceptChannelOrder(ctx, id); err != nil {
		return ChannelOrderView{}, err
	}
	return s.GetChannelOrder(ctx, id)
}

// RejectChannelOrder 是「拒单」（ErrChannelOrderNotAcceptable → 409）。
func (s *AdminChannelService) RejectChannelOrder(ctx context.Context, id int64, reason string) (ChannelOrderView, error) {
	if err := s.authorizeChannelOrderID(ctx, id); err != nil {
		return ChannelOrderView{}, err
	}
	if err := s.ch.RejectChannelOrder(ctx, id, reason); err != nil {
		return ChannelOrderView{}, err
	}
	return s.GetChannelOrder(ctx, id)
}

// DecideChannelOrderRequest 是对平台申请的同意 / 拒绝（ErrChannelRequestDecided → 409）。返回申请所在的渠道单。
func (s *AdminChannelService) DecideChannelOrderRequest(ctx context.Context, requestID int64, agree bool) (ChannelOrderView, error) {
	staff, err := requireStaff(ctx)
	if err != nil {
		return ChannelOrderView{}, err
	}
	var channelOrderID int64
	if err := s.ch.repo.WithTenant(ctx, func(tx repository.Tx) error {
		req, err := tx.LockChannelOrderRequest(ctx, requestID)
		if err != nil {
			return err
		}
		channelOrderID = req.ChannelOrderID
		co, err := tx.GetChannelOrder(ctx, req.ChannelOrderID)
		if err != nil {
			return err
		}
		return s.authorizeChannelOrder(ctx, tx, co)
	}); err != nil {
		return ChannelOrderView{}, err
	}
	if err := s.ch.DecideRequest(ctx, requestID, agree, staff.StaffID); err != nil {
		return ChannelOrderView{}, err
	}
	return s.GetChannelOrder(ctx, channelOrderID)
}
