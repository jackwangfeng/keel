package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// 消息通知的**写入**那一半（数据模型 §16）：事件 → 模板渲染 → 同一个事务里落
// notifications + 入外发任务。读的那一半在 notification_center.go，外发投递在
// notification_delivery.go。
//
// ===========================================================================
// 一、可靠性：只有一个写入口，而它只收业务事务的 tx
// ===========================================================================
//
// 每一个 notifyXxx 都收调用方的 repository.Tx —— 做状态变化的那个事务。
// 它们不开自己的事务，也拿不到开事务的东西（这个文件里没有 WithTenant）。
// 于是「状态改了通知丢了」与「回滚了通知却发了」在形状上就不存在：
// 通知行与外发任务要么跟着业务一起提交，要么跟着一起回滚。
//
// 行为由 internal/handler/notification_test.go 的两条回滚用例钉住
// （在提交时刻让事务失败：通知不许留下；业务写不许留下）。
//
// ===========================================================================
// 二、哪些状态变化发通知 —— 登记在 notificationCallSites，由测试从源头核对
// ===========================================================================
//
// notification_policy_test.go 从 db/queries 里枚举每一条改订单 / 退款单状态
// （以及扣库存、填寄回物流）的语句，找到 service 里每一处调它的地方，
// 要求每一处都在 notificationCallSites 里登记：要么点名发哪个 notifyXxx
// （而且那个函数里真的调了它），要么写明为什么刻意不发。新加一条状态迁移而
// 忘了决定发不发，那条测试当场红。
//
// ===========================================================================
// 三、模板在服务端渲染
// ===========================================================================
//
// 标题与正文在写入时按 notificationTemplates 渲染好、落进行里，客户端原样展示。
// 写入时渲染而不是读取时渲染：通知是「那一刻发生了什么」的快照 ——
// 三天后商品改了名、店铺改了运单号，那条「已发货」说的仍然是当时的事。

// 通知种类（契约 NotificationKind，逐字一致；notification_policy_test.go 核对）。
const (
	KindOrderPaid               = "order_paid"
	KindOrderShipped            = "order_shipped"
	KindOrderAutoConfirmSoon    = "order_auto_confirm_soon"
	KindOrderFinished           = "order_finished"
	KindOrderTimeoutClosed      = "order_timeout_closed"
	KindRefundApproved          = "refund_approved"
	KindRefundRejected          = "refund_rejected"
	KindRefundSucceeded         = "refund_succeeded"
	KindRefundReturnExpired     = "refund_return_expired"
	KindMerchantOrderPaid       = "merchant_order_paid"
	KindMerchantRefundRequest   = "merchant_refund_requested"
	KindMerchantReturnShipped   = "merchant_return_shipped"
	KindMerchantInventoryLow    = "merchant_inventory_low"
	notificationTargetOrder     = "order"
	notificationTargetRefund    = "refund"
	notificationTargetInventory = "inventory"
)

// notifyParams 是模板能用到的全部字段。一个结构体而不是每种一个：
// 模板只按名字取字段，多出来的不碍事，而一张表比十二个类型好读。
type notifyParams struct {
	OrderNo      string
	RefundNo     string
	AmountCents  int64
	CarrierCode  string
	TrackingNo   string
	Reason       string
	RefundType   int16
	Days         int
	ProductTitle string
	SpecLabel    string
	StoreName    string
	Left         int32
	Warning      int32
}

// notificationTemplate 是一种通知的全部静态属性：发给谁、点了跳哪、标题与正文怎么写。
type notificationTemplate struct {
	Audience int16
	Target   string
	Title    string
	Body     string
}

// notificationTemplates 是全部模板。改文案只改这里；契约里说「服务端渲染」指的就是这张表。
//
// 用 text/template 而不是 html/template：这是纯文本，由客户端当文本展示，
// html 转义会把驳回理由里的「<」变成 &lt; 原样显示给买家。
var notificationTemplates = map[string]notificationTemplate{
	KindOrderPaid: {repository.NotificationAudienceBuyer, notificationTargetOrder,
		"支付成功",
		"订单 {{.OrderNo}} 已支付 {{yuan .AmountCents}}，商家正在备货。"},
	KindOrderShipped: {repository.NotificationAudienceBuyer, notificationTargetOrder,
		"订单已发货",
		"订单 {{.OrderNo}} 已发货，{{carrier .CarrierCode}}运单号 {{.TrackingNo}}。"},
	KindOrderAutoConfirmSoon: {repository.NotificationAudienceBuyer, notificationTargetOrder,
		"即将自动确认收货",
		"订单 {{.OrderNo}} 将在约 24 小时后自动确认收货（发货满 {{.Days}} 天自动确认）。" +
			"如商品有问题，请在此之前申请售后。"},
	KindOrderFinished: {repository.NotificationAudienceBuyer, notificationTargetOrder,
		"订单已完成",
		"订单 {{.OrderNo}} 发货已满 {{.Days}} 天，系统已自动确认收货，交易完成。"},
	KindOrderTimeoutClosed: {repository.NotificationAudienceBuyer, notificationTargetOrder,
		"订单已关闭",
		"订单 {{.OrderNo}} 超时未支付，已自动关闭。"},
	KindRefundApproved: {repository.NotificationAudienceBuyer, notificationTargetRefund,
		"售后审核通过",
		"{{if eq .RefundType 2}}售后单 {{.RefundNo}} 已通过审核，请在售后详情里填写退货寄回的物流信息。" +
			"{{else}}售后单 {{.RefundNo}} 已通过审核，{{yuan .AmountCents}} 将原路退回。{{end}}"},
	KindRefundRejected: {repository.NotificationAudienceBuyer, notificationTargetRefund,
		"售后申请未通过",
		"售后单 {{.RefundNo}} 未通过审核，理由：{{.Reason}}"},
	KindRefundSucceeded: {repository.NotificationAudienceBuyer, notificationTargetRefund,
		"退款已到账",
		"售后单 {{.RefundNo}} 的退款 {{yuan .AmountCents}} 已原路退回。"},
	KindRefundReturnExpired: {repository.NotificationAudienceBuyer, notificationTargetRefund,
		"售后已关闭",
		"售后单 {{.RefundNo}} 审核通过后 {{.Days}} 天内没有填写退货寄回物流，已自动关闭。" +
			"如仍需售后，可以重新申请。"},
	KindMerchantOrderPaid: {repository.NotificationAudienceMerchant, notificationTargetOrder,
		"新订单待发货",
		"订单 {{.OrderNo}} 已支付 {{yuan .AmountCents}}，请及时发货。"},
	KindMerchantRefundRequest: {repository.NotificationAudienceMerchant, notificationTargetRefund,
		"新的售后待审核",
		"订单 {{.OrderNo}} 申请{{refundType .RefundType}} {{yuan .AmountCents}}（售后单 {{.RefundNo}}）。"},
	KindMerchantReturnShipped: {repository.NotificationAudienceMerchant, notificationTargetRefund,
		"买家已寄回退货",
		"售后单 {{.RefundNo}}：买家已寄回，{{carrier .CarrierCode}}运单号 {{.TrackingNo}}。" +
			"收到货后请在售后页确认收货。"},
	KindMerchantInventoryLow: {repository.NotificationAudienceMerchant, notificationTargetInventory,
		"{{if eq .Left 0}}商品已售罄{{else}}库存预警{{end}}",
		"{{.ProductTitle}}{{.SpecLabel}} 在{{.StoreName}}剩 {{.Left}} 件（预警线 {{.Warning}} 件）。"},
}

// carrierNames 是常见承运商代码的中文名。认不出的原样显示代码 —— 发货时填什么
// 是商家的自由（契约 carrier_code 只是「承运商标识」），这里不做校验。
var carrierNames = map[string]string{
	"sf": "顺丰", "jd": "京东", "yto": "圆通", "zto": "中通", "sto": "申通",
	"yd": "韵达", "ems": "EMS", "jt": "极兔", "db": "德邦",
}

var notificationFuncs = template.FuncMap{
	// yuan 把分写成「¥12.30」。整数运算，不经浮点。
	"yuan": func(cents int64) string {
		sign := ""
		if cents < 0 {
			sign, cents = "-", -cents
		}
		return fmt.Sprintf("%s¥%d.%02d", sign, cents/100, cents%100)
	},
	"carrier": func(code string) string {
		if name, ok := carrierNames[strings.ToLower(strings.TrimSpace(code))]; ok {
			return name
		}
		return code + " "
	},
	"refundType": func(t int16) string {
		if t == repository.RefundTypeReturnGoods {
			return "退货退款"
		}
		return "仅退款"
	},
}

type compiledTemplate struct {
	notificationTemplate
	title, body *template.Template
}

// compiledTemplates 在包初始化时解析全部模板：写错一个括号是启动即 panic，
// 而不是等到那一种事件第一次发生时才在一个业务事务里报错、把那次状态变化一起回滚。
var compiledTemplates = func() map[string]compiledTemplate {
	out := make(map[string]compiledTemplate, len(notificationTemplates))
	for kind, t := range notificationTemplates {
		out[kind] = compiledTemplate{
			notificationTemplate: t,
			title:                template.Must(template.New(kind + ".title").Funcs(notificationFuncs).Parse(t.Title)),
			body:                 template.Must(template.New(kind + ".body").Funcs(notificationFuncs).Parse(t.Body)),
		}
	}
	return out
}()

// renderNotification 按种类渲染标题与正文。
func renderNotification(kind string, p notifyParams) (compiledTemplate, string, string, error) {
	t, ok := compiledTemplates[kind]
	if !ok {
		return compiledTemplate{}, "", "", fmt.Errorf("没有通知种类 %q 的模板", kind)
	}
	var title, body strings.Builder
	if err := t.title.Execute(&title, p); err != nil {
		return compiledTemplate{}, "", "", err
	}
	if err := t.body.Execute(&body, p); err != nil {
		return compiledTemplate{}, "", "", err
	}
	return t, title.String(), body.String(), nil
}

// outgoing 是一条要写的通知：种类、收件人、定位、参数、去重后缀。
type outgoing struct {
	Kind string
	// UserID 发给买家时用。渠道单（00320）没有 keel 买家，为 nil：买家通知不发，
	// 平台自己通知顾客（emitNotification 里跳过）；员工侧通知照发。
	UserID  *int64
	StoreID int64 // 发给商家时；库存预警的定位也用它
	SKUID   int64 // 库存预警的定位
	Params  notifyParams
	// Dedupe 是去重键在种类之后的那一段，通常是单号。
	Dedupe string
}

// deliveryPayload 是外发任务的 payload：只放通知 id（jobs 没有 RLS，payload 越薄越好，
// 理由见 00022 文件头第一节第 ③ 条）。
type deliveryPayload struct {
	NotificationID int64 `json:"notification_id"`
}

// emitNotification 是**唯一**写通知的地方：渲染 → 落行 → （有渠道可投时）入外发任务，
// 全在调用方的 tx 里。
//
// 去重键撞上（同一件事已经通知过）时什么都不做、返回 nil —— 那是正常路径。
//
// ===========================================================================
// 站内消息照写，外发任务按「有没有渠道配置」决定入不入队
// ===========================================================================
//
// notifications 那一行（站内消息本身：买家消息中心、后台铃铛）无条件写 ——
// 它不依赖任何外发渠道，本来就不该受渠道配置影响。只有 notification.deliver
// 这个外发任务，在 anyChannelConfigured() 为假时才不入队：一个渠道都没配时，
// 入队换来的是 worker 必然把它投成「全部跳过」，而这件事从通知种类上就能
// 确定，不需要真跑一次 worker 才知道。理由与取舍的完整论证见
// notification_delivery.go 里 anyChannelConfigured 上面那段。
func emitNotification(ctx context.Context, tx repository.Tx, o outgoing) error {
	t, title, body, err := renderNotification(o.Kind, o.Params)
	if err != nil {
		return err
	}
	n := repository.NewNotification{
		Audience: t.Audience, Kind: o.Kind, Title: title, Body: body,
		TargetType: t.Target, DedupeKey: o.Kind + ":" + o.Dedupe,
	}
	if t.Audience == repository.NotificationAudienceBuyer {
		if o.UserID == nil {
			return nil // 渠道单无 keel 买家，平台自己通知顾客
		}
		n.UserID = o.UserID
	} else {
		n.StoreID = &o.StoreID
	}
	if o.Params.OrderNo != "" {
		n.OrderNo = &o.Params.OrderNo
	}
	if o.Params.RefundNo != "" {
		n.RefundNo = &o.Params.RefundNo
	}
	if t.Target == notificationTargetInventory {
		n.SKUID = &o.SKUID
	}
	id, inserted, err := tx.InsertNotification(ctx, n)
	if err != nil || !inserted {
		return err
	}
	if !anyChannelConfigured() {
		// 站内消息已经落地；没有渠道可投，连任务都不入队——不是「入队之后
		// worker 发现没渠道」，是根本不产生这条任务。
		return nil
	}
	payload, err := json.Marshal(deliveryPayload{NotificationID: id})
	if err != nil {
		return err
	}
	_, err = tx.EnqueueJob(ctx, repository.NewJob{
		Queue:   repository.QueueNotificationDelivery,
		JobKey:  "notification:" + strconv.FormatInt(id, 10),
		Payload: payload,
	})
	return err
}

// ---------------------------------------------------------------------------
// 事件：每一个都在状态变化的那个事务里被调一次
// ---------------------------------------------------------------------------

// notifyOrderPaid 支付成功：告诉买家，同时给履约门店一条「新订单待发货」。
func notifyOrderPaid(ctx context.Context, tx repository.Tx, order repository.Order) error {
	p := notifyParams{OrderNo: order.OrderNo, AmountCents: order.PayableCents}
	if err := emitNotification(ctx, tx, outgoing{Kind: KindOrderPaid, UserID: order.UserID,
		Params: p, Dedupe: order.OrderNo}); err != nil {
		return err
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindMerchantOrderPaid, StoreID: order.StoreID,
		Params: p, Dedupe: order.OrderNo})
}

// notifyOrderShipped 已发货，正文带物流。
func notifyOrderShipped(ctx context.Context, tx repository.Tx, order repository.Order, carrier, tracking string) error {
	return emitNotification(ctx, tx, outgoing{Kind: KindOrderShipped, UserID: order.UserID,
		Params: notifyParams{OrderNo: order.OrderNo, CarrierCode: carrier, TrackingNo: tracking},
		Dedupe: order.OrderNo})
}

// notifyOrderTimeoutClosed 超时未支付被系统关单。清扫路径手里只有单号，买家从订单行上读。
func notifyOrderTimeoutClosed(ctx context.Context, tx repository.Tx, orderNo string) error {
	order, err := tx.FindOrderByNo(ctx, orderNo)
	if err != nil {
		return err
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindOrderTimeoutClosed, UserID: order.UserID,
		Params: notifyParams{OrderNo: order.OrderNo}, Dedupe: order.OrderNo})
}

// notifyOrderAutoFinished 系统替买家确认收货。买家自己点的那一次不走这里（动作是他做的）。
func notifyOrderAutoFinished(ctx context.Context, tx repository.Tx, order repository.Order, days int) error {
	return emitNotification(ctx, tx, outgoing{Kind: KindOrderFinished, UserID: order.UserID,
		Params: notifyParams{OrderNo: order.OrderNo, Days: days}, Dedupe: order.OrderNo})
}

// notifyAutoConfirmSoon 自动确认收货即将到期。去重键的形状与
// db/queries/notifications.sql 的 ListAutoConfirmReminders 里那一个逐字一致。
func notifyAutoConfirmSoon(ctx context.Context, tx repository.Tx, r repository.AutoConfirmReminder, days int) error {
	return emitNotification(ctx, tx, outgoing{Kind: KindOrderAutoConfirmSoon, UserID: r.UserID,
		Params: notifyParams{OrderNo: r.OrderNo, Days: days}, Dedupe: r.OrderNo})
}

// notifyRefundRequested 新的售后申请：给履约门店一条待审核提醒。
func notifyRefundRequested(ctx context.Context, tx repository.Tx, refundNo string) error {
	r, err := tx.FindRefundByNo(ctx, refundNo)
	if err != nil {
		return err
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindMerchantRefundRequest, StoreID: r.StoreID,
		Params: notifyParams{OrderNo: r.OrderNo, RefundNo: r.RefundNo, AmountCents: r.AmountCents,
			RefundType: r.RefundType},
		Dedupe: r.RefundNo})
}

// notifyRefundApproved 审核通过。金额读审核之后的那一版（退货退款的运费可能刚被裁定过）。
func notifyRefundApproved(ctx context.Context, tx repository.Tx, refundNo string) error {
	r, err := tx.FindRefundByNo(ctx, refundNo)
	if err != nil {
		return err
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindRefundApproved, UserID: &r.UserID,
		Params: notifyParams{OrderNo: r.OrderNo, RefundNo: r.RefundNo, AmountCents: r.AmountCents,
			RefundType: r.RefundType},
		Dedupe: r.RefundNo})
}

// notifyRefundRejected 审核驳回，正文带理由。
func notifyRefundRejected(ctx context.Context, tx repository.Tx, refundNo string) error {
	r, err := tx.FindRefundByNo(ctx, refundNo)
	if err != nil {
		return err
	}
	reason := ""
	if r.RejectReason != nil {
		reason = *r.RejectReason
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindRefundRejected, UserID: &r.UserID,
		Params: notifyParams{OrderNo: r.OrderNo, RefundNo: r.RefundNo, Reason: reason},
		Dedupe: r.RefundNo})
}

// notifyReturnShipped 买家填了（或改了）寄回物流。改一次单号就是一条新提醒 ——
// 去重键带着承运商与运单号，同一个单号重复提交不重复提醒。
func notifyReturnShipped(ctx context.Context, tx repository.Tx, refundNo, carrier, tracking string) error {
	r, err := tx.FindRefundByNo(ctx, refundNo)
	if err != nil {
		return err
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindMerchantReturnShipped, StoreID: r.StoreID,
		Params: notifyParams{OrderNo: r.OrderNo, RefundNo: r.RefundNo, CarrierCode: carrier, TrackingNo: tracking},
		Dedupe: r.RefundNo + ":" + carrier + ":" + tracking})
}

// notifyRefundReturnExpired 退货退款审核通过后超过店铺设置的天数没填寄回物流，
// 售后单被定时任务关到 60（return_timeout.go）。一张单只会关一次，去重键就是售后单号。
func notifyRefundReturnExpired(ctx context.Context, tx repository.Tx, refundNo string, days int) error {
	r, err := tx.FindRefundByNo(ctx, refundNo)
	if err != nil {
		return err
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindRefundReturnExpired, UserID: &r.UserID,
		Params: notifyParams{OrderNo: r.OrderNo, RefundNo: r.RefundNo, Days: days},
		Dedupe: r.RefundNo})
}

// notifyRefundSucceeded 退款到账。
func notifyRefundSucceeded(ctx context.Context, tx repository.Tx, r repository.Refund) error {
	return emitNotification(ctx, tx, outgoing{Kind: KindRefundSucceeded, UserID: &r.UserID,
		Params: notifyParams{OrderNo: r.OrderNo, RefundNo: r.RefundNo, AmountCents: r.AmountCents},
		Dedupe: r.RefundNo})
}

// notifyLowStockIfCrossed 下单扣减之后，这一行库存如果**这一次**从预警线之上跌到了
// 预警线或以下（before > warning >= after），给这家门店一条库存预警。
//
// 只在跨线的那一次发：已经在线下的库存每卖一件都提醒一次，铃铛会被同一件商品刷屏。
// 补货回到线上之后再跌下来，是新的一次（去重键带着触发它的订单号）。
//
// ### 微服务拆分阶段 1b：判据来自库存服务的流水，发在 core 的收尾分支里
//
// 拆分前它跑在库存分支的屏障事务里，before / after 就是那一次扣减的前后水位。拆分后扣减在库存
// 服务里，dtmrs 又不把分支的响应带回给提交方，于是由排在库存分支之后的收尾分支
// （order_saga.go 的 finishBranch）向库存服务要这一单的流水：扣减那一行记着**这一次**扣减的
// 前后水位（行锁之下的精确值，不是事后读的近似），预警线取那一行库存此刻的值。所以「每次跨线
// 恰好一单报」照旧成立 —— 同一行库存上的扣减在行锁下排队，前后水位首尾相接，一条预警线只会
// 落在其中一单的 (after, before] 里；去重键（门店:SKU:订单号）照旧，收尾分支的重试不会重复发。
// 唯一的偏差是预警线本身在扣减与收尾之间（毫秒级）被后台改了，那时按新的线判。
//
// 这一单之后若被全局补偿（库存回补），这条预警不撤回：它说的是「刚才跌破过」，
// 而补偿回来的那几件在下一次跨线时会再报。
func notifyLowStockIfCrossed(ctx context.Context, tx repository.Tx, order repository.Order,
	e inventory.TrailEntry) error {
	if !(e.Before > e.Warning && e.After <= e.Warning) {
		return nil
	}
	a, err := tx.LowStockContext(ctx, e.SKUID, e.StoreID)
	if err != nil {
		return err
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindMerchantInventoryLow,
		StoreID: e.StoreID, SKUID: e.SKUID,
		Params: notifyParams{ProductTitle: a.ProductTitle, SpecLabel: specLabel(a.SpecValues),
			StoreName: a.StoreName, Left: e.After, Warning: e.Warning},
		Dedupe: strconv.FormatInt(e.StoreID, 10) + ":" + strconv.FormatInt(e.SKUID, 10) + ":" + order.OrderNo})
}

// specLabel 把 skus.spec_values（{"颜色":"黑","尺码":"M"}）写成「（黑 / M）」，按键名排序；
// 解不开或为空时返回空串。
func specLabel(raw string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]string, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, fmt.Sprint(m[k]))
	}
	return "（" + strings.Join(vals, " / ") + "）"
}
