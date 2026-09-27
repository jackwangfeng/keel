package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 支付渠道异步回调（Task 7）：POST /webhooks/payments/{channel}。
//
// # 这条接口最要紧的两个问题，都不是业务问题
//
// 它是这个系统里**唯一一条未认证的写接口**（契约里 `security: []`，因为调用方
// 是支付渠道，它没有也不可能有我们的令牌）。于是有两件事必须先答清楚。
//
// ## 一、它怎么确定租户
//
// **和别的每一条接口一模一样：Host。没有破例。**
//
// 渠道回调的 URL 是商家自己在渠道后台登记的，每家店登记的是自己的
// `{code}.{base_domain}`（或自己的自定义域名）。所以这条路由挂在 v1 组里，
// 和 /products、/orders 走同一道 res.Middleware()。
//
// internal/tenant/resolver.go 写着「刻意不支持用请求头指定租户 —— 公开接口没有
// 鉴权，那等于让调用方自己声明它是哪家店」。这条接口正是那句话里说的「公开接口」，
// 所以它是那条规矩最该守的地方，不是该破例的地方。往报文里放一个 merchant_id
// 或者加一个 X-Merchant 头，就是把「我是哪家店」这个判断交给了一个未认证的调用方。
//
// ## 二、它怎么防伪造
//
// Host 是调用方能控制的（任何人都能往我们的 IP 上发一个带 shop-b 的 Host），
// 所以 Host **只回答「哪家店」，不回答「这是不是真的」**。后者靠验签：
//
//	签名 = hex(HMAC-SHA256(这家店在这个渠道上的密钥, 原始请求体))
//
// 两者合起来才成立：攻击者挑一个 Host 等于挑了一把密钥，而他没有那把密钥。
// 挑 A 店的 Host 用 B 店的密钥签，验签失败；不带签名，验签失败；
// **这家店根本没配密钥，也是验签失败**（见 verify 里那一段）——
// 最后这条是这类代码最经典的洞：「没配密钥就跳过验签」会让一个新建的、
// 还没接支付的店成为伪造入口，而它不报错、不告警，只是安静地收单。
//
// # 诚实边界：这不是微信/支付宝的真实验签
//
// 真实渠道各有自己的签名算法（微信 v3 是 SHA256-RSA + 平台证书，支付宝是
// RSA2 + 公钥证书）与自己的报文字段名，而且微信 v3 的报文是加密的。
// 本轮实现的是 **Keel 自己的规范报文 + HMAC 验签**：结构（每租户一把密钥、
// 原始字节验签、常量时间比较、没配密钥就拒绝）是真的，渠道适配器不是。
// 真接渠道时要加的是一层 adapter（把渠道报文翻成下面的 notification，
// 把渠道验签翻成 verify），业务这一段不用动。这笔账记在报告里。

// 回调相关的业务错误。handler 按它们映射契约里那两个状态码。
var (
	// ErrWebhookSignature：验签没过。契约：401，**且不得泄露任何内部状态**。
	//
	// 它刻意把好几种成因合成一个：签名错、没带签名、这家店没配密钥、
	// 渠道名不认识。分开报等于给攻击者一个「这家店配没配支付」的探测器。
	ErrWebhookSignature = errors.New("支付回调验签失败")

	// ErrWebhookBadPayload：验签过了，但报文解不开。
	//
	// 签名过了说明发报文的确实是这家店的渠道，所以这是**我们的适配器**跟渠道
	// 对不上，不是攻击。重推一次不会变好，所以回 200 并留一条 Error 日志。
	ErrWebhookBadPayload = errors.New("支付回调报文解不开")

	// ErrWebhookOrderUnknown：验签过了，但这个订单号在本租户查不到。
	//
	// 两种成因：回调打到了别家店的入口（Host 与订单的租户对不上，RLS 把行挡住），
	// 或者订单号是编的。**钱可能是真的到账了**，所以这条要响。
	ErrWebhookOrderUnknown = errors.New("支付回调里的订单号在本租户查不到")

	// ErrWebhookAmountMismatch：到账金额与应付金额对不上。
	//
	// 系统**不替用户认账**：支付单照样落库（钱的痕迹不能丢），订单状态不动。
	// 少付了就该少付着，多付了要退差额 —— 两种都要人介入。
	ErrWebhookAmountMismatch = errors.New("支付回调的金额与应付金额不符")

	// ErrWebhookOrderNotPayable：钱到账了，而这一单已经不在 10 待支付上。
	//
	// **这是超时补偿任务与支付回调撞车时，输掉的那一边看到的东西**，也是
	// 「用户付了两次」的样子。支付单落库、订单不动、一条 Error 日志。
	// 回 200 是对的：让渠道一直重推不会让这一单重新变成待支付，
	// 而重推会把同一笔钱在日志里放大成几十条。
	ErrWebhookOrderNotPayable = errors.New("支付回调到达时订单已不在待支付状态")

	// ErrWebhookDuplicate：这个渠道流水号已经入过账了。**正常路径。**
	//
	// 契约：「重复回调时写入冲突，直接返回 200」。渠道重复推送是常态 ——
	// 它没收到我们上一次的 200 就会再推，而「没收到」和「我们没处理」
	// 在它那边是同一件事。
	ErrWebhookDuplicate = errors.New("这笔渠道流水已经入过账")
)

// 渠道名 → payments.channel。映射写在契约的 components 里：wechat=1 / alipay=2 /
// balance=3。
//
// 契约里 webhook 的 channel 枚举**只有 wechat 与 alipay**，没有 balance ——
// 余额支付不产生渠道回调（/webhooks/refunds 那条接口的 description 明说了
// 同一件事）。所以这张表里也不该有 balance：给它一个入口等于开了一条
// 「用一条伪造的余额回调把订单推成已支付」的路，而余额支付本来就不走这里。
var webhookChannels = map[string]int16{
	"wechat": repository.PaymentChannelWechat,
	"alipay": repository.PaymentChannelAlipay,
}

// SignatureHeader 是携带签名的请求头。
//
// 导出是给测试与将来的渠道 adapter 用的 —— 一个写在两处的字符串，
// 写岔了的症状是「所有回调都 401」，而那看上去像密钥配错了。
const SignatureHeader = "X-Keel-Signature"

// MaxNotifyBytes 是回调报文的大小上限。
//
// 未认证入口必须有这道闸门：没有它，一个匿名请求能让进程为一份报文分配任意
// 大小的内存，而验签要在**读完整个 body 之后**才做得了（HMAC 算的是全部字节）。
// 也就是说这道闸门必须在验签之前，它保护的正是验签本身。
//
// 256 KB 对任何渠道的回调报文都是宽裕一个量级的余量。
const MaxNotifyBytes = 256 << 10

// notification 是渠道回调报文里本系统需要的字段。
//
// 它是 **Keel 的规范形状**，不是任何一个真实渠道的报文格式（见文件头「诚实边界」）。
// 契约里 requestBody 是 `additionalProperties: true` 的自由对象，正是因为
// 每家渠道的字段名都不一样 —— 契约不规定它，适配器负责翻译。
type notification struct {
	// OrderNo 是我们自己的订单号（渠道会原样带回来，它在渠道那边叫「商户订单号」）。
	OrderNo string `json:"order_no"`

	// ChannelTxnID 是渠道的流水号。**支付回调的幂等全靠它**
	// （uk_payments_channel_txn，见 00014）。
	ChannelTxnID string `json:"channel_txn_id"`

	// AmountCents 是实收金额，单位分。
	AmountCents int64 `json:"amount_cents"`

	// PaidAt 是渠道侧的到账时间。缺省时用收到回调的时间 —— 差几秒，
	// 但「用渠道的时间」在对账时是对的那一个。
	PaidAt *time.Time `json:"paid_at,omitempty"`
}

// PaymentRepository 是支付回调需要的仓储能力。
type PaymentRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error

	// ChannelNotifySecret 取本租户在这个渠道上的验签密钥。没配返回空串。
	ChannelNotifySecret(ctx context.Context, channel string) (string, error)
}

// PaymentService 实现支付渠道异步回调，以及发起支付（payment_intent.go）。
//
// 两条接口在同一个服务上，不是凑在一起的：发起支付要用的验签密钥、渠道映射、
// 规范报文的形状，与回调用的是同一份。拆成两个服务就要把它们复制一遍，
// 而复制之后沙箱造出来的报文与回调认得的报文可以慢慢分叉 —— 那种分叉的症状是
// 「沙箱支付 401」，看上去像密钥配错了。
type PaymentService struct {
	repo PaymentRepository
	cfg  PaymentConfig
	log  *slog.Logger
	now  func() time.Time
}

func NewPaymentService(r PaymentRepository, cfg PaymentConfig, log *slog.Logger) *PaymentService {
	if log == nil {
		log = slog.Default()
	}
	return &PaymentService{repo: r, cfg: cfg, log: log, now: time.Now}
}

// Notify 处理一次支付渠道回调。
//
// rawBody 必须是**原始字节**，不是重新序列化出来的 JSON：HMAC 算的是字节，
// 而 `{"a":1}` 与 `{"a": 1}` 是同一个对象、不同的字节。
//
// 返回 nil 表示这一单真的从 10 推到了 20。其余每一种返回值都对应上面一个
// sentinel，由 handler 决定状态码 —— 契约只给了 200 与 401 两种。
func (s *PaymentService) Notify(ctx context.Context, channel string, rawBody []byte, signature string) error {
	code, ok := webhookChannels[channel]
	if !ok {
		// 不认识的渠道名。归到验签失败那一支：我们没有这个渠道的密钥，
		// 所以这份报文确实验不过，而且这么回不会泄露「哪些渠道配了」。
		return fmt.Errorf("%w: 渠道 %q 不在 wechat/alipay 里", ErrWebhookSignature, channel)
	}
	if err := s.verify(ctx, channel, rawBody, signature); err != nil {
		return err
	}

	var n notification
	if err := json.Unmarshal(rawBody, &n); err != nil {
		return fmt.Errorf("%w: %v", ErrWebhookBadPayload, err)
	}
	if n.OrderNo == "" || n.ChannelTxnID == "" {
		return fmt.Errorf("%w: order_no 与 channel_txn_id 都不能为空（实得 %q / %q）",
			ErrWebhookBadPayload, n.OrderNo, n.ChannelTxnID)
	}
	if n.AmountCents <= 0 {
		return fmt.Errorf("%w: amount_cents 是 %d", ErrWebhookBadPayload, n.AmountCents)
	}

	paidAt := s.now()
	if n.PaidAt != nil {
		paidAt = *n.PaidAt
	}
	return s.settle(ctx, code, n, paidAt, rawBody)
}

// verify 验签。**没配密钥 = 验签失败**，这一条是这个函数存在的主要理由。
func (s *PaymentService) verify(ctx context.Context, channel string, rawBody []byte, signature string) error {
	return verifyChannelSignature(ctx, s.repo, s.log, channel, rawBody, signature)
}

// secretSource 是验签需要的全部仓储能力：取本租户在某个渠道上的回调密钥。
type secretSource interface {
	ChannelNotifySecret(ctx context.Context, channel string) (string, error)
}

// verifyChannelSignature 是支付回调与退款回调**共用**的验签。
//
// 退款回调（refund.go）与支付回调同构到底：同一把每租户每渠道的密钥、同一个
// 签名算法、同一条「没配密钥就拒绝」。抽成一个函数而不是在退款那边再抄一份：
// 两份验签各自演化，迟早有一份忘了常量时间比较，或者在没配密钥时「先放行」。
func verifyChannelSignature(ctx context.Context, repo secretSource, log *slog.Logger,
	channel string, rawBody []byte, signature string) error {
	secret, err := repo.ChannelNotifySecret(ctx, channel)
	if err != nil {
		// 读密钥失败是服务端故障，不是验签失败。原样上浮 —— 把它也报成 401
		// 的话，一次数据库抖动会表现为「渠道的回调全被拒了」，
		// 而监控上看不到任何 5xx，只看到渠道那边的告警。
		return err
	}
	if secret == "" {
		// 这家店还没配这个渠道的回调密钥。
		//
		// **拒绝，而不是跳过验签。** 「没配就放行」是这类代码最经典的洞：
		// 它让每一个新建的、还没接支付的店都成为一条伪造入口，
		// 而且不报错、不告警，只是安静地把订单推成已支付。
		//
		// 日志是 Warn 不是 Error：一家还没接支付的店收到回调，最可能的成因是
		// 有人在扫，而不是这家店坏了。
		log.WarnContext(ctx, "收到渠道回调，但这家店没有配这个渠道的回调密钥，一律拒绝",
			"channel", channel)
		return fmt.Errorf("%w: 本租户没有配置 %s 的回调密钥", ErrWebhookSignature, channel)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	want := hex.EncodeToString(mac.Sum(nil))

	// 常量时间比较。用 == 的话，比较耗时会随「前面对上了几个字符」变化，
	// 而攻击者可以一个字节一个字节地把签名试出来。这是个老掉牙的攻击，
	// 也正因为老掉牙，写成 == 时没有任何东西会提醒你。
	if subtle.ConstantTimeCompare([]byte(want), []byte(signature)) != 1 {
		return fmt.Errorf("%w: %s 对不上", ErrWebhookSignature, SignatureHeader)
	}
	return nil
}

// settle 是回调的业务这一段：支付单落库 + 订单 10 → 20。**一个事务。**
//
// 两件事必须原子：一笔已入账的支付单配一个还停在待支付的订单，
// 与一笔已支付的订单配不上任何支付单，都是对账查不清的账。
//
// 但「原子」不等于「要么全做要么全不做」——有两种情形是**支付单落库、订单不动**，
// 而且那正是对的：金额对不上、订单已不在待支付上。这两种都不是错误路径，
// 是「钱到了但我们不认这笔账」，痕迹必须留下来给人看。所以它们走 outcome 这条
// 出口，让事务正常提交；而真正该整体回滚的两种（订单查不到、重复流水）
// 直接从 fn 里返回错误。
func (s *PaymentService) settle(ctx context.Context, channel int16, n notification,
	paidAt time.Time, rawBody []byte) error {
	paymentNo, err := newPaymentNo(s.now())
	if err != nil {
		return err
	}

	var outcome error
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		order, err := tx.FindOrderByNo(ctx, n.OrderNo)
		if err != nil {
			// 查不到就整体回滚 —— 没有 order_id 就落不了支付单（外键），
			// 本来也没有任何东西可以提交。
			return err
		}

		// 支付单先落库：它同时是幂等的闸门。重复回调撞 uk_payments_channel_txn，
		// 从这里返回 ErrDuplicateChannelTxn，事务回滚，订单一个字都没动过。
		//
		// status 记 1 成功，**哪怕我们接下来不认这笔账**：这一列描述的是
		// 「渠道那边这笔支付成没成」，而 orders.status 描述的是「我们认不认」。
		// 把它们压进一列的话，「钱到了但订单已关」这种情形就没有形状可以表达。
		if _, err := tx.InsertPayment(ctx, repository.NewPayment{
			PaymentNo:     paymentNo,
			OrderID:       order.ID,
			Channel:       channel,
			AmountCents:   n.AmountCents,
			Status:        repository.PaymentSucceeded,
			ChannelTxnID:  n.ChannelTxnID,
			NotifyPayload: rawBody,
			PaidAt:        paidAt,
		}); err != nil {
			return err
		}

		if n.AmountCents != order.PayableCents {
			outcome = fmt.Errorf("%w: 订单 %s 应付 %d，到账 %d",
				ErrWebhookAmountMismatch, n.OrderNo, order.PayableCents, n.AmountCents)
			return nil
		}

		if err := tx.SettleOrder(ctx, n.OrderNo, n.AmountCents, paidAt); err != nil {
			if errors.Is(err, repository.ErrOrderNotPayable) {
				outcome = fmt.Errorf("%w: 订单 %s 当前状态是 %d",
					ErrWebhookOrderNotPayable, n.OrderNo, order.Status)
				return nil
			}
			return err
		}

		if err := consumeOrderCoupon(ctx, tx, s.log, order); err != nil {
			return err
		}
		// 通知（买家「支付成功」+ 门店「新订单待发货」）与 10 → 20 同一个事务（数据模型 §16）。
		return notifyOrderPaid(ctx, tx, order)
	})

	switch {
	case errors.Is(err, repository.ErrOrderNotFound):
		return fmt.Errorf("%w: %s", ErrWebhookOrderUnknown, n.OrderNo)
	case errors.Is(err, repository.ErrDuplicateChannelTxn):
		return fmt.Errorf("%w: %s", ErrWebhookDuplicate, n.ChannelTxnID)
	case err != nil:
		return err
	}
	return outcome
}

// consumeOrderCoupon 是订单 10 → 20 之后、**同一个事务里**的券核销。
// 支付回调（settle）与 0 元单在收尾分支里自动入账（settleFreeOrder）共用它 ——
// 两处各写一份的话，迟早有一处忘了核销券（券停在「锁定」，买家的券就这么没了）。
// 通知不收进来：notification_policy_test 要在改状态的那个函数里直接看到 notifyXxx 的调用。
func consumeOrderCoupon(ctx context.Context, tx repository.Tx, log *slog.Logger, order repository.Order) error {
	// 券核销：2 锁定 → 3 已使用，与订单 10 → 20 **同一个事务**（数据模型 §7）。
	// 「已使用」只描述一件事：有一笔认了账的订单用了这张券。
	//
	// 走到这里说明 SettleOrder 刚把这一单从 10 推到 20，而订单到得了 10 就意味着
	// SAGA 的券分支锁上了券（它排在建单之后、库存之前）。所以挂了券却核销 0 行
	// 是一条被破坏的不变量 —— 但**不回滚**：钱已经到了，回滚会让这笔到账记不下来、
	// 渠道一遍遍重推。留一条 Error 让人去对账，订单照常认账。
	if order.UserCouponID != nil {
		consumed, err := tx.ConsumeCouponForOrder(ctx, order.ID)
		if err != nil {
			return err
		}
		if consumed != 1 {
			log.ErrorContext(ctx, "订单已支付，但它挂的券不在「锁定」状态，核销了 0 张 —— 需要人工对账",
				"order_no", order.OrderNo, "user_coupon_id", *order.UserCouponID)
		}
	}
	return nil
}

// settleFreeOrder 让一张应付 0 元的单直接入账（10 → 20），不经过任何支付渠道。
//
// 立减券 / 满 100 减 100 再叠免运费，应付可以是 0。这样的单没有钱可收：发起支付会造出
// 一份 amount_cents=0 的回调，而回调那一侧拒绝 amount<=0（渠道从不回 0 元到账，
// 收到了就是伪造或报文坏了，那条校验不能放）—— 于是这一单卡在「待支付」直到超时被关，
// 券被解锁、库存被放回，买家白下一单。
//
// 所以它在收尾分支里、库存扣成的同一个事务里就认账：不落 payments 行（没有渠道、没有到账，
// payments 描述的是「渠道那边这笔支付成没成」），paid_cents 记 0，其余与支付回调逐项相同
// （consumeOrderCoupon + notifyOrderPaid）。退款那一侧对 paid_cents<=0 本来就回「没有可退的」，不用另改。
func settleFreeOrder(ctx context.Context, tx repository.Tx, log *slog.Logger,
	order repository.Order, now time.Time) error {
	if err := tx.SettleOrder(ctx, order.OrderNo, 0, now); err != nil {
		return err
	}
	if err := consumeOrderCoupon(ctx, tx, log, order); err != nil {
		return err
	}
	return notifyOrderPaid(ctx, tx, order)
}

// newPaymentNo 生成支付单号：14 位时间前缀 + 18 位十六进制随机。
//
// 形状与订单号一模一样（newOrderNo），理由也一样，写在那里：时间前缀给人看，
// 随机部分让它不可枚举。支付单号会出现在对账单上。
//
// 刻意不复用 newOrderNo 这个函数名下的实现：两者今天恰好同形，但订单号的形状是
// 对外承诺（写在数据模型 §2 那条分界线上），而支付单号只是我们自己的编号 ——
// 合成一个函数之后，改其中一个会静默改掉另一个。
func newPaymentNo(now time.Time) (string, error) {
	var b [orderNoRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源坏了。绝不回落到时间戳或计数器：可枚举的支付单号意味着
		// 任何人都能拿别人的单号去对账、去申诉。
		return "", fmt.Errorf("生成支付单号失败: %w", err)
	}
	return now.UTC().Format("20060102150405") + hex.EncodeToString(b[:]), nil
}
