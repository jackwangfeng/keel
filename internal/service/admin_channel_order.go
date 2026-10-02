package service

// 后台的渠道订单（/admin/channel-orders*，第三期 Task 7）：ChannelService 外面那一层权限。
//
// 读与后台订单同一个范围判据：全店范围（管理员、操作员）看全部；大区 / 门店管理员只看自己范围内门店的渠道单
// （列表在 SQL 里按 orderListScope 过滤，详情越界回 404、不泄露存在性）。没映射到门店的渠道单只有全店范围看得见。
// 重试 / 接单 / 拒单 / 申请决定是「订单处理」：与发货同一个门店范围判据（authorizeOrderStore，按渠道单映射到的
// keel 门店）；渠道单没映射到门店时没有门店可判，要全店范围。动作之后的回包按同一个读判据，动作做得了的就读得到。
// 渠道账号、凭据、规则、映射仍只限全店范围（admin_channel.go）。

import (
	"context"
	"errors"
	"fmt"

	"github.com/keel/keel/internal/repository"
)

// ChannelOrderView 是后台看到的一张渠道单。金额 / 行 / 收货人是 JSONB 原样（形状见 channel_order.go 的
// channelOrderAmounts 等，与契约 ChannelOrderAmounts 等逐字段一致），由 handler 解开。Requests 只有详情才填。
type ChannelOrderView struct {
	Order    repository.ChannelOrder
	Requests []repository.ChannelOrderRequest
	// Channel / BindingName：所属账号的渠道种类与名称（门店范围的员工读不了账号列表，回包里直接带上）。
	Channel, BindingName string
}

// attachChannelOrderRefs 给一组渠道单补上所属账号的渠道种类与名称（一次查询）。
func attachChannelOrderRefs(ctx context.Context, tx repository.Tx, views []ChannelOrderView) error {
	if len(views) == 0 {
		return nil
	}
	ids := make([]int64, len(views))
	for i, v := range views {
		ids[i] = v.Order.ID
	}
	refs, err := tx.ChannelOrderRefs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range views {
		r := refs[views[i].Order.ID]
		views[i].Channel, views[i].BindingName = r.Channel, r.BindingName
	}
	return nil
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
	only, err := orderListScope(ctx)
	if err != nil {
		return ChannelOrderPage{}, err
	}
	f.Only = only
	page, pageSize = clampPaging(page, pageSize)
	f.Limit, f.Offset = int32(pageSize), int32(offsetOf(page, pageSize))
	out := ChannelOrderPage{Items: []ChannelOrderView{}, Page: page, PageSize: pageSize}
	err = s.ch.repo.WithTenant(ctx, func(tx repository.Tx) error {
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
		return attachChannelOrderRefs(ctx, tx, out.Items)
	})
	return out, err
}

// GetChannelOrder 是后台渠道单详情（含平台申请，新的在前）。不在调用者范围内（含没映射门店、调用者不是全店范围）
// 的回 404（ErrChannelNotFound），与不存在一样，不泄露存在性。
func (s *AdminChannelService) GetChannelOrder(ctx context.Context, id int64) (ChannelOrderView, error) {
	staff, err := requireStaff(ctx)
	if err != nil {
		return ChannelOrderView{}, err
	}
	var v ChannelOrderView
	err = s.ch.repo.WithTenant(ctx, func(tx repository.Tx) error {
		co, err := tx.GetChannelOrder(ctx, id)
		if err != nil {
			return err
		}
		if !staff.MerchantWide() {
			if co.StoreID == nil {
				return fmt.Errorf("渠道单 %d 没映射门店，只有全店范围看得见: %w", id, repository.ErrChannelNotFound)
			}
			if _, err := authorizeOrderStore(ctx, tx, *co.StoreID); errors.Is(err, ErrOutOfScope) {
				return fmt.Errorf("渠道单 %d: %w", id, repository.ErrChannelNotFound)
			} else if err != nil {
				return err
			}
		}
		v = channelOrderView(co)
		one := []ChannelOrderView{v}
		if err := attachChannelOrderRefs(ctx, tx, one); err != nil {
			return err
		}
		v = one[0]
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
