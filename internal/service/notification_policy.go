package service

import "sort"

// notifyPolicy 是一处状态变化调用点对通知的决定：发（Notify 点名 notifyXxx）或不发（Silent 写理由）。
type notifyPolicy struct {
	Notify string
	Silent string
}

// notificationCallSites 登记 service 里每一处状态变化调用（键是「所在函数/repository 方法」）
// 发什么通知，或为什么刻意不发。notification_policy_test.go 从 db/queries 枚举状态变化语句、
// 找到每一处调用逐一核对这张表（文件头见 notification.go 第二节）。
var notificationCallSites = map[string]notifyPolicy{
	// —— 订单履约维度
	"PaymentService.settle/SettleOrder":                 {Notify: "notifyOrderPaid"},
	"settleFreeOrder/SettleOrder":                       {Notify: "notifyOrderPaid"},
	"AdminOrderService.Ship/ShipOrder":                  {Notify: "notifyOrderShipped"},
	"AutoConfirmService.confirmOne/ConfirmOrderReceipt": {Notify: "notifyOrderAutoFinished"},
	"AutoConfirmService.confirmOne/FinishChannelOrder":  {Silent: "渠道单（00320）无 keel 买家，平台自己通知顾客"},
	"OrderService.Confirm/ConfirmOrderReceipt": {Silent: "买家自己点的确认收货：动作是他做的，" +
		"响应里就是完成后的订单，不需要再发一条「订单已完成」告诉他"},
	"SweepService.releasePending/ClaimExpiredPendingOrder": {Notify: "notifyOrderTimeoutClosed"},
	"OrderService.Cancel/CancelPendingOrder":               {Silent: "买家自己取消：动作是他做的，响应里就是关闭后的订单"},
	"promoteOrder/PromoteOrderDraft": {Silent: "下单 SAGA 的建单分支（0 → 10）：买家正在同步等 POST /orders 的结果，" +
		"「下单成功」由那次响应告诉他；此刻订单号还没对外返回（order_saga.go 的 closeOrder 注释）"},
	"closeOrder/CloseOrder": {Silent: "下单 SAGA 的全局补偿（0/10 → 90）：POST /orders 同步回的是失败（库存不足、券不可用），" +
		"那一单从没对买家「存在」过"},
	// 渠道单（第三期）的接单 SAGA：channel_order_saga.go。
	"ChannelService.channelOrderOpenBranch/PromoteOrderDraft": {Silent: "渠道单接单 SAGA 的建单分支（0 → 10）：" +
		"渠道单无 keel 买家，平台自己通知顾客；门店的「新订单待发货」在收尾分支 10 → 20 时发，这里再发就是同一件事说两遍"},
	"ChannelService.channelOrderOpenUndoBranch/CloseOrder": {Notify: "notifyChannelOrderException"},
	"ChannelService.channelOrderFinishBranch/SettleOrder":  {Notify: "notifyOrderPaid"},
	"SweepService.closeDraft/CloseExpiredDraftOrder": {Silent: "孤儿草稿（0 → 90）：进程在建单与 SAGA 之间死掉留下的，" +
		"买家那次下单已经拿到了失败或超时，订单号从没对外返回过"},

	// —— 售后
	"RefundService.Create/InsertRefund":          {Notify: "notifyRefundRequested"},
	"RefundService.Create/StartWholeOrderRefund": {Notify: "notifyRefundRequested"},
	"RefundService.Audit/ApproveRefund":          {Notify: "notifyRefundApproved"},
	"RefundService.Audit/RejectRefund":           {Notify: "notifyRefundRejected"},
	"RefundService.Cancel/CancelRefund": {Silent: "买家自己撤回（10/20 → 60）：动作是他做的。" +
		"门店那条「新的售后待审核」不撤回 —— 点进去看到的是已取消，比一条凭空消失的提醒好懂"},
	"leaveRefunding/RevertWholeOrderRefund": {Silent: "订单 50 → 20 是驳回 / 撤回 / 退货超时关闭的附随动作，" +
		"通知由那三条自己决定（驳回发 refund_rejected，撤回不发，超时关闭发 refund_return_expired），" +
		"这里再发就是同一件事说两遍"},
	"ReturnTimeoutService.expireOne/ExpireReturnRefund":       {Notify: "notifyRefundReturnExpired"},
	"RefundService.SubmitReturnShipment/SubmitReturnShipment": {Notify: "notifyReturnShipped"},
	"RefundService.Receive/ReceiveRefundGoods": {Silent: "商家确认收到退货（20 → 30）：紧接着就是退款入账，" +
		"沙箱在同一个事务里入到 40 并发 refund_succeeded；真渠道回调到达时同样会发。" +
		"「商家收到货了」这一步单独再发一条，买家会在几秒内收到两条说的是同一件事的消息"},
	"RefundService.settleTx/CompleteRefund":         {Notify: "notifyRefundSucceeded"},
	"RefundService.settleTx/FinishWholeOrderRefund": {Notify: "notifyRefundSucceeded"},
	"RefundService.settleTx/RecordRefundNotify": {Silent: "退款回调金额对不上：状态不动，只留原始报文，" +
		"是要人对账的异常，不是买家该收到的消息（日志里有 Error）"},

	// —— 库存水位
	// 下单扣减与补偿是库存服务的 SAGA 分支（inventory 包 saga.go，由协调器调用，service 里没有调用点）；
	// 扣减的库存预警由 core 的收尾分支 OrderService.finishBranch 按库存流水发（notifyLowStockIfCrossed，
	// notification_policy_test.go 单独核对它真的调了）。
	"inventoryOutbox.run/ReleaseForOrder": {Silent: "关单 / 取消之后的 outbox 任务放回库存与活动配额（升高，不会跌破预警线）。" +
		"超时关单那条路的通知在 releasePending 里（order_timeout_closed，与关单同一个事务），买家取消那条路不发"},
	"inventoryOutbox.run/RestockForRefund": {Silent: "退款到账后的 outbox 任务把未发货的货加回门店库存（升高）；" +
		"这一次状态变化的通知是 settleTx 里的 refund_succeeded（与退款到账同一个事务）"},
	// 后台改库存四处（微服务拆分阶段 1a 起经库存服务 inventory.Service 写，登记的是 service 里
	// 调 inv.Set / inv.Adjust / inv.InitSKUs 的那一处，见 notification_policy_test.go）。
	"setStockSole/Set": {Silent: "后台手工改库存（单店捷径）：动作是商家自己做的，" +
		"改到预警线以下时他正看着那个数"},
	"setStockInStore/Set":   {Silent: "后台按门店改库存：商家自己做的，理由同 setStockSole"},
	"initSKUStock/InitSKUs": {Silent: "建 SKU 时写初始库存：商家自己做的，理由同 setStockSole"},
	"adjustInventory/Adjust": {Silent: "后台相对调整库存（进货 / 盘亏 / 验货入库，两条路径共用）：" +
		"商家自己做的，理由同 setStockSole —— 扣到预警线以下时他正看着那个数"},

	// —— 营销活动的配额（00075 起在库存服务）与每人限购（core）
	"promoteOrder/ReservePromotionLimit": {Silent: "建单分支累计每人限购：超限是正常的业务结局，" +
		"买家由 POST /orders 同步收到 409 promotion-limit-exceeded，不是要人处理的事"},
	"releasePromotionLimits/ReleasePromotionLimit": {Silent: "建单补偿、超时关单、买家取消时放回每人限购（升高）；" +
		"这几条路径的通知由各自的调用方决定（超时关单发 order_timeout_closed，另两条不发）"},
	"AdminPromotionService.syncQuotas/SetActivityQuotas": {Silent: "后台改活动商品时整组设配额：商家自己做的，" +
		"理由同 setStockSole；秒杀抢光由下单时的 409 promotion-sold-out 告诉买家"},
	"ChannelService.syncCatalogItem/InitSKUs": {Silent: "商品源（Shopify）拉进来的新 SKU 写初始库存：取的是商品源上那一刻的现货，" +
		"商家自己接的渠道；与批量导入同一个理由，不是买家或员工要知道的变化"},
	"ProductImportService.initImportStock/InitSKUs": {Silent: "批量导入时建 SKU 写初始库存：商家自己确认的导入，" +
		"理由同 initSKUStock；而且导入的商品是草稿，买家看不见，库存高低与任何人的订单无关"},
}

// stateEdges 登记状态机的每一条边由哪条语句走（order:/refund: 前缀，与迁移里的
// order_status_transitions / refund_status_transitions 逐条对应）。
var stateEdges = map[string][]string{
	"order:0->10":  {"PromoteOrderDraft"},
	"order:0->90":  {"CloseOrder", "CloseExpiredDraftOrder"},
	"order:10->20": {"SettleOrder"},
	"order:10->90": {"CancelPendingOrder", "ClaimExpiredPendingOrder", "CloseOrder"},
	"order:20->30": {"ShipOrder"},
	"order:20->50": {"StartWholeOrderRefund"},
	"order:30->40": {"ConfirmOrderReceipt", "FinishChannelOrder"},
	"order:50->20": {"RevertWholeOrderRefund"},
	"order:50->60": {"FinishWholeOrderRefund"},

	"refund:10->20": {"ApproveRefund"},
	"refund:10->30": {"ApproveRefund"},
	"refund:10->50": {"RejectRefund"},
	"refund:10->60": {"CancelRefund"},
	"refund:20->30": {"ReceiveRefundGoods"},
	"refund:20->60": {"CancelRefund", "ExpireReturnRefund"},
	"refund:30->40": {"CompleteRefund"},
}

// NotificationKinds 返回全部通知种类（排好序）。给 handler 那边与契约枚举对账用。
func NotificationKinds() []string {
	out := make([]string, 0, len(notificationTemplates))
	for k := range notificationTemplates {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
