package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 订单后半程的履约维度（数据模型 §5）：
//
//	POST /orders/{order_no}/cancel             买家取消    10 → 90，放回库存与券
//	POST /orders/{order_no}/confirm            买家确认收货 30 → 40
//	POST /admin/orders/{order_no}/shipments    后台发货    20 → 30，落一个包裹
//
// 三条都是**本地事务**：「抢占幂等键 → 条件 UPDATE → 附带动作 → 存档」一个事务
// 装得下（idempotentTx）。其中取消那一条值得单说：
//
// # 取消为什么不再起一个 SAGA
//
// 契约的描述写的是「触发 SAGA 回滚库存与优惠券」。下单是 SAGA，是因为它跨了
// 三个分支、每个分支各自一个事务，而且必须在「订单落库」与「SAGA 提交」之间
// 容忍进程死掉（order.go 的文件头）。取消没有这些前提：要动的三样东西
// （订单状态、库存、券）在同一个库里，一个本地事务就能让它们同生共死。
// 超时关单（sweep.go）早就是这么做的，而取消与超时关单**放回去的东西一模一样**，
// 差别只在「谁、凭什么把这一单关掉」。所以取消复用 sweep.go 的
// releaseClosedOrder，不另写一套回补 —— 两套各自演化，迟早有一套漏掉券。
//
// 再起一个 SAGA 的代价是真的：补偿分支、屏障、gid 文法、协调器可用性，
// 全都为了一件本地事务已经能原子完成的事。数据模型 §6 那句「超时关单用 SAGA」
// 与实现的出入早就存在（sweep.go 同样是本地事务），本轮在文档里一并写明。

var (
	// ErrOrderNotCancelable：这一单当前不能取消（只有 10 待支付可以）。
	// 契约：409 order-status-not-cancelable。
	ErrOrderNotCancelable = errors.New("订单当前状态不允许取消")

	// ErrOrderNotConfirmable：这一单当前不能确认收货（只有 30 已发货可以）。
	// 契约：409 order-status-not-confirmable。
	ErrOrderNotConfirmable = errors.New("订单当前状态不允许确认收货")

	// ErrOrderNotShippable：这一单当前不能发货（非 20 已支付，或已发过货）。
	// 契约：409 order-status-not-shippable。
	ErrOrderNotShippable = errors.New("订单当前状态不允许发货")

	// ErrOrderHasPendingFullRefund：这一单有一张未完结的整单退款申请
	// （订单停在 50 退款中）。契约：409 order-has-pending-full-refund。
	//
	// 与 ErrOrderNotShippable 分开，因为后台的处置不同：这一种要先去审那张退款单，
	// 那一种只是刷新一下订单。
	ErrOrderHasPendingFullRefund = errors.New("订单有未完结的整单退款申请，不能发货")

	// ErrTrackingNoDuplicated：运单号在本店已登记过。契约：409 tracking-no-duplicated。
	ErrTrackingNoDuplicated = errors.New("运单号已被登记过")

	// ErrShipmentBadRequest：承运商或运单号为空、或长得离谱。契约：422 invalid-request。
	ErrShipmentBadRequest = errors.New("发货参数不合法")
)

// 订单状态（数据模型 §5）。orderStatusPending（10）在 order.go 里，
// 其余的在这里一次给全，免得散落的字面量在比较里写岔。
const (
	orderStatusPaid           int16 = 20
	orderStatusShipped        int16 = 30
	orderStatusFinished       int16 = 40
	orderStatusRefunding      int16 = 50
	orderStatusRefunded       int16 = 60
	shipmentFieldMaxRunes           = 64
	idempotencyScopeCancel          = "orders.cancel"
	idempotencyScopeConfirm         = "orders.confirm"
	idempotencyScopeAdminShip       = "admin.orders.ship"
)

// Cancel 实现 POST /orders/{order_no}/cancel。
//
// 返回取消之后的订单（status 90）。第二个返回值为 true 表示这是一次幂等重放。
func (s *OrderService) Cancel(ctx context.Context, orderNo, idemKey string) (repository.Order, bool, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.Order{}, false, err
	}
	return idempotentTx(ctx, s.repo, repository.BuyerSubject(id.UserID),
		idempotencyScopeCancel, idemKey, pathHash(orderNo), archivedOK,
		func(tx repository.Tx) (repository.Order, error) {
			order, err := findBuyerOrder(ctx, tx, orderNo, id.UserID)
			if err != nil {
				return repository.Order{}, err
			}
			ok, err := tx.CancelPendingOrder(ctx, orderNo, id.UserID)
			if errors.Is(err, repository.ErrIllegalOrderTransition) {
				ok, err = false, nil
			}
			if err != nil {
				return repository.Order{}, err
			}
			if !ok {
				// 读到的状态是 order.Status，但它可能刚被支付回调改掉 ——
				// 真正的裁判是那条条件 UPDATE 的 rows_affected，这里只是给 detail
				// 一个大致的线索。
				return repository.Order{}, fmt.Errorf("%w: 订单 %s 当前状态是 %d，只有 %d 待支付可以取消",
					ErrOrderNotCancelable, orderNo, order.Status, orderStatusPending)
			}
			// 与超时关单同一份放回逻辑，只是流水记成「买家取消释放」（biz_type 6）。
			if _, err := releaseClosedOrder(ctx, tx, order.ID, order.OrderNo, order.StoreID,
				repository.InventoryLogBuyerCancel); err != nil {
				return repository.Order{}, err
			}
			return findBuyerOrder(ctx, tx, orderNo, id.UserID)
		})
}

// Confirm 实现 POST /orders/{order_no}/confirm。
func (s *OrderService) Confirm(ctx context.Context, orderNo, idemKey string) (repository.Order, bool, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.Order{}, false, err
	}
	return idempotentTx(ctx, s.repo, repository.BuyerSubject(id.UserID),
		idempotencyScopeConfirm, idemKey, pathHash(orderNo), archivedOK,
		func(tx repository.Tx) (repository.Order, error) {
			order, err := findBuyerOrder(ctx, tx, orderNo, id.UserID)
			if err != nil {
				return repository.Order{}, err
			}
			ok, err := tx.ConfirmOrderReceipt(ctx, orderNo, id.UserID)
			if errors.Is(err, repository.ErrIllegalOrderTransition) {
				ok, err = false, nil
			}
			if err != nil {
				return repository.Order{}, err
			}
			if !ok {
				return repository.Order{}, fmt.Errorf("%w: 订单 %s 当前状态是 %d，只有 %d 已发货可以确认收货",
					ErrOrderNotConfirmable, orderNo, order.Status, orderStatusShipped)
			}
			return findBuyerOrder(ctx, tx, orderNo, id.UserID)
		})
}

// findBuyerOrder 按单号取当前买家自己的订单，查不到（含不是他的）翻成 ErrOrderNotFound。
func findBuyerOrder(ctx context.Context, tx repository.Tx, orderNo string, userID int64) (repository.Order, error) {
	order, err := tx.FindUserOrderByNo(ctx, orderNo, userID)
	if errors.Is(err, repository.ErrOrderNotFound) {
		return repository.Order{}, fmt.Errorf("%w: order_no=%s", ErrOrderNotFound, orderNo)
	}
	return order, err
}

// ---------------------------------------------------------------------------
// 后台：发货
// ---------------------------------------------------------------------------

// AdminOrderService 实现后台的订单与售后写操作（发货、退款审核、确认收到退货）。
type AdminOrderService struct {
	repo tenantRunner
}

// NewAdminOrderService 建后台订单服务。
func NewAdminOrderService(r tenantRunner) *AdminOrderService {
	return &AdminOrderService{repo: r}
}

// ShipRequest 是 ShipmentCreateRequest 在 service 边界上的形状。
type ShipRequest struct {
	CarrierCode string
	TrackingNo  string
}

// Ship 实现 POST /admin/orders/{order_no}/shipments。
//
// # 谁能发货
//
// 契约的权限矩阵（StaffRole）里没有「发货」这一行。这里按「门店库存」那一行判
// （authorizeOrderStore，与 authorizeStore 的 storeOperate 同一个判据）：管理员与操作员全店都能发；大区管理员只能发
// 本大区门店的单；门店管理员只能发自己门店的单。理由是履约与库存是同一件事的
// 两面 —— 货从哪家店出，就该由管那家店库存的人发，一个能改 A 店库存却不能发 A 店
// 货的角色，或者反过来，都说不出道理。
//
// # 三条规则（§5）
//
//	一、不动库存 —— 这里一行 inventories 都不碰。
//	二、有未完结的整单退款时拒绝 —— 订单停在 50 就是那个信号（整单退款申请把
//	    订单推到 50，驳回 / 撤回把它推回 20）。部分退款不改 status，所以不阻断。
//	三、自动确认收货 —— 不在这里，在定时任务里（auto_confirm.go）：发货满
//	    shop_settings.auto_confirm_days 天（默认 7）由系统替买家确认收货。
func (s *AdminOrderService) Ship(ctx context.Context, orderNo string, req ShipRequest,
	idemKey string) (repository.Shipment, bool, error) {

	staff, err := requireStaff(ctx)
	if err != nil {
		return repository.Shipment{}, false, err
	}
	req.CarrierCode = strings.TrimSpace(req.CarrierCode)
	req.TrackingNo = strings.TrimSpace(req.TrackingNo)
	for name, v := range map[string]string{"carrier_code": req.CarrierCode, "tracking_no": req.TrackingNo} {
		if v == "" || utf8.RuneCountInString(v) > shipmentFieldMaxRunes {
			return repository.Shipment{}, false, fmt.Errorf("%w: %s 不能为空，也不能超过 %d 个字符",
				ErrShipmentBadRequest, name, shipmentFieldMaxRunes)
		}
	}

	hash := pathHash(orderNo, req.CarrierCode, req.TrackingNo)
	return idempotentTx(ctx, s.repo, repository.StaffSubject(staff.StaffID),
		idempotencyScopeAdminShip, idemKey, hash, archivedCreated,
		func(tx repository.Tx) (repository.Shipment, error) {
			order, err := findAdminOrder(ctx, tx, orderNo)
			if err != nil {
				return repository.Shipment{}, err
			}
			if _, err := authorizeOrderStore(ctx, tx, order.StoreID); err != nil {
				return repository.Shipment{}, err
			}
			switch {
			case order.Status == orderStatusRefunding:
				return repository.Shipment{}, fmt.Errorf("%w: 订单 %s 停在 50 退款中",
					ErrOrderHasPendingFullRefund, orderNo)
			case order.Status != orderStatusPaid:
				return repository.Shipment{}, fmt.Errorf("%w: 订单 %s 当前状态是 %d，只有 %d 已支付可以发货",
					ErrOrderNotShippable, orderNo, order.Status, orderStatusPaid)
			}
			ok, err := tx.ShipOrder(ctx, order.ID)
			if errors.Is(err, repository.ErrIllegalOrderTransition) {
				ok, err = false, nil
			}
			if err != nil {
				return repository.Shipment{}, err
			}
			if !ok {
				// 读的时候还是 20，推的时候已经不是了：并发的另一次发货、或者一张整单
				// 退款申请刚把它推到 50。按「不能发货」报，客户端刷新之后会看到原因。
				return repository.Shipment{}, fmt.Errorf("%w: 订单 %s 在发货的同时被改了状态",
					ErrOrderNotShippable, orderNo)
			}
			sh, err := tx.InsertShipment(ctx, repository.NewShipment{
				OrderID:     order.ID,
				CarrierCode: req.CarrierCode,
				TrackingNo:  req.TrackingNo,
				CreatedBy:   staff.StaffID,
			})
			if errors.Is(err, repository.ErrTrackingNoDuplicated) {
				return repository.Shipment{}, fmt.Errorf("%w: %v", ErrTrackingNoDuplicated, err)
			}
			if err != nil {
				return repository.Shipment{}, err
			}
			// 通知与 20 → 30 同一个事务（数据模型 §16）。
			return sh, notifyOrderShipped(ctx, tx, order, req.CarrierCode, req.TrackingNo)
		})
}

// findAdminOrder 按单号取本租户的订单（后台没有买家过滤，租户由 RLS 管）。
// status 0 创建中的订单对后台同样不存在：它要么被推到 10，要么被补偿关掉。
func findAdminOrder(ctx context.Context, tx repository.Tx, orderNo string) (repository.Order, error) {
	order, err := tx.FindOrderByNo(ctx, orderNo)
	if errors.Is(err, repository.ErrOrderNotFound) || (err == nil && order.Status == orderStatusDraft) {
		return repository.Order{}, fmt.Errorf("%w: order_no=%s", ErrOrderNotFound, orderNo)
	}
	return order, err
}

// pathHash 是这几条接口的 request_hash：路径上的单号加上请求体里决定语义的字段。
//
// 单号必须进哈希，理由同 intentHash：同一把钥匙拿去取消**另一笔**订单，
// 不能拿到上一笔的存档。各段之间用 \x00 隔开 —— 单号与运单号里都不会出现它，
// 于是 ("ab","c") 与 ("a","bc") 不会撞成同一个哈希。
func pathHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
