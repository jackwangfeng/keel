package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 退款与售后（数据模型 §11）在 repository 边界上的那一面。

// refundTransitionConstraint 是 00034 那个退款状态机触发器 RAISE 时带的约束名。
// 与 orderTransitionConstraint 同一个理由：23514 也是每一条 CHECK 的错误码。
const refundTransitionConstraint = "refund_status_transition"

// channelRefundConstraint 是退款回调幂等的最后一道锁（00034）。
// 与 channelTxnConstraint 完全同构：索引名是接口的一部分。
const channelRefundConstraint = "uk_refunds_channel_txn"

var (
	// ErrRefundNotFound：这张退款单在本租户查不到（买家侧：或者不是他的）。
	ErrRefundNotFound = errors.New("退款单不存在")

	// ErrNoSettledPayment：订单是已支付状态，却找不到一笔成功的支付。
	// 不变量被破坏（SettleOrder 与支付单落库在同一个事务里），不是业务分支。
	ErrNoSettledPayment = errors.New("订单没有成功的支付记录")

	// ErrIllegalRefundTransition：退款状态机触发器拒绝了这次跳转。
	ErrIllegalRefundTransition = errors.New("退款状态机不允许这次跳转")

	// ErrDuplicateChannelRefund：这个渠道退款流水号在本租户已经入过账。
	// **正常路径**：渠道重复推送是常态，契约明写「重复推送时直接返回 200」。
	ErrDuplicateChannelRefund = errors.New("渠道退款流水号重复")

	// ErrRefundOverflow：入账回写订单项时条件更新影响 0 行 —— 超退。
	// §11：「rows_affected = 0 ⇒ 超退，整个退款事务回滚」。
	ErrRefundOverflow = errors.New("退款回写超出了订单项的可退范围")
)

// 退款单状态（§11，与契约 RefundStatus 逐值一致）。常量而不是散落的字面量：
// 这几个数字决定一笔钱算「在途」还是「已退」。
const (
	RefundPending        int16 = 10 // 待审核
	RefundAwaitingReturn int16 = 20 // 待买家退货
	RefundProcessing     int16 = 30 // 退款中
	RefundSucceeded      int16 = 40 // 已退款
	RefundRejected       int16 = 50 // 已拒绝
	RefundCanceled       int16 = 60 // 已取消
)

// 退款类型（§11 refund_type）。
const (
	RefundTypeMoneyOnly   int16 = 1 // 仅退款
	RefundTypeReturnGoods int16 = 2 // 退货退款
)

// Refund 是一张退款单（契约的 Refund）。
type Refund struct {
	ID       int64
	RefundNo string
	OrderID  int64
	OrderNo  string
	// StoreID 是这一单的履约门店 —— 后台判权按它（与发货同一个判据）。
	StoreID          int64
	PaymentNo        string
	UserID           int64
	RefundType       int16
	ReasonCode       int16
	ReasonText       *string
	EvidenceURLs     []string
	GoodsAmountCents int64
	FreightCents     int64
	AmountCents      int64
	Status           int16
	Channel          int16
	ChannelRefundID  *string
	RejectReason     *string
	AuditedAt        *time.Time
	RefundedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Items            []RefundItem

	// ReturnShipment 是买家填的寄回物流（00037）。只有退货退款、且买家填过时非 nil。
	ReturnShipment *ReturnShipment

	// ReturnDeadlineAt 是寄回截止时间（契约 Refund.return_deadline_at，00059）：
	// 退货退款停在 20、还没填寄回物流时 = audited_at + 店铺设置的 return_ship_days 天，
	// 过了它定时任务把这张单关到 60（service/return_timeout.go）。其余为 nil。
	ReturnDeadlineAt *time.Time
}

// AwaitingReturn 判这张退款单是不是「等买家寄回、还没寄」：退货退款、停在 20、
// 没填寄回物流。退货超时关闭与寄回截止时间都只对这种单成立。
func (r Refund) AwaitingReturn() bool {
	return r.RefundType == RefundTypeReturnGoods && r.Status == RefundAwaitingReturn &&
		r.ReturnShipment == nil && r.AuditedAt != nil
}

// ReturnShipment 是退货寄回的物流（契约的 ReturnShipment）。
type ReturnShipment struct {
	CarrierCode string
	TrackingNo  string
	// SubmittedAt 是买家最近一次填写的时间，不是揽收时间。
	SubmittedAt time.Time
}

// RefundItem 是退款单的一行明细。
type RefundItem struct {
	OrderItemID int64
	SKUID       int64
	Quantity    int32
	AmountCents int64
	Title       string
	ImageURL    *string
}

// RefundableItem 是算退款金额要的一行订单项。
type RefundableItem struct {
	ID            int64
	SKUID         int64
	Quantity      int32
	AmountCents   int64
	DiscountCents int64
	RefundedQty   int32
	RefundedCents int64
}

// SettledPayment 是原路退回的落点。
type SettledPayment struct {
	ID        int64
	PaymentNo string
	Channel   int16
}

// NewRefund 是落一张退款单要写的列。没有 MerchantID（00034 的 DEFAULT）。
type NewRefund struct {
	RefundNo         string
	OrderID          int64
	PaymentID        int64
	UserID           int64
	RefundType       int16
	ReasonCode       int16
	ReasonText       *string
	EvidenceURLs     []string
	GoodsAmountCents int64
	FreightCents     int64
	Channel          int16
	Items            []NewRefundItem
}

// NewRefundItem 是一行退款明细。
type NewRefundItem struct {
	OrderItemID int64
	Quantity    int32
	AmountCents int64
}

// RefundTx 是退款这一面。
//
// 状态推进一律返回 (bool, error)：false 表示「已经不在预期的起点状态上」，
// 调用方据此回契约里的 409。
type RefundTx interface {
	LockUserOrderByNo(ctx context.Context, orderNo string, userID int64) (Order, error)
	LockOrderByID(ctx context.Context, orderID int64) (Order, error)
	ListRefundableItems(ctx context.Context, orderID int64) ([]RefundableItem, error)
	// RefundingQtyByItem 每一行订单项的在途件数（没有在途的行不在 map 里）。
	RefundingQtyByItem(ctx context.Context, orderID int64) (map[int64]int32, error)
	FindSettledPayment(ctx context.Context, orderID int64) (SettledPayment, error)
	InsertRefund(ctx context.Context, r NewRefund) (int64, error)
	InsertChannelRefund(ctx context.Context, r NewChannelRefund) (int64, error)
	ChannelRefundExists(ctx context.Context, channelRefundID string) (bool, error)
	// AddChannelRefundedCents 累加渠道退款的已退金额并同时对齐 refund_status（理由见 SQL 的注释）。
	AddChannelRefundedCents(ctx context.Context, orderID, amount int64) error
	StartWholeOrderRefund(ctx context.Context, orderID int64) (bool, error)
	RevertWholeOrderRefund(ctx context.Context, orderID int64) (bool, error)
	FinishWholeOrderRefund(ctx context.Context, orderID int64) (bool, error)
	RecomputeOrderRefundStatus(ctx context.Context, orderID int64) error

	FindRefundByNo(ctx context.Context, refundNo string) (Refund, error)
	FindUserRefundByNo(ctx context.Context, refundNo string, userID int64) (Refund, error)
	// LockRefundStatus 在订单行锁之下锁住退款单，返回它此刻的状态。
	LockRefundStatus(ctx context.Context, refundID int64) (int16, error)
	ListOrderRefunds(ctx context.Context, orderID int64) ([]Refund, error)
	ListUserRefunds(ctx context.Context, userID int64, status *int16, limit, offset int64) ([]Refund, error)
	CountUserRefunds(ctx context.Context, userID int64, status *int16) (int64, error)

	CancelRefund(ctx context.Context, refundID, userID int64) (bool, error)
	// ListReturnOverdueRefunds 退货超时未寄回的候选：退货退款、停在 20、没填寄回物流、
	// 审核时间早于 cutoff，按审核时间从早到晚，至多 limit 张（00059 / 00060）。只是预筛。
	ListReturnOverdueRefunds(ctx context.Context, cutoff time.Time, limit int32) ([]ReturnOverdueRefund, error)
	// ExpireReturnRefund 20 → 60（退货超时未寄回）。谓词重判「没填寄回物流、审核早于 cutoff」，
	// 不再满足时返回 false —— 调用方要先锁订单、再锁退款单。
	ExpireReturnRefund(ctx context.Context, refundID int64, cutoff time.Time) (bool, error)
	// 审核与确认收到退货三条都记下是谁做的（staffID → audited_by / received_by，00035）。
	RejectRefund(ctx context.Context, refundID int64, reason string, staffID int64) (bool, error)
	ApproveRefund(ctx context.Context, refundID int64, next int16, freightCents int64, staffID int64) (bool, error)
	ReceiveRefundGoods(ctx context.Context, refundID int64, staffID int64) (bool, error)
	// SubmitReturnShipment 买家填寄回物流，状态不变（00037）。false 表示这张单不是
	// 退货退款、不在 20，或者不是这个买家的。
	SubmitReturnShipment(ctx context.Context, refundID, userID int64, carrierCode, trackingNo string) (bool, error)
	// CompleteRefund 30 → 40。流水号重复返回 ErrDuplicateChannelRefund。
	CompleteRefund(ctx context.Context, refundID int64, channelRefundID string, payload []byte, at time.Time) (bool, error)
	RecordRefundNotify(ctx context.Context, refundID int64, payload []byte) error

	// WriteBackOrderItemRefund 回写一行；超退返回 ErrRefundOverflow。
	WriteBackOrderItemRefund(ctx context.Context, orderItemID int64, qty int32, amount int64) error
	AddOrderRefundedCents(ctx context.Context, orderID, amount int64) error
	CountOrderItemsNotFullyRefunded(ctx context.Context, orderID int64) (int64, error)
	ReturnCouponForOrder(ctx context.Context, orderID int64) (bool, error)
	OtherRefundFreight(ctx context.Context, orderID, refundID int64) (int64, error)
}

func orderFromRow(r db.GetOrderByNoRow) Order {
	return Order{
		ID:                     r.ID,
		OrderNo:                r.OrderNo,
		UserID:                 r.UserID,
		StoreID:                r.StoreID,
		RegionID:               r.RegionID,
		Status:                 r.Status,
		GoodsAmountCents:       r.GoodsAmountCents,
		FreightCents:           r.FreightCents,
		FreightDiscountCents:   r.FreightDiscountCents,
		DiscountCents:          r.DiscountCents,
		PayableCents:           r.PayableCents,
		PaidCents:              r.PaidCents,
		RefundedCents:          r.RefundedCents,
		RefundStatus:           r.RefundStatus,
		ExpireAt:               r.ExpireAt.Time,
		CreatedAt:              r.CreatedAt.Time,
		PaidAt:                 optTime(r.PaidAt),
		ShippedAt:              optTime(r.ShippedAt),
		FinishedAt:             optTime(r.FinishedAt),
		UserCouponID:           r.UserCouponID,
		CouponName:             r.CouponName,
		PromotionDiscountCents: r.PromotionDiscountCents,
		Promotions:             r.Promotions,
		Source:                 r.Source,
		ChannelOrderID:         r.ChannelOrderID,
	}
}

func (t tenantTx) LockUserOrderByNo(ctx context.Context, orderNo string, userID int64) (Order, error) {
	r, err := t.q.LockUserOrderByNo(ctx, db.LockUserOrderByNoParams{OrderNo: orderNo, UserID: &userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("order %s: %w", orderNo, ErrOrderNotFound)
	}
	if err != nil {
		return Order{}, err
	}
	// 两条查询选的是同一组列、同一个顺序，所以行类型可以直接转换 ——
	// 哪天其中一条多选了一列，这一行编译不过，而不是悄悄漏掉那一列。
	return orderFromRow(db.GetOrderByNoRow(r)), nil
}

func (t tenantTx) LockOrderByID(ctx context.Context, orderID int64) (Order, error) {
	r, err := t.q.LockOrderByID(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("order id %d: %w", orderID, ErrOrderNotFound)
	}
	if err != nil {
		return Order{}, err
	}
	return orderFromRow(db.GetOrderByNoRow(r)), nil
}

func (t tenantTx) ListRefundableItems(ctx context.Context, orderID int64) ([]RefundableItem, error) {
	rows, err := t.q.ListOrderItemsForRefund(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]RefundableItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, RefundableItem{
			ID: r.ID, SKUID: r.SkuID, Quantity: r.Quantity, AmountCents: r.AmountCents,
			DiscountCents: r.DiscountCents, RefundedQty: r.RefundedQty, RefundedCents: r.RefundedCents,
		})
	}
	return out, nil
}

func (t tenantTx) RefundingQtyByItem(ctx context.Context, orderID int64) (map[int64]int32, error) {
	rows, err := t.q.RefundingQtyByItem(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int32, len(rows))
	for _, r := range rows {
		out[r.OrderItemID] = r.RefundingQty
	}
	return out, nil
}

func (t tenantTx) FindSettledPayment(ctx context.Context, orderID int64) (SettledPayment, error) {
	r, err := t.q.FindSettledPayment(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SettledPayment{}, fmt.Errorf("order id %d: %w", orderID, ErrNoSettledPayment)
	}
	if err != nil {
		return SettledPayment{}, err
	}
	return SettledPayment{ID: r.ID, PaymentNo: r.PaymentNo, Channel: r.Channel}, nil
}

func (t tenantTx) InsertRefund(ctx context.Context, r NewRefund) (int64, error) {
	evidence := r.EvidenceURLs
	if evidence == nil {
		evidence = []string{}
	}
	id, err := t.q.InsertRefund(ctx, db.InsertRefundParams{
		RefundNo:         r.RefundNo,
		OrderID:          r.OrderID,
		PaymentID:        &r.PaymentID,
		UserID:           &r.UserID,
		RefundType:       r.RefundType,
		ReasonCode:       r.ReasonCode,
		ReasonText:       r.ReasonText,
		EvidenceUrls:     evidence,
		GoodsAmountCents: r.GoodsAmountCents,
		FreightCents:     r.FreightCents,
		AmountCents:      r.GoodsAmountCents + r.FreightCents,
		Channel:          r.Channel,
	})
	if err != nil {
		return 0, err
	}
	for _, it := range r.Items {
		if err := t.q.InsertRefundItem(ctx, db.InsertRefundItemParams{
			RefundID:    id,
			OrderItemID: it.OrderItemID,
			Quantity:    it.Quantity,
			AmountCents: it.AmountCents,
		}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// RefundChannelPlatform 是渠道退款单的 refunds.channel（00323）：钱在平台上退，keel 只记账。
const RefundChannelPlatform int16 = 10

// NewChannelRefund 是一张渠道退款单（直接落 40 已退款，没有买家与 payments 行）。
// ChannelRefundID 是幂等键（channel_refund:<binding>:<平台退款 ID>）。
type NewChannelRefund struct {
	RefundNo         string
	OrderID          int64
	ReasonText       string
	GoodsAmountCents int64
	FreightCents     int64
	ChannelRefundID  string
	RefundedAt       time.Time
	Items            []NewRefundItem
}

// InsertChannelRefund 落一张渠道退款单与它的明细（只记退款单，回写订单行 / 订单金额由调用方照入账那几步做）。
func (t tenantTx) InsertChannelRefund(ctx context.Context, r NewChannelRefund) (int64, error) {
	if r.ChannelRefundID == "" {
		return 0, errors.New("渠道退款的幂等键为空")
	}
	reason := r.ReasonText
	id, err := t.q.InsertChannelRefund(ctx, db.InsertChannelRefundParams{RefundNo: r.RefundNo, OrderID: r.OrderID,
		ReasonText: &reason, GoodsAmountCents: r.GoodsAmountCents, FreightCents: r.FreightCents,
		ChannelRefundID: &r.ChannelRefundID, RefundedAt: pgtype.Timestamptz{Time: r.RefundedAt, Valid: true}})
	if err != nil {
		return 0, err
	}
	for _, it := range r.Items {
		if err := t.q.InsertRefundItem(ctx, db.InsertRefundItemParams{RefundID: id, OrderItemID: it.OrderItemID,
			Quantity: it.Quantity, AmountCents: it.AmountCents}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// ChannelRefundExists：这个幂等键的渠道退款单记过没有。
func (t tenantTx) ChannelRefundExists(ctx context.Context, channelRefundID string) (bool, error) {
	return t.q.ChannelRefundExists(ctx, channelRefundID)
}

func (t tenantTx) AddChannelRefundedCents(ctx context.Context, orderID, amount int64) error {
	return t.q.AddChannelRefundedCents(ctx, db.AddChannelRefundedCentsParams{ID: orderID, Amount: amount})
}

func (t tenantTx) StartWholeOrderRefund(ctx context.Context, orderID int64) (bool, error) {
	n, err := t.q.StartWholeOrderRefund(ctx, orderID)
	if err != nil {
		return false, transitionErr(err)
	}
	return n == 1, nil
}

func (t tenantTx) RevertWholeOrderRefund(ctx context.Context, orderID int64) (bool, error) {
	n, err := t.q.RevertWholeOrderRefund(ctx, orderID)
	if err != nil {
		return false, transitionErr(err)
	}
	return n == 1, nil
}

func (t tenantTx) FinishWholeOrderRefund(ctx context.Context, orderID int64) (bool, error) {
	n, err := t.q.FinishWholeOrderRefund(ctx, orderID)
	if err != nil {
		return false, transitionErr(err)
	}
	return n == 1, nil
}

func (t tenantTx) RecomputeOrderRefundStatus(ctx context.Context, orderID int64) error {
	return t.q.RecomputeOrderRefundStatus(ctx, orderID)
}

func refundFromRow(r db.GetRefundByNoRow) Refund {
	evidence := r.EvidenceUrls
	if evidence == nil {
		evidence = []string{}
	}
	return Refund{
		ID:               r.ID,
		RefundNo:         r.RefundNo,
		OrderID:          r.OrderID,
		OrderNo:          r.OrderNo,
		StoreID:          r.StoreID,
		PaymentNo:        r.PaymentNo,
		UserID:           r.UserID,
		RefundType:       r.RefundType,
		ReasonCode:       r.ReasonCode,
		ReasonText:       r.ReasonText,
		EvidenceURLs:     evidence,
		GoodsAmountCents: r.GoodsAmountCents,
		FreightCents:     r.FreightCents,
		AmountCents:      r.AmountCents,
		Status:           r.Status,
		Channel:          r.Channel,
		ChannelRefundID:  r.ChannelRefundID,
		RejectReason:     r.RejectReason,
		AuditedAt:        optTime(r.AuditedAt),
		RefundedAt:       optTime(r.RefundedAt),
		CreatedAt:        r.CreatedAt.Time,
		UpdatedAt:        r.UpdatedAt.Time,
		Items:            []RefundItem{},
		ReturnShipment:   returnShipmentOf(r.ReturnCarrierCode, r.ReturnTrackingNo, r.ReturnSubmittedAt),
	}
}

// returnShipmentOf 把三列收成一个可空的值。chk_refund_return_shipment 保证三列同生同灭，
// 所以只看一列就够；仍然三列都判，是因为「半截的物流」一旦出现（约束被改坏），
// 返回 nil 比返回一个空单号更不会误导人。
func returnShipmentOf(carrier, tracking *string, at pgtype.Timestamptz) *ReturnShipment {
	if carrier == nil || tracking == nil || !at.Valid {
		return nil
	}
	return &ReturnShipment{CarrierCode: *carrier, TrackingNo: *tracking, SubmittedAt: at.Time}
}

// withItems 给一批退款单挂上明细（一次查询，不是 N 次），顺带填寄回截止时间。
//
// 寄回截止放在这里而不是各个 handler：退款单从五六条读路径出去（买家详情、列表、
// 订单详情、后台详情、各个写接口的回显），在这一处算，就不会有哪条路径漏了它。
// 只有这一批里真有「等买家寄回」的单时才读一次店铺设置。
func (t tenantTx) withItems(ctx context.Context, refunds []Refund) ([]Refund, error) {
	if len(refunds) == 0 {
		return refunds, nil
	}
	if err := t.withReturnDeadlines(ctx, refunds); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(refunds))
	idx := make(map[int64]int, len(refunds))
	for i, r := range refunds {
		ids = append(ids, r.ID)
		idx[r.ID] = i
	}
	rows, err := t.q.ListRefundItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, it := range rows {
		i := idx[it.RefundID]
		refunds[i].Items = append(refunds[i].Items, RefundItem{
			OrderItemID: it.OrderItemID,
			SKUID:       it.SkuID,
			Quantity:    it.Quantity,
			AmountCents: it.AmountCents,
			Title:       it.TitleSnapshot,
			ImageURL:    it.ImageSnapshot,
		})
	}
	return refunds, nil
}

// withReturnDeadlines 给「等买家寄回」的退款单填 ReturnDeadlineAt（与 service/return_timeout.go
// 的截止同一个算式：audited_at + N 天）。
func (t tenantTx) withReturnDeadlines(ctx context.Context, refunds []Refund) error {
	days := 0
	for i := range refunds {
		if !refunds[i].AwaitingReturn() {
			continue
		}
		if days == 0 {
			p, err := t.ShopPreferences(ctx)
			if err != nil {
				return err
			}
			days = p.ReturnShipDays
		}
		at := refunds[i].AuditedAt.Add(time.Duration(days) * 24 * time.Hour)
		refunds[i].ReturnDeadlineAt = &at
	}
	return nil
}

func (t tenantTx) FindRefundByNo(ctx context.Context, refundNo string) (Refund, error) {
	r, err := t.q.GetRefundByNo(ctx, refundNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, fmt.Errorf("refund %s: %w", refundNo, ErrRefundNotFound)
	}
	if err != nil {
		return Refund{}, err
	}
	out, err := t.withItems(ctx, []Refund{refundFromRow(r)})
	if err != nil {
		return Refund{}, err
	}
	return out[0], nil
}

func (t tenantTx) FindUserRefundByNo(ctx context.Context, refundNo string, userID int64) (Refund, error) {
	r, err := t.q.GetUserRefundByNo(ctx, db.GetUserRefundByNoParams{RefundNo: refundNo, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, fmt.Errorf("refund %s: %w", refundNo, ErrRefundNotFound)
	}
	if err != nil {
		return Refund{}, err
	}
	out, err := t.withItems(ctx, []Refund{refundFromRow(db.GetRefundByNoRow(r))})
	if err != nil {
		return Refund{}, err
	}
	return out[0], nil
}

func (t tenantTx) LockRefundStatus(ctx context.Context, refundID int64) (int16, error) {
	s, err := t.q.LockRefundStatus(ctx, refundID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("refund id %d: %w", refundID, ErrRefundNotFound)
	}
	return s, err
}

func (t tenantTx) ListOrderRefunds(ctx context.Context, orderID int64) ([]Refund, error) {
	rows, err := t.q.ListOrderRefunds(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]Refund, 0, len(rows))
	for _, r := range rows {
		out = append(out, refundFromRow(db.GetRefundByNoRow(r)))
	}
	return t.withItems(ctx, out)
}

func (t tenantTx) ListUserRefunds(ctx context.Context, userID int64, status *int16, limit, offset int64) ([]Refund, error) {
	rows, err := t.q.ListUserRefunds(ctx, db.ListUserRefundsParams{
		UserID: userID, Status: status, PageLimit: int32(limit), PageOffset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Refund, 0, len(rows))
	for _, r := range rows {
		out = append(out, refundFromRow(db.GetRefundByNoRow(r)))
	}
	return t.withItems(ctx, out)
}

func (t tenantTx) CountUserRefunds(ctx context.Context, userID int64, status *int16) (int64, error) {
	return t.q.CountUserRefunds(ctx, db.CountUserRefundsParams{UserID: userID, Status: status})
}

// refundTransitionErr 把退款状态机触发器的 23514 挑成 ErrIllegalRefundTransition。
func refundTransitionErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == refundTransitionConstraint {
		return fmt.Errorf("%w: %s", ErrIllegalRefundTransition, pgErr.Message)
	}
	return err
}

func rowsOrTransition(n int64, err error) (bool, error) {
	if err != nil {
		return false, refundTransitionErr(err)
	}
	return n == 1, nil
}

// ReturnOverdueRefund 是退货超时扫描扫到的一张退款单。
type ReturnOverdueRefund struct {
	ID       int64
	RefundNo string
	OrderID  int64
}

func (t tenantTx) ListReturnOverdueRefunds(ctx context.Context, cutoff time.Time,
	limit int32) ([]ReturnOverdueRefund, error) {
	rows, err := t.q.ListReturnOverdueRefunds(ctx, db.ListReturnOverdueRefundsParams{
		Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true}, PageLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ReturnOverdueRefund, 0, len(rows))
	for _, r := range rows {
		out = append(out, ReturnOverdueRefund{ID: r.ID, RefundNo: r.RefundNo, OrderID: r.OrderID})
	}
	return out, nil
}

func (t tenantTx) ExpireReturnRefund(ctx context.Context, refundID int64, cutoff time.Time) (bool, error) {
	return rowsOrTransition(t.q.ExpireReturnRefund(ctx, db.ExpireReturnRefundParams{
		ID: refundID, Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true},
	}))
}

func (t tenantTx) CancelRefund(ctx context.Context, refundID, userID int64) (bool, error) {
	return rowsOrTransition(t.q.CancelRefund(ctx, db.CancelRefundParams{ID: refundID, UserID: userID}))
}

func (t tenantTx) RejectRefund(ctx context.Context, refundID int64, reason string, staffID int64) (bool, error) {
	return rowsOrTransition(t.q.RejectRefund(ctx, db.RejectRefundParams{
		ID: refundID, RejectReason: &reason, AuditedBy: &staffID,
	}))
}

func (t tenantTx) ApproveRefund(ctx context.Context, refundID int64, next int16, freightCents int64,
	staffID int64) (bool, error) {
	return rowsOrTransition(t.q.ApproveRefund(ctx, db.ApproveRefundParams{
		ID: refundID, NextStatus: next, FreightCents: freightCents, AuditedBy: &staffID,
	}))
}

func (t tenantTx) ReceiveRefundGoods(ctx context.Context, refundID int64, staffID int64) (bool, error) {
	return rowsOrTransition(t.q.ReceiveRefundGoods(ctx, db.ReceiveRefundGoodsParams{
		ID: refundID, ReceivedBy: &staffID,
	}))
}

func (t tenantTx) SubmitReturnShipment(ctx context.Context, refundID, userID int64,
	carrierCode, trackingNo string) (bool, error) {
	n, err := t.q.SubmitReturnShipment(ctx, db.SubmitReturnShipmentParams{
		CarrierCode: &carrierCode, TrackingNo: &trackingNo, ID: refundID, UserID: userID,
	})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (t tenantTx) CompleteRefund(ctx context.Context, refundID int64, channelRefundID string,
	payload []byte, at time.Time) (bool, error) {
	if channelRefundID == "" {
		// 空流水号会让那个部分唯一索引整个失效 —— 幂等的最后一道锁静默消失。
		// NOT NULL 拦不住空串，同 InsertPayment 那一段。
		return false, errors.New("渠道退款流水号为空：退款回调的幂等全靠它")
	}
	n, err := t.q.CompleteRefund(ctx, db.CompleteRefundParams{
		ID:              refundID,
		ChannelRefundID: &channelRefundID,
		NotifyPayload:   payload,
		RefundedAt:      pgtype.Timestamptz{Time: at, Valid: true},
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == channelRefundConstraint {
			return false, fmt.Errorf("channel_refund_id=%s: %w", channelRefundID, ErrDuplicateChannelRefund)
		}
		return false, refundTransitionErr(err)
	}
	return n == 1, nil
}

func (t tenantTx) RecordRefundNotify(ctx context.Context, refundID int64, payload []byte) error {
	return t.q.RecordRefundNotify(ctx, db.RecordRefundNotifyParams{ID: refundID, NotifyPayload: payload})
}

func (t tenantTx) WriteBackOrderItemRefund(ctx context.Context, orderItemID int64, qty int32, amount int64) error {
	n, err := t.q.WriteBackOrderItemRefund(ctx, db.WriteBackOrderItemRefundParams{
		ID: orderItemID, Qty: qty, Amount: amount,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("order_item %d 回写 %d 件 / %d 分: %w", orderItemID, qty, amount, ErrRefundOverflow)
	}
	return nil
}

func (t tenantTx) AddOrderRefundedCents(ctx context.Context, orderID, amount int64) error {
	return t.q.AddOrderRefundedCents(ctx, db.AddOrderRefundedCentsParams{ID: orderID, RefundedCents: amount})
}

func (t tenantTx) CountOrderItemsNotFullyRefunded(ctx context.Context, orderID int64) (int64, error) {
	return t.q.CountOrderItemsNotFullyRefunded(ctx, orderID)
}

func (t tenantTx) ReturnCouponForOrder(ctx context.Context, orderID int64) (bool, error) {
	n, err := t.q.ReturnCouponForOrder(ctx, &orderID)
	return n == 1, err
}

func (t tenantTx) OtherRefundFreight(ctx context.Context, orderID, refundID int64) (int64, error) {
	return t.q.OtherRefundFreight(ctx, db.OtherRefundFreightParams{OrderID: orderID, ID: refundID})
}
