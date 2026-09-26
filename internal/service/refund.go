package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 退款与售后（数据模型 §11）：
//
//	POST /orders/{order_no}/refunds        买家申请        → 10 待审核
//	POST /refunds/{refund_no}/cancel       买家撤回        10 / 20 → 60
//	POST /admin/refunds/{refund_no}/audit  后台审核        10 → 20 / 30 / 50
//	POST /admin/refunds/{refund_no}/receipt 后台确认收到退货 20 → 30
//	POST /webhooks/refunds/{channel}       渠道回调入账    30 → 40
//	GET  /refunds、/refunds/{refund_no}、/orders/{order_no}/refunds
//
// # 为什么入账是一个本地事务，而不是 §11 写的三分支 SAGA
//
// §11 的编排是「渠道退款、库存回补、订单金额回写」三个分支，渠道退款没有补偿、
// 必须排最后。那条约束要防的是「钱退出去了，后面的分支失败了回滚不了」。
//
// 本仓库的形态里，**渠道退款不是我们发起的一个分支，而是渠道推回来的一次回调**：
// 钱在回调到达之前已经退出去了（真实渠道是「申请退款 → 渠道异步退 → 回调通知」）。
// 回调要做的只剩三件本地的事 —— 退款单 30 → 40、回写订单项与订单金额、按规则回补
// 库存与券 —— 它们在同一个库里，一个事务就能同生共死。任何一步失败，整个事务
// 回滚、回 5xx，渠道会重推（契约：未能入账请渠道重推）；退款单还停在 30，
// 没有「钱退了一半账」的中间态。这与支付回调（payment.go）是同一个形状。
// 真接渠道时要加的是「审核通过 → 调渠道退款 API」那一步，它是一个出站调用，
// 失败就重试（30 没有失败态），也不需要补偿。这笔出入写进了数据模型 §11。
//
// # 沙箱渠道
//
// 支付的沙箱把签好名的回调交给买家（payment_intent.go），因为「付款」是买家的动作。
// 退款的「渠道把钱退回来」没有一个自然的人来扮演，所以沙箱开着
// （KEEL_PAYMENT_SANDBOX，与支付同一个开关）时，退款单进入 30 的**同一个事务**里，
// 服务端自己扮演渠道：造一份规范回调报文、用这家店的回调密钥签名、过同一个
// verifyChannelSignature、走同一个 settleTx —— 金额校验、流水号唯一索引、
// 状态机、回写，一条都不少。响应里看到的就是 40 已退款。
// 沙箱关掉时退款单停在 30，等真实渠道的回调（真渠道适配器本轮没有）。
//
// # 退款到账之后库存与券怎么办（§11 的规则 + 文档没写的部分取保守）
//
//	库存：只有**这一单还没发货**（shipped_at 为空）时才回补到履约门店，流水
//	      biz_type 4。已发货的货在买家手里或在退回的路上，成色未知 ——
//	      退货入库是商家收到货、验过之后的手工调整（biz_type 5），不自动加回可售。
//	券  ：整单的货都退完了才退回「未使用」（且没过期）；部分退款不退（§11 原文）。

var (
	// ErrRefundOrderNotFound：申请退款时订单不存在或不属于当前用户。
	// 契约：404，type 点名 order-not-found（别的 404 用通用的 not-found）。
	ErrRefundOrderNotFound = errors.New("订单不存在或不属于当前用户")

	// ErrRefundNotFound：退款单不存在（或不是你的 / 不在本租户）。契约：404。
	ErrRefundNotFound = errors.New("退款单不存在")

	// ErrOrderNotRefundable：订单状态不允许申请售后（待支付、已关闭、整单退款中、
	// 已退款、没付过钱）。契约：409 order-status-not-refundable。
	ErrOrderNotRefundable = errors.New("订单当前状态不允许申请退款")

	// ErrRefundQuantityExceeded：退的件数超过「购买 - 已退」。契约：409。
	ErrRefundQuantityExceeded = errors.New("退款件数超出可退范围")

	// ErrRefundAlreadyInProgress：这一行已经在一张进行中的退款单里。契约：409。
	// uk_refund_items 只保证单张单内不重复，跨单重复必须业务层拦（契约原话）。
	ErrRefundAlreadyInProgress = errors.New("这一行已有进行中的退款单")

	// ErrOrderItemMismatch：order_item_id 不属于这一单。契约：422 order-item-mismatch。
	ErrOrderItemMismatch = errors.New("订单项不属于这个订单")

	// ErrRefundNotCancelable：只有 10 待审核 / 20 待买家退货能撤回。契约：409。
	ErrRefundNotCancelable = errors.New("退款单当前状态不允许撤回")

	// ErrRefundNotAuditable：只有 10 待审核能审。契约：409。
	ErrRefundNotAuditable = errors.New("退款单当前状态不允许审核")

	// ErrRefundNotReceivable：只有 20 待买家退货能确认收到退货。契约：409。
	ErrRefundNotReceivable = errors.New("退款单当前状态不允许确认收货")

	// ErrRefundFreightExceeded：审核裁定的退运费超过订单实收运费（减去别的退款单
	// 已占的）。契约：422 refund-freight-exceeded。
	ErrRefundFreightExceeded = errors.New("退运费超过订单实收运费")

	// —— 退款回调那一组。与支付回调同构：验签失败 401；以下这些都是「渠道重推也
	// 没用」的业务结论，回 200 并留一条 Error 日志等人处置。

	// ErrRefundWebhookUnknown：退款单号在本租户查不到。
	ErrRefundWebhookUnknown = errors.New("退款回调里的退款单号在本租户查不到")

	// ErrRefundWebhookAmountMismatch：到账金额与退款单金额对不上。
	// 原始报文留在退款单上，状态不动 —— 系统不替任何一方认账。
	ErrRefundWebhookAmountMismatch = errors.New("退款回调的金额与退款单金额不符")

	// ErrRefundWebhookNotRefunding：回调到达时退款单不在 30 退款中（且不是同一笔的重推）。
	ErrRefundWebhookNotRefunding = errors.New("退款回调到达时退款单不在退款中")

	// ErrRefundWebhookChannelMismatch：回调走的渠道与退款单原路退回的渠道不是同一个。
	ErrRefundWebhookChannelMismatch = errors.New("退款回调的渠道与退款单的渠道不符")
)

const (
	idempotencyScopeRefundCreate  = "refunds.create"
	idempotencyScopeRefundCancel  = "refunds.cancel"
	idempotencyScopeRefundAudit   = "admin.refunds.audit"
	idempotencyScopeRefundReceipt = "admin.refunds.receipt"

	refundTextMaxRunes   = 200
	refundEvidenceMax    = 9
	refundEvidenceMaxLen = 1024

	// sandboxRefundPrefix 是沙箱渠道退款流水号的前缀，与支付的 KEEL-SANDBOX- 同一个用意：
	// 肉眼可辨，没有人会把它当成一笔真实的微信退款。
	sandboxRefundPrefix = "KEEL-SANDBOX-R-"
)

// refundChannelNames 是 refunds.channel → 回调路径上的渠道名。只有 wechat / alipay：
// 余额退款不产生渠道回调（契约 /webhooks/refunds 的 channel 枚举原话）。
var refundChannelNames = map[int16]string{
	repository.PaymentChannelWechat: "wechat",
	repository.PaymentChannelAlipay: "alipay",
}

// refundNotification 是退款回调报文里本系统需要的字段 —— Keel 的规范形状，
// 不是任何真实渠道的格式（诚实边界与 payment.go 的 notification 一字不差）。
type refundNotification struct {
	RefundNo        string     `json:"refund_no"`
	ChannelRefundID string     `json:"channel_refund_id"`
	AmountCents     int64      `json:"amount_cents"`
	RefundedAt      *time.Time `json:"refunded_at,omitempty"`
}

// RefundRepository 是退款需要的仓储能力：开租户事务、取回调密钥。
type RefundRepository interface {
	tenantRunner
	secretSource
}

// RefundService 实现退款与售后。
type RefundService struct {
	repo RefundRepository
	cfg  PaymentConfig
	log  *slog.Logger
	now  func() time.Time
}

func NewRefundService(r RefundRepository, cfg PaymentConfig, log *slog.Logger) *RefundService {
	if log == nil {
		log = slog.Default()
	}
	return &RefundService{repo: r, cfg: cfg, log: log, now: time.Now}
}

// RefundCreateRequest 是 RefundCreateRequest 在 service 边界上的形状。
// json tag 是 request_hash 的规范化形状（encoding/json 按字段声明顺序序列化）。
type RefundCreateRequest struct {
	Items        []RefundLineInput `json:"items"`
	RefundType   int16             `json:"refund_type"`
	ReasonCode   int16             `json:"reason_code"`
	ReasonText   *string           `json:"reason_text,omitempty"`
	EvidenceURLs []string          `json:"evidence_urls,omitempty"`
}

func (r RefundCreateRequest) validate() error {
	if r.RefundType != repository.RefundTypeMoneyOnly && r.RefundType != repository.RefundTypeReturnGoods {
		return fmt.Errorf("%w: refund_type 只能是 1 或 2", ErrRefundBadRequest)
	}
	if r.ReasonCode < 1 || r.ReasonCode > 5 {
		return fmt.Errorf("%w: reason_code 只能是 1～5", ErrRefundBadRequest)
	}
	if r.ReasonText != nil && utf8.RuneCountInString(*r.ReasonText) > refundTextMaxRunes {
		return fmt.Errorf("%w: reason_text 超过 %d 个字符", ErrRefundBadRequest, refundTextMaxRunes)
	}
	if len(r.EvidenceURLs) > refundEvidenceMax {
		return fmt.Errorf("%w: evidence_urls 至多 %d 张", ErrRefundBadRequest, refundEvidenceMax)
	}
	for _, u := range r.EvidenceURLs {
		if strings.TrimSpace(u) == "" || len(u) > refundEvidenceMaxLen {
			return fmt.Errorf("%w: evidence_urls 里有空的或过长的地址", ErrRefundBadRequest)
		}
	}
	if len(r.Items) == 0 {
		return fmt.Errorf("%w: items 至少一行", ErrRefundBadRequest)
	}
	return nil
}

// Create 实现 POST /orders/{order_no}/refunds。
//
// 整个申请在订单行锁之下完成（LockUserOrderByNo 是 FOR UPDATE）：
// 「在途件数」的复算与退款单的插入之间没有别的申请插得进来 —— 这是 §11 那句
// 「数据库会放行契约算式禁止的超退」在业务层的兑现。
func (s *RefundService) Create(ctx context.Context, orderNo string, req RefundCreateRequest,
	idemKey string) (repository.Refund, bool, error) {

	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.Refund{}, false, err
	}
	if err := req.validate(); err != nil {
		return repository.Refund{}, false, err
	}
	canon, err := json.Marshal(req)
	if err != nil {
		return repository.Refund{}, false, err
	}

	return idempotentTx(ctx, s.repo, repository.BuyerSubject(id.UserID),
		idempotencyScopeRefundCreate, idemKey, pathHash(orderNo, string(canon)), archivedCreated,
		func(tx repository.Tx) (repository.Refund, error) {
			order, err := tx.LockUserOrderByNo(ctx, orderNo, id.UserID)
			if errors.Is(err, repository.ErrOrderNotFound) {
				return repository.Refund{}, fmt.Errorf("%w: order_no=%s", ErrRefundOrderNotFound, orderNo)
			}
			if err != nil {
				return repository.Refund{}, err
			}
			switch order.Status {
			case orderStatusPaid, orderStatusShipped, orderStatusFinished:
			default:
				// 10 待支付 / 90 已关闭：没付钱，没有可退的。50 退款中：整单都在途。
				// 60 已退款：没有剩下的了。
				return repository.Refund{}, fmt.Errorf("%w: 订单 %s 当前状态是 %d",
					ErrOrderNotRefundable, orderNo, order.Status)
			}
			if order.PaidCents <= 0 {
				return repository.Refund{}, fmt.Errorf("%w: 订单 %s 实收为 0", ErrOrderNotRefundable, orderNo)
			}
			if req.RefundType == repository.RefundTypeReturnGoods && order.ShippedAt == nil {
				// 货还没发出去，没有东西可以退回来。契约说「已发货但用户还没收到货时
				// 两种都合法」—— 反过来，未发货时只有仅退款合法。
				return repository.Refund{}, fmt.Errorf("%w: 订单还没发货，只能申请仅退款（refund_type=1）",
					ErrRefundBadRequest)
			}

			items, err := tx.ListRefundableItems(ctx, order.ID)
			if err != nil {
				return repository.Refund{}, err
			}
			inflight, err := tx.RefundingQtyByItem(ctx, order.ID)
			if err != nil {
				return repository.Refund{}, err
			}
			plan, err := planRefund(items, inflight, req.Items)
			if err != nil {
				return repository.Refund{}, err
			}

			// 运费（§11 一期规则）：未发货整单退全退；部分退款不退；
			// 退货退款的运费由客服在审核时裁定（这里先记 0）。
			whole := plan.CoversEverything && order.Status == orderStatusPaid
			var freight int64
			if whole {
				freight = order.FreightCents
			}
			if plan.Goods+freight <= 0 {
				// 一行被券分摊到实付 0 元：没有钱可退，chk_refund_amount 也不收一张 0 元的单。
				return repository.Refund{}, fmt.Errorf("%w: 这几行的实付净额是 0，没有可退的金额", ErrRefundBadRequest)
			}

			pay, err := tx.FindSettledPayment(ctx, order.ID)
			if err != nil {
				return repository.Refund{}, err
			}
			refundNo, err := newRefundNo(s.now())
			if err != nil {
				return repository.Refund{}, err
			}
			if _, err := tx.InsertRefund(ctx, repository.NewRefund{
				RefundNo:         refundNo,
				OrderID:          order.ID,
				PaymentID:        pay.ID,
				UserID:           id.UserID,
				RefundType:       req.RefundType,
				ReasonCode:       req.ReasonCode,
				ReasonText:       req.ReasonText,
				EvidenceURLs:     req.EvidenceURLs,
				GoodsAmountCents: plan.Goods,
				FreightCents:     freight,
				Channel:          pay.Channel,
				Items:            plan.Lines,
			}); err != nil {
				return repository.Refund{}, err
			}
			if whole {
				// 未发货整单退：履约维度 20 → 50（§5 两维度那张表的第一行）。
				// 我们持有订单行锁、刚读到它是 20，所以这里失配就是不变量坏了。
				ok, err := tx.StartWholeOrderRefund(ctx, order.ID)
				if err != nil {
					return repository.Refund{}, err
				}
				if !ok {
					return repository.Refund{}, fmt.Errorf("订单 %s 在行锁之下从 20 推 50 失败", orderNo)
				}
			}
			if err := tx.RecomputeOrderRefundStatus(ctx, order.ID); err != nil {
				return repository.Refund{}, err
			}
			return tx.FindUserRefundByNo(ctx, refundNo, id.UserID)
		})
}

// Cancel 实现 POST /refunds/{refund_no}/cancel（买家撤回）。
func (s *RefundService) Cancel(ctx context.Context, refundNo, idemKey string) (repository.Refund, bool, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.Refund{}, false, err
	}
	return idempotentTx(ctx, s.repo, repository.BuyerSubject(id.UserID),
		idempotencyScopeRefundCancel, idemKey, pathHash(refundNo), archivedOK,
		func(tx repository.Tx) (repository.Refund, error) {
			r, err := tx.FindUserRefundByNo(ctx, refundNo, id.UserID)
			if errors.Is(err, repository.ErrRefundNotFound) {
				return repository.Refund{}, fmt.Errorf("%w: refund_no=%s", ErrRefundNotFound, refundNo)
			}
			if err != nil {
				return repository.Refund{}, err
			}
			order, status, err := lockRefund(ctx, tx, r)
			if err != nil {
				return repository.Refund{}, err
			}
			ok, err := tx.CancelRefund(ctx, r.ID, id.UserID)
			if errors.Is(err, repository.ErrIllegalRefundTransition) {
				ok, err = false, nil
			}
			if err != nil {
				return repository.Refund{}, err
			}
			if !ok {
				return repository.Refund{}, fmt.Errorf("%w: 退款单 %s 当前状态是 %d，只有 10 / 20 能撤回",
					ErrRefundNotCancelable, refundNo, status)
			}
			if err := s.leaveRefunding(ctx, tx, order); err != nil {
				return repository.Refund{}, err
			}
			return tx.FindUserRefundByNo(ctx, refundNo, id.UserID)
		})
}

// lockRefund 按「先订单、后退款单」的顺序锁住这张退款单（db/queries/refunds.sql 文件头），
// 返回订单与退款单此刻的状态。
func lockRefund(ctx context.Context, tx repository.Tx, r repository.Refund) (repository.Order, int16, error) {
	order, err := tx.LockOrderByID(ctx, r.OrderID)
	if err != nil {
		return repository.Order{}, 0, err
	}
	status, err := tx.LockRefundStatus(ctx, r.ID)
	if err != nil {
		return repository.Order{}, 0, err
	}
	return order, status, nil
}

// leaveRefunding 是一张退款单**没有退成**（驳回 / 撤回）之后订单那一侧的收尾：
// 整单退款中的订单回到 20 已支付（§5 的 (50,20)），资金维度按事实重算。
//
// 50 退款中只可能是「一张覆盖了全部剩余件数、且当时没有别的在途单」的整单退款
// （planRefund 的 CoversEverything），而订单停在 50 期间不能再申请新的售后 ——
// 所以此刻离开的这张就是让它进 50 的那一张。
func (s *RefundService) leaveRefunding(ctx context.Context, tx repository.Tx, order repository.Order) error {
	if order.Status == orderStatusRefunding {
		ok, err := tx.RevertWholeOrderRefund(ctx, order.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("订单 %s 在行锁之下从 50 回 20 失败", order.OrderNo)
		}
	}
	return tx.RecomputeOrderRefundStatus(ctx, order.ID)
}

// ---------------------------------------------------------------------------
// 后台：审核、确认收到退货
// ---------------------------------------------------------------------------

// AuditRequest 是审核请求体。
type AuditRequest struct {
	Action       string  `json:"action"`
	RejectReason *string `json:"reject_reason,omitempty"`
	FreightCents *int64  `json:"freight_cents,omitempty"`
}

// Audit 实现 POST /admin/refunds/{refund_no}/audit。
//
// 谁能审：与发货同一个判据（门店库存那一行，storeOperate），按订单的履约门店判。
// 退款是资金动作，比发货更敏感，但它的对象仍是「这家店卖出去的这一单」——
// 一个管不了这家店库存与发货的人，也不该能替这家店把钱退出去；反过来，
// 门店自己的管理员处理本店售后是连锁的常态。契约没写，写在报告里。
func (s *RefundService) Audit(ctx context.Context, refundNo string, req AuditRequest,
	idemKey string) (repository.Refund, bool, error) {

	staff, err := requireStaff(ctx)
	if err != nil {
		return repository.Refund{}, false, err
	}
	switch req.Action {
	case "approve":
	case "reject":
		if req.RejectReason == nil || strings.TrimSpace(*req.RejectReason) == "" {
			return repository.Refund{}, false, fmt.Errorf("%w: 驳回必须填写 reject_reason", ErrRefundBadRequest)
		}
		if utf8.RuneCountInString(*req.RejectReason) > refundTextMaxRunes {
			return repository.Refund{}, false, fmt.Errorf("%w: reject_reason 超过 %d 个字符",
				ErrRefundBadRequest, refundTextMaxRunes)
		}
	default:
		return repository.Refund{}, false, fmt.Errorf("%w: action 只能是 approve 或 reject", ErrRefundBadRequest)
	}
	if req.FreightCents != nil && *req.FreightCents < 0 {
		return repository.Refund{}, false, fmt.Errorf("%w: freight_cents 不能为负", ErrRefundBadRequest)
	}
	canon, err := json.Marshal(req)
	if err != nil {
		return repository.Refund{}, false, err
	}

	return idempotentTx(ctx, s.repo, repository.StaffSubject(staff.StaffID),
		idempotencyScopeRefundAudit, idemKey, pathHash(refundNo, string(canon)), archivedOK,
		func(tx repository.Tx) (repository.Refund, error) {
			r, order, status, err := s.adminLockRefund(ctx, tx, refundNo)
			if err != nil {
				return repository.Refund{}, err
			}
			if status != repository.RefundPending {
				return repository.Refund{}, fmt.Errorf("%w: 退款单 %s 当前状态是 %d，只有 10 待审核能审",
					ErrRefundNotAuditable, refundNo, status)
			}

			if req.Action == "reject" {
				ok, err := tx.RejectRefund(ctx, r.ID, strings.TrimSpace(*req.RejectReason))
				if err := notAuditable(ok, err, refundNo); err != nil {
					return repository.Refund{}, err
				}
				if err := s.leaveRefunding(ctx, tx, order); err != nil {
					return repository.Refund{}, err
				}
				return tx.FindRefundByNo(ctx, refundNo)
			}

			freight := r.FreightCents
			if req.FreightCents != nil && *req.FreightCents != r.FreightCents {
				if r.RefundType != repository.RefundTypeReturnGoods {
					// 仅退款的运费由规则决定（未发货整单退全退，其余不退），
					// 契约只把裁定权交给了退货退款。
					return repository.Refund{}, fmt.Errorf("%w: 仅退款的运费按规则计算，审核时不能改",
						ErrRefundBadRequest)
				}
				other, err := tx.OtherRefundFreight(ctx, order.ID, r.ID)
				if err != nil {
					return repository.Refund{}, err
				}
				if limit := order.FreightCents - other; *req.FreightCents > limit {
					return repository.Refund{}, fmt.Errorf("%w: 订单实收运费 %d，别的退款单已占 %d，最多还能退 %d",
						ErrRefundFreightExceeded, order.FreightCents, other, limit)
				}
				freight = *req.FreightCents
			}
			next := repository.RefundProcessing
			if r.RefundType == repository.RefundTypeReturnGoods {
				next = repository.RefundAwaitingReturn
			}
			ok, err := tx.ApproveRefund(ctx, r.ID, next, freight)
			if err := notAuditable(ok, err, refundNo); err != nil {
				return repository.Refund{}, err
			}
			if next == repository.RefundProcessing {
				if err := s.submitToChannel(ctx, tx, refundNo); err != nil {
					return repository.Refund{}, err
				}
			}
			return tx.FindRefundByNo(ctx, refundNo)
		})
}

func notAuditable(ok bool, err error, refundNo string) error {
	if errors.Is(err, repository.ErrIllegalRefundTransition) {
		ok, err = false, nil
	}
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 退款单 %s 在审核的同时被改了状态", ErrRefundNotAuditable, refundNo)
	}
	return nil
}

// Receive 实现 POST /admin/refunds/{refund_no}/receipt（商家确认收到退货，20 → 30）。
//
// 契约的状态机里有「20 待买家退货 ─商家收货─► 30 退款中」这条边，却没有任何一条
// 接口走它 —— 退货退款会永远停在 20。本轮在契约里补了这一条（契约先行）。
func (s *RefundService) Receive(ctx context.Context, refundNo, idemKey string) (repository.Refund, bool, error) {
	staff, err := requireStaff(ctx)
	if err != nil {
		return repository.Refund{}, false, err
	}
	return idempotentTx(ctx, s.repo, repository.StaffSubject(staff.StaffID),
		idempotencyScopeRefundReceipt, idemKey, pathHash(refundNo), archivedOK,
		func(tx repository.Tx) (repository.Refund, error) {
			r, _, status, err := s.adminLockRefund(ctx, tx, refundNo)
			if err != nil {
				return repository.Refund{}, err
			}
			if status != repository.RefundAwaitingReturn {
				return repository.Refund{}, fmt.Errorf("%w: 退款单 %s 当前状态是 %d，只有 20 待买家退货能确认收货",
					ErrRefundNotReceivable, refundNo, status)
			}
			ok, err := tx.ReceiveRefundGoods(ctx, r.ID)
			if errors.Is(err, repository.ErrIllegalRefundTransition) {
				ok, err = false, nil
			}
			if err != nil {
				return repository.Refund{}, err
			}
			if !ok {
				return repository.Refund{}, fmt.Errorf("%w: 退款单 %s 在确认收货的同时被改了状态",
					ErrRefundNotReceivable, refundNo)
			}
			if err := s.submitToChannel(ctx, tx, refundNo); err != nil {
				return repository.Refund{}, err
			}
			return tx.FindRefundByNo(ctx, refundNo)
		})
}

// adminLockRefund 取退款单、锁订单与退款单、按订单的履约门店判权。
func (s *RefundService) adminLockRefund(ctx context.Context, tx repository.Tx,
	refundNo string) (repository.Refund, repository.Order, int16, error) {
	r, err := tx.FindRefundByNo(ctx, refundNo)
	if errors.Is(err, repository.ErrRefundNotFound) {
		return repository.Refund{}, repository.Order{}, 0,
			fmt.Errorf("%w: refund_no=%s", ErrRefundNotFound, refundNo)
	}
	if err != nil {
		return repository.Refund{}, repository.Order{}, 0, err
	}
	if _, err := authorizeStore(ctx, tx, r.StoreID, storeOperate); err != nil {
		return repository.Refund{}, repository.Order{}, 0, err
	}
	order, status, err := lockRefund(ctx, tx, r)
	if err != nil {
		return repository.Refund{}, repository.Order{}, 0, err
	}
	return r, order, status, nil
}

// submitToChannel 是退款单进入 30 退款中之后「把钱交给渠道去退」那一步。
//
// 沙箱开着：服务端扮演渠道，在同一个事务里造回调、签名、验签、入账（见文件头）。
// 沙箱关着：真实渠道适配器本轮没有，退款单停在 30 等回调 —— 这不是错误，
// 30 本来就没有失败态（§11）。两种「造不出回调」的配置问题（渠道不支持回调、
// 这家店没配密钥）同样只留一条日志、不让审核失败：审核这个动作本身已经成立了。
func (s *RefundService) submitToChannel(ctx context.Context, tx repository.Tx, refundNo string) error {
	if !s.cfg.Sandbox {
		s.log.InfoContext(ctx, "退款单已进入退款中；沙箱关着，而真实退款渠道尚未对接，等渠道回调",
			"refund_no", refundNo)
		return nil
	}
	r, err := tx.FindRefundByNo(ctx, refundNo)
	if err != nil {
		return err
	}
	channel, ok := refundChannelNames[r.Channel]
	if !ok {
		s.log.WarnContext(ctx, "退款单的渠道不产生回调（余额？），沙箱渠道不代为入账，停在退款中",
			"refund_no", refundNo, "channel", r.Channel)
		return nil
	}
	secret, err := s.repo.ChannelNotifySecret(ctx, channel)
	if err != nil {
		return err
	}
	if secret == "" {
		// 绝不退化成「不签名」：那会让回调那条路上「没配密钥 = 拒绝」的判断作废。
		s.log.WarnContext(ctx, "这家店没配该渠道的回调密钥，沙箱渠道造不出可验签的退款回调，停在退款中",
			"refund_no", refundNo, "channel", channel)
		return nil
	}
	txn, err := newSandboxRefundID(s.now())
	if err != nil {
		return err
	}
	body, err := json.Marshal(refundNotification{
		RefundNo:        r.RefundNo,
		ChannelRefundID: txn,
		// 金额取退款单上的实退总额 —— 沙箱模拟的就是「渠道退了该退的那个数」。
		// settleTx 照样会拿它和 amount_cents 比。
		AmountCents: r.AmountCents,
	})
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if err := verifyChannelSignature(ctx, s.repo, s.log, channel, body, hex.EncodeToString(mac.Sum(nil))); err != nil {
		return err
	}
	var n refundNotification
	if err := json.Unmarshal(body, &n); err != nil {
		return err
	}
	outcome, err := s.settleTx(ctx, tx, r.Channel, n, body)
	if err != nil {
		return err
	}
	// 沙箱自己造的报文不该有业务性结论（金额是照抄的、状态刚推到 30）。
	// 真出现了就是装配错了，让审核失败、事务回滚，而不是留下一张半截的单。
	return outcome
}

// ---------------------------------------------------------------------------
// 渠道回调
// ---------------------------------------------------------------------------

// Notify 处理一次退款渠道回调（POST /webhooks/refunds/{channel}）。
//
// 与支付回调同构：Host 决定租户、HMAC 决定真假、没配密钥即拒绝、原始字节验签；
// 幂等由 uk_refunds_channel_txn 兜底。返回 nil 表示这张退款单真的从 30 推到了 40。
func (s *RefundService) Notify(ctx context.Context, channel string, rawBody []byte, signature string) error {
	code, ok := webhookChannels[channel]
	if !ok {
		return fmt.Errorf("%w: 渠道 %q 不在 wechat/alipay 里", ErrWebhookSignature, channel)
	}
	if err := verifyChannelSignature(ctx, s.repo, s.log, channel, rawBody, signature); err != nil {
		return err
	}
	var n refundNotification
	if err := json.Unmarshal(rawBody, &n); err != nil {
		return fmt.Errorf("%w: %v", ErrWebhookBadPayload, err)
	}
	if n.RefundNo == "" || n.ChannelRefundID == "" {
		return fmt.Errorf("%w: refund_no 与 channel_refund_id 都不能为空（实得 %q / %q）",
			ErrWebhookBadPayload, n.RefundNo, n.ChannelRefundID)
	}
	if n.AmountCents <= 0 {
		return fmt.Errorf("%w: amount_cents 是 %d", ErrWebhookBadPayload, n.AmountCents)
	}

	var outcome error
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		outcome, err = s.settleTx(ctx, tx, code, n, rawBody)
		return err
	})
	if errors.Is(err, repository.ErrDuplicateChannelRefund) {
		return fmt.Errorf("%w: %s", ErrWebhookDuplicate, n.ChannelRefundID)
	}
	if err != nil {
		return err
	}
	return outcome
}

// settleTx 是退款入账的业务这一段，跑在调用方给的事务里（回调与沙箱渠道共用）。
//
// 两种出口，与 payment.go 的 settle 同一个约定：
//   - err 非空：整个事务要回滚（查不到、渠道不符、状态不对、重复流水、超退、库坏了）；
//   - outcome 非空、err 为空：事务照常提交，但这笔账我们不认（金额不符 ——
//     原始报文留在退款单上，状态不动）。
func (s *RefundService) settleTx(ctx context.Context, tx repository.Tx, channel int16,
	n refundNotification, rawBody []byte) (outcome error, err error) {

	r, err := tx.FindRefundByNo(ctx, n.RefundNo)
	if errors.Is(err, repository.ErrRefundNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrRefundWebhookUnknown, n.RefundNo)
	}
	if err != nil {
		return nil, err
	}
	order, status, err := lockRefund(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	if r.Channel != channel {
		return nil, fmt.Errorf("%w: 退款单 %s 原路退回到渠道 %d，回调来自渠道 %d",
			ErrRefundWebhookChannelMismatch, n.RefundNo, r.Channel, channel)
	}
	if status != repository.RefundProcessing {
		// 锁之后重读一次：FindRefundByNo 读的是锁之前的快照。
		cur, err := tx.FindRefundByNo(ctx, n.RefundNo)
		if err != nil {
			return nil, err
		}
		if status == repository.RefundSucceeded && cur.ChannelRefundID != nil &&
			*cur.ChannelRefundID == n.ChannelRefundID {
			// 同一笔的重推。**正常路径**。
			return nil, fmt.Errorf("%w: %s", repository.ErrDuplicateChannelRefund, n.ChannelRefundID)
		}
		return nil, fmt.Errorf("%w: 退款单 %s 当前状态是 %d", ErrRefundWebhookNotRefunding, n.RefundNo, status)
	}
	if n.AmountCents != r.AmountCents {
		if err := tx.RecordRefundNotify(ctx, r.ID, rawBody); err != nil {
			return nil, err
		}
		return fmt.Errorf("%w: 退款单 %s 应退 %d，渠道报 %d",
			ErrRefundWebhookAmountMismatch, n.RefundNo, r.AmountCents, n.AmountCents), nil
	}

	at := s.now()
	if n.RefundedAt != nil {
		at = *n.RefundedAt
	}
	ok, err := tx.CompleteRefund(ctx, r.ID, n.ChannelRefundID, rawBody, at)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: 退款单 %s 在行锁之下不在 30 上", ErrRefundWebhookNotRefunding, n.RefundNo)
	}

	// §11「退款与订单状态的联动」，同一个事务：
	// 1. 回写订单项（条件更新，超退则整体回滚）；
	for _, it := range r.Items {
		if err := tx.WriteBackOrderItemRefund(ctx, it.OrderItemID, it.Quantity, it.AmountCents); err != nil {
			return nil, err
		}
	}
	// 2. 累加订单已退金额；
	if err := tx.AddOrderRefundedCents(ctx, order.ID, r.AmountCents); err != nil {
		return nil, err
	}
	// 4. 未发货整单退：履约维度 50 → 60。
	if order.Status == orderStatusRefunding {
		ok, err := tx.FinishWholeOrderRefund(ctx, order.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("订单 %s 在行锁之下从 50 推 60 失败", order.OrderNo)
		}
	}
	// 库存：只有没发过货才回补（文件头的保守规则）。流水 biz_id 记退款单号 ——
	// 一单可以有几张退款单，记订单号的话分不清是哪一张放回来的。
	if order.ShippedAt == nil {
		for _, it := range r.Items {
			after, err := tx.RestoreInventory(ctx, it.SKUID, order.StoreID, it.Quantity)
			if err != nil {
				return nil, err
			}
			if err := tx.AppendInventoryLog(ctx, it.SKUID, order.StoreID, it.Quantity,
				repository.InventoryLogRefundRestock, r.RefundNo, after-it.Quantity, after); err != nil {
				return nil, err
			}
		}
	}
	// 券：整单的货都退完了才退回（§11 末段）。
	if order.UserCouponID != nil {
		left, err := tx.CountOrderItemsNotFullyRefunded(ctx, order.ID)
		if err != nil {
			return nil, err
		}
		if left == 0 {
			returned, err := tx.ReturnCouponForOrder(ctx, order.ID)
			if err != nil {
				return nil, err
			}
			if !returned {
				s.log.InfoContext(ctx, "整单退款到账，但这一单的券已过期（或不在已使用上），没有退回，由客服补发",
					"order_no", order.OrderNo, "user_coupon_id", *order.UserCouponID)
			}
		}
	}
	// 3. 重算资金维度。
	if err := tx.RecomputeOrderRefundStatus(ctx, order.ID); err != nil {
		return nil, err
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// 买家侧读
// ---------------------------------------------------------------------------

// RefundList 是「我的退款单」的一页。
type RefundList struct {
	Items    []repository.Refund
	Page     int
	PageSize int
	Total    int64
}

// ListMine 实现 GET /refunds。
func (s *RefundService) ListMine(ctx context.Context, page, pageSize int, status *int16) (RefundList, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return RefundList{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	out := RefundList{Items: []repository.Refund{}, Page: page, PageSize: pageSize}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		total, err := tx.CountUserRefunds(ctx, id.UserID, status)
		if err != nil {
			return err
		}
		out.Total = total
		rows, err := tx.ListUserRefunds(ctx, id.UserID, status, int64(pageSize), offsetOf(page, pageSize))
		if err != nil {
			return err
		}
		out.Items = rows
		return nil
	})
	return out, err
}

// Detail 实现 GET /refunds/{refund_no}。
func (s *RefundService) Detail(ctx context.Context, refundNo string) (repository.Refund, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.Refund{}, err
	}
	var out repository.Refund
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		r, err := tx.FindUserRefundByNo(ctx, refundNo, id.UserID)
		if errors.Is(err, repository.ErrRefundNotFound) {
			return fmt.Errorf("%w: refund_no=%s", ErrRefundNotFound, refundNo)
		}
		out = r
		return err
	})
	return out, err
}

// ListForOrder 实现 GET /orders/{order_no}/refunds。
func (s *RefundService) ListForOrder(ctx context.Context, orderNo string) ([]repository.Refund, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	var out []repository.Refund
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		order, err := findBuyerOrder(ctx, tx, orderNo, id.UserID)
		if err != nil {
			return err
		}
		out, err = tx.ListOrderRefunds(ctx, order.ID)
		return err
	})
	return out, err
}

// newRefundNo 生成退款单号：14 位时间前缀 + 18 位十六进制随机。
// 形状与订单号 / 支付单号一样（不可枚举，时间前缀给人看），理由见 newPaymentNo。
func newRefundNo(now time.Time) (string, error) {
	var b [orderNoRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成退款单号失败: %w", err)
	}
	return now.UTC().Format("20060102150405") + hex.EncodeToString(b[:]), nil
}

// newSandboxRefundID 生成沙箱渠道的退款流水号（入账后即 channel_refund_id）。
func newSandboxRefundID(now time.Time) (string, error) {
	var b [orderNoRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成沙箱退款流水号失败: %w", err)
	}
	return sandboxRefundPrefix + now.UTC().Format("20060102150405") + "-" + hex.EncodeToString(b[:]), nil
}
