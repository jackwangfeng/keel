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
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 发起支付：POST /orders/{order_no}/payments。
//
// # 这条接口本轮返回的是**沙箱支付**，而且它不打算被认错
//
// Keel 没有对接微信 v3，也没有对接支付宝。真接渠道要的是商户号、平台证书、
// 私钥保管、回调报文解密与各自的验签算法 —— 那是一份独立的工作，不在本轮范围里。
//
// 但客户端需要一条**能走通**的支付路径，否则 Demo 只能演到「下单成功」，
// 而「下单之后会发生什么」恰恰是这条链路上最值得演示的部分（金额校验、幂等、
// 状态机、超时补偿与回调撞车）。所以本轮给的是沙箱，并且用两道措施保证
// 没有人会把它当成真的微信支付：
//
//	① payment_no 带 KEEL-SANDBOX- 前缀，肉眼可辨；
//	② payload 里第一个键就是 sandbox: true，第二个是一句中文的白话警告。
//
// 契约的 PaymentIntent.payload 是 `additionalProperties: true` 的自由对象
// （「渠道特定的调起参数」），所以这两样东西都放得进去，**不需要改契约**。
// 渠道枚举里没有也不该有一个 sandbox 值：那会让「用哪个渠道付」与
// 「这是不是沙箱」这两件正交的事挤进同一个字段，真接渠道之后还得把枚举改回去。
//
// # 沙箱**不走**捷径：它只是把回调的钥匙交给调用方
//
// 最省事的做法是在这个函数里直接把订单改成 20 已支付。那会造出一段生产上
// 根本不存在的代码：金额校验、(channel, channel_txn_id) 的幂等、
// `SettleOrder` 里 `status = 10` 那个谓词（它是与超时补偿任务撞车时的唯一裁判）
// —— Demo 一条都演示不到，而它们才是这条链路真正的难点。
//
// 所以这里**只生成一份规范回调报文与它的签名**，原样交给调用方。调用方把它
// POST 到 `/webhooks/payments/{channel}`，之后发生的每一件事都与真实渠道
// 回调一模一样：同一个 verify、同一个 settle、同一张唯一索引。
// 这条接口自己一行 orders 都不写。
//
// # 代价说清楚：沙箱开着的时候，买家可以给自己的订单免费「付款」
//
// 签名是用这家店的回调密钥算的，所以拿到它的人能让**这一单**入账
// （而且只有这一单，只有这个金额 —— 报文的每个字节都进了 HMAC）。
// 那正是沙箱的定义，也正是它必须可关的理由：KEEL_PAYMENT_SANDBOX=off 之后
// 这条接口回 501，而不是回一个假装成功的支付参数。
// 密钥本身不会泄露（HMAC 不可逆），泄露的是这一次调起的通行证。

// PaymentConfig 是支付这一面的部署配置。
type PaymentConfig struct {
	// Sandbox 打开沙箱支付。**默认开**，因为本轮一个真实渠道都没接 ——
	// 关掉它之后这条接口除了 501 什么也回不了，而 README 那条
	// `docker compose up` 承诺的 Demo 要走到支付。
	//
	// 任何真的在收钱的部署都必须显式关掉它（app.EnvPaymentSandbox），
	// 而那一天同时也是有人在写真渠道适配器的那一天。
	Sandbox bool
}

// 发起支付相关的业务错误。handler 按它们映射契约里明写的那几个状态码。
var (
	// ErrOrderNotPayable：这一单当前不能支付。契约：409
	// `.../order-status-not-payable`（描述里明写「非 10 待支付，或已超时关闭」）。
	//
	// 名字里不带 Webhook，与 ErrWebhookOrderNotPayable 是两件事：那一个说的是
	// **钱已经到账了**而订单不在待支付上（要人去退钱），这一个说的是用户
	// 点了「去支付」而这一单已经付过或者关了（让客户端刷新页面）。
	// 合成一个 sentinel 会让前者的严重性被后者的日常噪音淹没。
	ErrOrderNotPayable = errors.New("订单当前状态不允许支付")

	// ErrPaymentChannelUnknown：渠道名不在契约的枚举里。契约：422。
	ErrPaymentChannelUnknown = errors.New("不认识的支付渠道")

	// ErrBalancePaymentNotImplemented：余额支付。契约的 PaymentIntent 里有它，
	// 本轮**没有实现**，回 501 而不是静默当成微信支付。
	//
	// 它与沙箱那条路的差别是结构性的，不只是「还没做」：余额支付不产生渠道回调
	// （契约里 webhook 的 channel 枚举只有 wechat/alipay），所以它根本不走
	// settle 那条路，得有自己的账户表与扣减事务。数据模型里没有那张表。
	ErrBalancePaymentNotImplemented = errors.New("余额支付尚未实现")

	// ErrSandboxDisabled：沙箱关了，而真实渠道没接。契约：501（default: Problem）。
	//
	// 这是关掉沙箱之后唯一诚实的回答。回一个「成功」的支付参数让客户端自己去
	// 调起微信，用户会在微信里看到一个报错，而服务端日志上什么都没有。
	ErrSandboxDisabled = errors.New("沙箱支付已关闭，而真实支付渠道尚未对接")

	// ErrSandboxNoSecret：这家店没配这个渠道的回调密钥。
	//
	// 沙箱造回调要用它签名，没有密钥就造不出一份能被 verify 放行的报文。
	// **绝不退化成「不签名」**：那等于让 /webhooks 那条路上「没配密钥 = 拒绝」
	// 的判断作废，而那一条正是支付回调最重要的一道闸门。
	ErrSandboxNoSecret = errors.New("本店没有配置该渠道的回调密钥，沙箱支付造不出可回推的回调")
)

// paymentIdempotencyScope 是这条接口在 idempotency_keys 里的作用域（数据模型 §12）。
//
// 与下单那个 scope 分开：同一个客户端在同一秒里给同一笔订单下单又发起支付，
// 用的很可能是同一个请求 id。作用域不分开的话，第二次调用会命中第一次的存档，
// 于是「发起支付」返回一个订单。
const paymentIdempotencyScope = "payments.create"

// archivedIntentStatus 是成功存档里的 response_code。201 来自契约。
const archivedIntentStatus int32 = 201

// sandboxPrefix 是沙箱支付单号的前缀。
//
// 它是「不可误认为真支付」这条要求的第一道措施，所以是一个常量而不是一句
// 拼在 Sprintf 里的字面量：测试对着它断言，改掉前缀会让那条断言红，
// 而不是让一串看上去像真支付单号的东西悄悄回给客户端。
const sandboxPrefix = "KEEL-SANDBOX-"

// sandboxNotice 是 payload 里那句白话警告。
//
// 写成中文的完整句子，不是一个 code。客户端开发者、验收的人、翻日志的人
// 都会看到它，而他们里没有谁应该先去查一张枚举表才知道这不是真钱。
const sandboxNotice = "这是 Keel 的沙箱支付，不是真实的微信支付或支付宝支付。" +
	"Keel 没有对接任何真实支付渠道，这里没有任何真实资金流动。" +
	"把 settle.body 原样 POST 到 settle.url（带上 settle.headers 里的两个头），" +
	"这一单就会经过与真实渠道回调**完全相同**的那条路径入账。"

// PaymentIntent 是一次支付调起（契约的 PaymentIntent）。
type PaymentIntent struct {
	PaymentNo   string
	Channel     string
	AmountCents int64

	// Payload 是契约里那个自由对象。本轮装的是沙箱标记与一份可回推的回调。
	Payload map[string]any

	// Replayed 为真表示这是一次幂等重放，本次调用没有生成新的调起凭据。
	// 不参与存档的 JSON（存档里存的是「首次那一份」，而首次不是重放）。
	Replayed bool `json:"-"`
}

// CreateIntent 实现 POST /orders/{order_no}/payments。
//
// 它**一行 orders 都不写**，也不预落支付单。支付单由回调那一侧落
// （service/payment.go 的 settle），那里那条光秃秃的 INSERT 同时是幂等闸门 ——
// 在调起阶段先插一行 status = 0 的支付单，会让回调带着同一个 channel_txn_id
// 撞上 uk_payments_channel_txn，于是订单永远推不到 20。
//
// 代价是**返回的 payment_no 不是 payments.payment_no**：它是这次调起的凭据，
// 入账之后以 `payments.channel_txn_id` 的形式落库（回调报文里带的就是它）。
// 对账时按 channel_txn_id 反查得到那一行。这一处与契约 201 的描述
// 「支付单已创建」不完全贴合，是本轮一处明写的欠账 —— 真接渠道时应当在调起阶段
// 预落 status = 0 待支付的支付单，并把 settle 从 INSERT 改成「认领已有那一行」，
// 而那会动到幂等闸门的形状，不该和本任务混在一起做。
func (s *PaymentService) CreateIntent(ctx context.Context, orderNo, channel, idemKey string) (PaymentIntent, error) {
	if channel == "balance" {
		// 在碰幂等键之前就拒，同 OrderService.Create 里券那一支：
		// 一个注定被拒的请求不该占掉客户端的那把钥匙。
		return PaymentIntent{}, ErrBalancePaymentNotImplemented
	}
	if _, ok := webhookChannels[channel]; !ok {
		return PaymentIntent{}, fmt.Errorf("%w: %q 不在 wechat/alipay/balance 里",
			ErrPaymentChannelUnknown, channel)
	}
	if !s.cfg.Sandbox {
		return PaymentIntent{}, fmt.Errorf("%w（渠道 %s）", ErrSandboxDisabled, channel)
	}
	if idemKey == "" {
		return PaymentIntent{}, fmt.Errorf("%w: 缺少 Idempotency-Key 请求头", ErrBadRequest)
	}
	id, err := auth.FromContext(ctx)
	if err != nil {
		return PaymentIntent{}, err
	}

	// 密钥在开事务之前取：它不依赖订单，而且它走的是池上另一条连接
	// （shop_settings 是 tenant-root 类，没有 RLS，不在 Tx 那一面）。
	secret, err := s.repo.ChannelNotifySecret(ctx, channel)
	if err != nil {
		return PaymentIntent{}, err
	}
	if secret == "" {
		return PaymentIntent{}, fmt.Errorf("%w（渠道 %s）", ErrSandboxNoSecret, channel)
	}

	hash := intentHash(orderNo, channel)

	var out PaymentIntent
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 抢占、校验、存档三件事在**同一个事务**里。
		//
		// 于是失败路径上不需要任何「把钥匙还回去」的善后代码：订单不存在、
		// 状态不对、熵源坏了，任何一个错误都让整个事务回滚，抢占那一行随之消失，
		// 客户端拿同一把钥匙原样重试即可 —— 因为确实什么都没发生。
		// （下单那条链路做不到这一点，它的 SAGA 跨了好几个事务。）
		claimed, err := tx.ClaimIdempotencyKey(ctx, paymentIdempotencyScope, repository.BuyerSubject(id.UserID), idemKey, hash)
		if err != nil {
			return err
		}
		if !claimed {
			out, err = replayIntent(ctx, tx, id.UserID, idemKey, hash)
			return err
		}

		// 锁住订单行：同一单并发的两次发起支付要串行，才能保证「至多一个有效的支付意图」（00150）。
		order, err := tx.LockUserOrderByNo(ctx, orderNo, id.UserID)
		if errors.Is(err, repository.ErrOrderNotFound) {
			return fmt.Errorf("%w: order_no=%s", ErrOrderNotFound, orderNo)
		}
		if err != nil {
			return err
		}
		if order.Status != orderStatusPending {
			return fmt.Errorf("%w: 订单 %s 当前状态是 %d，只有 %d 待支付可以发起支付",
				ErrOrderNotPayable, orderNo, order.Status, orderStatusPending)
		}
		placed, err := tx.IsOrderPlaced(ctx, order.ID)
		if err != nil {
			return err
		}
		if !placed {
			// 下单 SAGA 还没走完（库存还没扣成）。单号可能已经在买家的订单列表里了 ——
			// 拆分部署下库存服务不在时这一段能有几分钟。这时收钱，库存分支若被拒，全局补偿
			// 关不掉一张已支付的单（closeOrder 里那条 ERROR）。与「状态不对」同一个 409，客户端稍后重试。
			return fmt.Errorf("%w: 订单 %s 还在确认库存，请稍后再付", ErrOrderNotPayable, orderNo)
		}
		if !order.ExpireAt.After(s.now()) {
			// 已经过了支付时限。它还停在 10 只是因为超时补偿任务还没扫到它
			// （service/sweep.go 每隔一会儿跑一轮）。放行的话，用户会付一笔
			// 下一秒就被关掉的单 —— 那是真实到账配一笔已关闭订单，要人退钱。
			return fmt.Errorf("%w: 订单 %s 已于 %s 超时",
				ErrOrderNotPayable, orderNo, order.ExpireAt.Format(time.RFC3339))
		}

		// 一单同一时刻至多一个有效的支付意图（00150）。之前每次都造一个新流水号、库里不留痕，同一单能拿到任意多套
		// 都能付的参数（先点微信再点支付宝、并发点两次），第二笔到账就是多收（2026-09-28 破坏性测试）。
		//   · 同渠道：复用同一个流水号，参数一模一样 —— 买家不管付哪一次拿到的，都是同一笔；
		//   · 换渠道：旧的作废（真实渠道在这里要调它的关单接口；沙箱不需要），再发新的。作废之前旧的已经付了的，
		//     那笔到账订单不认，由多收款退回原路退回（payment_return.go）—— 关单挡不死，兜底必须在。
		chCode := webhookChannels[channel]
		active, found, err := tx.FindActivePaymentIntent(ctx, order.ID)
		if err != nil {
			return err
		}
		txn := ""
		if found && active.Channel == chCode && active.AmountCents == order.PayableCents {
			txn = active.ChannelTxnID
		} else {
			if found {
				if err := tx.SupersedePaymentIntent(ctx, active.ID); err != nil {
					return err
				}
			}
			if txn, err = newSandboxTxnID(s.now()); err != nil {
				return err
			}
			if err := tx.InsertPaymentIntent(ctx, order.ID, chCode, txn, order.PayableCents); err != nil {
				return err
			}
		}
		intent, err := s.sandboxIntent(order, channel, secret, txn)
		if err != nil {
			return err
		}
		body, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		code := archivedIntentStatus
		if err := tx.FinishIdempotencyKey(ctx, paymentIdempotencyScope, repository.BuyerSubject(id.UserID), idemKey,
			repository.IdempotencySucceeded, &code, body); err != nil {
			return err
		}
		out = intent
		return nil
	})
	if err != nil {
		return PaymentIntent{}, err
	}
	return out, nil
}

// sandboxIntent 造一次沙箱调起：一个不可误认的单号，外加一份**签好名的**
// 规范回调报文。
//
// 签名用的是这家店在这个渠道上的真实回调密钥，算法与 verify 逐字相同
// （hex(HMAC-SHA256(secret, 原始字节))）。这一点是整条沙箱路径成立的关键：
// 签名要是用别的算法或别的密钥算的，回调会被 401 挡下，而 Demo 会停在
// 「发起支付成功、订单还是待支付」——一个看上去像业务 bug 的装配错误。
//
// **报文的字节就是要被签的字节。** 这里先 Marshal 成 []byte 再签、再把同一份
// 字节以字符串形式放进 payload，不是绕远路：重新序列化一次会得到语义相同、
// 字节不同的 JSON（空格、字段顺序），而 HMAC 算的是字节。
func (s *PaymentService) sandboxIntent(order repository.Order, channel, secret, txn string) (PaymentIntent, error) {

	// 报文的形状是 service/payment.go 里那个 notification —— Keel 的规范报文，
	// 不是任何真实渠道的格式。用同一个类型而不是手拼一个 JSON 字面量：
	// 那个结构体改一个 json tag，这里会跟着改，而手拼的字面量不会，
	// 它的症状是沙箱回调忽然「报文解不开」。
	body, err := json.Marshal(notification{
		OrderNo:      order.OrderNo,
		ChannelTxnID: txn,

		// 金额取**订单的应付金额**，不是客户端传来的任何数字。
		// settle 会拿它和 payable_cents 比，不一致就落支付单、不动订单
		// （ErrWebhookAmountMismatch）。这里照抄 payable_cents 不是为了
		// 「让校验通过」，而是因为沙箱模拟的就是「用户付了应付的那个数」。
		AmountCents: order.PayableCents,
	})
	if err != nil {
		return PaymentIntent{}, err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return PaymentIntent{
		PaymentNo:   txn,
		Channel:     channel,
		AmountCents: order.PayableCents,
		Payload: map[string]any{
			"sandbox":        true,
			"sandbox_notice": sandboxNotice,

			// 渠道流水号单独给一份：入账之后它就是 payments.channel_txn_id，
			// 对账与排查都从这个值进。
			"channel_txn_id": txn,
			"settle": map[string]any{
				"method": "POST",

				// 相对路径：这个进程不知道自己对外是什么 origin（反代、
				// 自定义域名），而猜一个绝对 URL 只会在自定义域名的店上猜错。
				// 客户端拿自己请求用的那个 origin 拼上去即可。
				"url":     "/api/v1/webhooks/payments/" + channel,
				"headers": map[string]any{"Content-Type": "application/json", SignatureHeader: hex.EncodeToString(mac.Sum(nil))},
				"body":    string(body),
			},
		},
	}, nil
}

// replayIntent 读出已存在的幂等记录，按三态处理。
//
// 与 OrderService.replay 不合并：那一个回放的是一笔订单，而且它要处理
// 「失败也存档回放」（§12）—— 这条接口的失败路径根本不存档（整个事务回滚了，
// 抢占那一行都不在），所以这里遇到 status = 2 失败态只可能是数据坏了。
func replayIntent(ctx context.Context, tx repository.Tx, userID int64,
	idemKey, hash string) (PaymentIntent, error) {
	rec, err := tx.FindIdempotencyKey(ctx, paymentIdempotencyScope, repository.BuyerSubject(userID), idemKey)
	if errors.Is(err, repository.ErrIdempotencyKeyNotFound) {
		// 抢占说「已存在」，回头读却读不到：expire_at 到了、清理任务刚好把行删掉。
		// 让它以「处理中」的形式回 409 + Retry-After —— 客户端退避重试时
		// 那把钥匙已经彻底没了，会正常地抢到。
		return PaymentIntent{}, fmt.Errorf("%w: 幂等记录刚好过期了", ErrIdempotencyInFlight)
	}
	if err != nil {
		return PaymentIntent{}, err
	}
	if rec.RequestHash != hash {
		return PaymentIntent{}, fmt.Errorf("%w: 同一把 Idempotency-Key 配了不同的订单或渠道",
			ErrIdempotencyKeyReused)
	}
	if rec.Status != repository.IdempotencySucceeded {
		// 0 处理中：另一个并发请求正拿着这把钥匙。
		//   （抢占与存档在同一个事务里，所以这个窗口其实小到只有「进程在两者
		//   之间崩了」才看得到 —— 并发的第二个 INSERT 会在唯一索引上等着，
		//   等到的一定是已提交的存档。留着这一支是因为「小到看不到」不等于没有。）
		// 2 失败：这条接口不存档失败，出现它说明这一行是别的东西写的。
		return PaymentIntent{}, fmt.Errorf("%w: 幂等记录状态是 %d", ErrIdempotencyInFlight, rec.Status)
	}

	var out PaymentIntent
	if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
		return PaymentIntent{}, fmt.Errorf("幂等存档解不开: %w", err)
	}
	out.Replayed = true
	return out, nil
}

// intentHash 是幂等键那一列 request_hash。
//
// 这条接口的「同一个逻辑请求」由 (order_no, channel) 定义，不是整个请求体 ——
// 请求体里只有 channel，而 order_no 在路径上。漏掉 order_no 的后果很具体：
// 客户端用同一把钥匙给**另一笔**订单发起支付，会拿到上一笔订单的调起参数，
// 于是他付的是上一单的钱。
func intentHash(orderNo, channel string) string {
	sum := sha256.Sum256([]byte(orderNo + "\x00" + channel))
	return hex.EncodeToString(sum[:])
}

// newSandboxTxnID 生成沙箱的渠道流水号，也就是回给客户端的 payment_no。
//
// 形状：KEEL-SANDBOX-20060102150405-<18 位十六进制随机>。
//
// 前缀是「不可误认为真支付」的第一道措施。随机部分与订单号同源
// （orderNoRandomBytes），理由也一样：它会被当成 channel_txn_id 落进
// uk_payments_channel_txn，可枚举的流水号意味着别人能猜出你的那一条。
func newSandboxTxnID(now time.Time) (string, error) {
	var b [orderNoRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源坏了。绝不回落到时间戳或计数器 —— 同 newPaymentNo 那段。
		return "", fmt.Errorf("生成沙箱流水号失败: %w", err)
	}
	return sandboxPrefix + now.UTC().Format("20060102150405") + "-" + hex.EncodeToString(b[:]), nil
}
