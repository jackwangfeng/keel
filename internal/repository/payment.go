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
	"github.com/keel/keel/internal/tenant"
)

// 支付回调（Task 7）在 repository 边界上的那一面。

// channelTxnConstraint 是 00014 里那个部分唯一索引的名字。
//
// **它是接口的一部分，不是一个内部细节。** 支付回调的幂等全靠它：重复回调撞在
// 这个约束上，而 Go 侧要把这一种 23505 与别的 23505（payment_no 撞车）分开。
// 靠约束名而不是「凡是 23505 都当重复」，是因为后者会把一次熵源故障
// （payment_no 重复）伪装成一次正常的重复回调，然后静默丢掉一笔真实到账。
//
// 改索引名要同时改这里。00014 的文件头写着同一句话。
const channelTxnConstraint = "uk_payments_channel_txn"

var (
	// ErrDuplicateChannelTxn：这个渠道流水号在本租户已经入过账。
	//
	// **正常路径。** 渠道重复推送是常态（尤其是它没收到我们的 200 的时候），
	// 契约明写「重复回调时写入冲突，直接返回 200」。
	ErrDuplicateChannelTxn = errors.New("渠道流水号重复")

	// ErrOrderNotPayable：钱到账了，而这一单已经不在 10 待支付上。
	//
	// **它不是「可以忽略」的那一类。** 两种成因都要人介入：
	//   · 订单已被超时任务关到 90 —— 用户在关单之后才付款成功，要退款；
	//   · 订单已经是 20 或更远 —— 用户付了两次，要退一次。
	// 所以它是一个 sentinel 而不是一句日志：支付单那一行照样落库（钱的痕迹
	// 不能丢），但订单状态不动，而调用方必须显式处理这一支。
	ErrOrderNotPayable = errors.New("订单已不在待支付状态")
)

// 支付渠道（数据模型 §5 payments.channel / 契约的 components 映射）。
//
// 常量而不是散落的 1 / 2 / 3：这个数字会决定对账时一笔钱该找微信还是找支付宝，
// 而一个写反了的字面量在任何测试里都长得像一笔正常的支付。
const (
	PaymentChannelWechat  int16 = 1
	PaymentChannelAlipay  int16 = 2
	PaymentChannelBalance int16 = 3
)

// 支付单状态（数据模型 §5）。
const (
	PaymentPending   int16 = 0
	PaymentSucceeded int16 = 1
	PaymentFailed    int16 = 2
	PaymentClosed    int16 = 3
)

// NewPayment 是落一行支付单要写的列。
//
// 没有 MerchantID：那一列的默认值是 current_merchant()（00014），
// 调用方没有那个参数可以传错。
type NewPayment struct {
	PaymentNo     string
	OrderID       int64
	Channel       int16
	AmountCents   int64
	Status        int16
	ChannelTxnID  string
	NotifyPayload []byte
	PaidAt        time.Time
}

// PaymentTx 是支付回调这一面。
type PaymentTx interface {
	// InsertPayment 落一行支付单。
	//
	// 渠道流水号在本租户已经存在时返回 ErrDuplicateChannelTxn —— 那是幂等，
	// 不是失败。任何别的唯一冲突原样上浮。
	InsertPayment(ctx context.Context, p NewPayment) (int64, error)

	// SettleOrder 把订单从 10 待支付推到 20 已支付。
	//
	// 订单已不在 10 上时返回 ErrOrderNotPayable。**调用方不许把它当成成功**：
	// 那意味着一笔真实到账没有对应的已支付订单，要人来退钱。
	SettleOrder(ctx context.Context, orderNo string, paidCents int64, paidAt time.Time) error
}

func (t tenantTx) InsertPayment(ctx context.Context, p NewPayment) (int64, error) {
	if p.ChannelTxnID == "" {
		// 空流水号会让那个部分唯一索引（WHERE channel_txn_id IS NOT NULL）整个
		// 失效 —— 幂等的最后一道锁静默消失，重复回调会一行行堆进来。
		// 挡在这里，因为 NOT NULL 拦不住空串。
		return 0, errors.New("渠道流水号为空：支付回调的幂等全靠它，不能落一行没有流水号的支付单")
	}
	row, err := t.q.InsertPayment(ctx, db.InsertPaymentParams{
		PaymentNo:     p.PaymentNo,
		OrderID:       p.OrderID,
		Channel:       p.Channel,
		AmountCents:   p.AmountCents,
		Status:        p.Status,
		ChannelTxnID:  &p.ChannelTxnID,
		NotifyPayload: p.NotifyPayload,
		PaidAt:        pgtype.Timestamptz{Time: p.PaidAt, Valid: !p.PaidAt.IsZero()},
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == channelTxnConstraint {
			return 0, fmt.Errorf("channel=%d txn=%s: %w",
				p.Channel, p.ChannelTxnID, ErrDuplicateChannelTxn)
		}
		// 别的 23505 原样上浮。最可能的一个是 payment_no 撞车，而那是熵源坏了
		// 或者生成逻辑被改坏了 —— 把它当成「重复回调」会静默丢掉一笔真实到账。
		return 0, err
	}
	return row.ID, nil
}

func (t tenantTx) SettleOrder(ctx context.Context, orderNo string, paidCents int64, paidAt time.Time) error {
	n, err := t.q.SettleOrder(ctx, db.SettleOrderParams{
		OrderNo:   orderNo,
		PaidCents: paidCents,
		PaidAt:    pgtype.Timestamptz{Time: paidAt, Valid: true},
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("order %s: %w", orderNo, ErrOrderNotPayable)
	}
	return nil
}

// ChannelNotifySecret 取本租户在某个支付渠道上的回调验签密钥。
//
// 密钥存在 `shop_settings.extra`（00001 建的那一列，JSONB）里：
//
//	{"payment_channels": {"wechat": {"notify_secret": "..."}}}
//
// **为什么不新建一张表**：check_tenancy.py 拿设计文档当真相源，一张文档里没有的
// 表会让「库里有而文档里没有的表」那道闸门当场红（9a3e771 就是加这道闸门的那次
// 提交）。而 `extra` 是数据模型里**已经存在**的扩展位，用它不需要改 schema，
// 也不需要往两份真相源里各加一张表。
//
// 这条路的代价要说清楚，它是本轮的一处诚实边界：
//   - 密钥是明文存的。真接渠道时这一列应当是密文（或者干脆挪到 KMS / 环境变量），
//     而那要一份密钥管理的设计，不在本任务范围里。
//   - 没有「换密钥」的过渡期支持（同时接受新旧两把）。
//
// 走裸 SQL 而不是 sqlc：shop_settings 是 tenant-root 类，**没有 RLS**
// （租户解析要在 SET LOCAL 之前读它），所以它不属于 tenantTx 那一面。
// tenant/resolver.go 读它时同此惯例。
//
// merchantID 从 ctx 取，与 WithTenant 同一个规矩：调用方没有那个参数可以传错。
// 这一条在这里尤其要紧 —— 传错了的后果是**拿 A 店的密钥去验 B 店的回调**，
// 而那正好是伪造一笔支付所需要的全部条件。
func (r *Repo) ChannelNotifySecret(ctx context.Context, channel string) (string, error) {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return "", err
	}
	var secret *string
	err = r.pool.QueryRow(ctx, `
		SELECT extra #>> ARRAY['payment_channels', $2, 'notify_secret']
		  FROM shop_settings
		 WHERE merchant_id = $1`, merchantID, channel).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
		// 这家店连 shop_settings 都没有。不是错误 —— 它就是「没配密钥」，
		// 调用方按空串走「拒绝」那一支。
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读商家 %d 的 %s 回调密钥失败: %w", merchantID, channel, err)
	}
	if secret == nil {
		return "", nil
	}
	return *secret, nil
}
