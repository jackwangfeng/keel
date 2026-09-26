package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 消息通知的**外发**那一半（数据模型 §16）：outbox 里的 notification.deliver 任务 →
// 逐个渠道投递 → 每一次尝试记进 notification_deliveries。另外管通知的保留期清理。
//
// ===========================================================================
// 一、本期不接任何真实外发渠道，而这不是「以后再说」
// ===========================================================================
//
// 微信订阅消息要小程序的模板 id 与用户的订阅授权，短信要签名与模板的报备资质，
// 邮件要发信域名与 SMTP 凭据。三样本期都没有，所以三个渠道的默认实现都是
// unconfiguredChannel：Configured() 为 false，worker 把这一路记成 2 跳过（附上原因），
// **不报错、不重试**。站内消息不受影响 —— 它就是 notifications 那一行本身，
// 在业务事务里就已经「送达」了。
//
// 不写假实现：一个「假装发了」的短信渠道会让投递记录里全是 1 已发出，
// 而用户一条都没收到，那比「明写跳过」难查得多。将来拿到资质后怎么接，
// 写在 docs/指南/消息通知外发渠道接入.md（草案）。
//
// ===========================================================================
// 二、投递机制复用 jobs 队列（§12），不另造
// ===========================================================================
//
// 出队（每租户在途上限 + 兜底）、占位、指数退避、五次转死信、回收卡死任务、
// 清理七天前的成功任务 —— 全部是 repository/jobs.go 现成的那几条，与商品理解服务
// （index.go）共用。消费侧不需要枚举商户：租户写在出队拿到的那一行上
// （jobs.merchant_id），worker 按它进 WithTenant。
//
// 一次投递分三段，**网络调用不在任何数据库事务里**：
//
//	① 租户事务：读通知、读哪些渠道已经有定论（发出 / 跳过）；
//	② 事务外：逐个渠道 Send（真渠道是一次 HTTP 调用，可能要几百毫秒）；
//	③ 租户事务：把每一路的结果记下来。
//
// 有一路失败 → RetryJob（退避后整条任务重来，但①会跳过已有定论的渠道，
// 发出去的短信不会因为邮件失败而再发一遍）。全部有定论 → FinishJobs。
//
// ===========================================================================
// 三、保留期：90 天
// ===========================================================================
//
// 通知是提醒，不是账：订单、退款单本身永久保留，通知只是「那时告诉过你」。
// 90 天覆盖了最长的售后周期（自动确认 7 天 + 售后处理），再往前没有人会去翻消息中心。
// 清理按租户分批（notifications 有 RLS，没有能跨租户的 DELETE），每轮每家至多
// notificationPurgeBatch 条，一小时一轮；reads / deliveries 由外键级联。

// NotificationChannel 是一个外发渠道：微信订阅消息、短信、邮件。
//
// 实现要求：
//   - Name() 是 notification_deliveries.channel 的取值之一（wechat_subscribe / sms / email，
//     00053 的 CHECK 约束钉着）；
//   - Configured() 为 false 时 worker 不调 Send，直接记一行「跳过」；
//   - Send 返回 nil 表示渠道已受理；返回错误则这一路记「失败」并退避重试。
//     收件人（openid / 手机号 / 邮箱）由实现自己按 m.UserID 或 m.StoreID 查 ——
//     查不到收件人（买家没绑手机号）应当返回 ErrNoRecipient，worker 记成跳过而不是失败。
type NotificationChannel interface {
	Name() string
	Configured() bool
	Send(ctx context.Context, m NotificationMessage) error
}

// ErrNoRecipient：这一路找不到收件人（买家没授权订阅消息、没绑手机号……）。
// 不是故障，重试也不会变好 —— worker 记成「跳过」。
var ErrNoRecipient = errors.New("这个渠道找不到收件人")

// NotificationMessage 是交给渠道的一条消息：渲染好的标题正文 + 收件人 + 跳转目标。
type NotificationMessage struct {
	NotificationID int64
	MerchantID     int64
	Audience       int16
	UserID         *int64 // 发给买家时
	StoreID        *int64 // 发给商家时
	Kind           string
	Title          string
	Body           string
	TargetType     string
	OrderNo        *string
	RefundNo       *string
}

// 三个渠道名，与 00053 的 CHECK 约束一致。
const (
	ChannelWechatSubscribe = "wechat_subscribe"
	ChannelSMS             = "sms"
	ChannelEmail           = "email"
)

// unconfiguredChannel 是本期三个渠道的默认实现：未配置，不发送。
type unconfiguredChannel struct{ name, why string }

func (c unconfiguredChannel) Name() string            { return c.name }
func (c unconfiguredChannel) Configured() bool        { return false }
func (c unconfiguredChannel) UnconfiguredWhy() string { return c.why }
func (c unconfiguredChannel) Send(context.Context, NotificationMessage) error {
	return fmt.Errorf("渠道 %s 未配置：%s", c.name, c.why)
}

// DefaultNotificationChannels 是本期的渠道清单：三个都未配置。
//
// 顺序就是投递顺序。接入真渠道时替换对应的那一项（见接入文档草案），不改 worker。
func DefaultNotificationChannels() []NotificationChannel {
	return []NotificationChannel{
		unconfiguredChannel{ChannelWechatSubscribe, "需要小程序订阅消息模板与用户订阅授权（本期未接）"},
		unconfiguredChannel{ChannelSMS, "需要短信签名与模板报备资质（本期未接）"},
		unconfiguredChannel{ChannelEmail, "需要发信域名与 SMTP 凭据（本期未接）"},
	}
}

// NotificationDeliveryRepository 是外发 worker 需要的仓储能力：队列那一半（跑在 pool 上）、
// 租户事务、活跃商家清单（保留期清理要逐家进）。
type NotificationDeliveryRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)

	DequeueJobs(ctx context.Context, req repository.DequeueRequest) ([]repository.Job, error)
	FinishJobs(ctx context.Context, ids []int64) error
	RetryJob(ctx context.Context, id int64, reason string) error
	ReapStuckJobs(ctx context.Context, queue string, olderThan time.Duration) (int64, error)
	PurgeFinishedJobs(ctx context.Context, queue string, retain time.Duration, limit int) (int64, error)
}

// NotificationDeliveryConfig 是 worker 的几个旋钮。零值全部取默认。
type NotificationDeliveryConfig struct {
	// Workers 常驻消费者个数。<= 0 用 1 —— 本期三个渠道都是跳过，一个足够。
	Workers int
	// BatchSize 一次出队至多几条。<= 0 用 20。
	BatchSize int
	// PerTenantInflight 每租户在途上限（jobs §12）。<= 0 用 BatchSize。
	PerTenantInflight int
	// PollInterval 没活时歇多久。<= 0 用 2 秒。
	PollInterval time.Duration
	// Retention 通知保留多久。<= 0 用 NotificationRetention。
	Retention time.Duration
	// PurgeInterval 保留期清理多久一轮。<= 0 用 1 小时。
	PurgeInterval time.Duration
}

const (
	// NotificationRetention 通知保留 90 天（文件头第三节）。
	NotificationRetention = 90 * 24 * time.Hour

	// notificationPurgeBatch 每轮每家至多删这么多条（有界的 DELETE）。
	notificationPurgeBatch = 1000

	// notificationStuckAfter 执行中超过这么久就当 worker 死了、回收任务。
	// 一次投递是三个短事务加三次渠道调用，正常远小于它。
	notificationStuckAfter = 5 * time.Minute

	// notificationJobRetention 成功的投递任务留 7 天（jobs §12）。
	notificationJobRetention = 7 * 24 * time.Hour
	notificationJobPurge     = 1000
)

// NotificationDeliveryReport 是一批投递的结果。
type NotificationDeliveryReport struct {
	Jobs     int
	Sent     int // 各渠道「已发出」的行数
	Skipped  int // 各渠道「未配置 / 没有收件人」的行数
	Failed   int // 各渠道「失败」的行数
	Retried  int // 放回队列退避的任务数
	Dead     int // 转了死信的任务数
	Gone     int // 通知已被保留期删掉的任务数
	Finished int // 标成成功的任务数
}

// NotificationDeliveryService 是外发 worker + 保留期清理。
type NotificationDeliveryService struct {
	repo     NotificationDeliveryRepository
	channels []NotificationChannel
	cfg      NotificationDeliveryConfig
	log      *slog.Logger
	workerID string
	now      func() time.Time

	// cursor 是保留期清理的轮转起点（同 SweepService.cursor）。
	cursor uint64
}

// NewNotificationDeliveryService 建外发 worker。channels 为 nil 时用 DefaultNotificationChannels。
func NewNotificationDeliveryService(r NotificationDeliveryRepository, channels []NotificationChannel,
	cfg NotificationDeliveryConfig, log *slog.Logger) *NotificationDeliveryService {
	if log == nil {
		log = slog.Default()
	}
	if channels == nil {
		channels = DefaultNotificationChannels()
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}
	if cfg.PerTenantInflight <= 0 {
		cfg.PerTenantInflight = cfg.BatchSize
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Retention <= 0 {
		cfg.Retention = NotificationRetention
	}
	if cfg.PurgeInterval <= 0 {
		cfg.PurgeInterval = time.Hour
	}
	host, _ := os.Hostname()
	return &NotificationDeliveryService{
		repo: r, channels: channels, cfg: cfg, log: log, now: time.Now,
		workerID: fmt.Sprintf("notify@%s:%d", host, os.Getpid()),
	}
}

// WithClock 换掉时钟，只给测试用（保留期的边界要能在不等 90 天的情况下验到）。
func (s *NotificationDeliveryService) WithClock(now func() time.Time) *NotificationDeliveryService {
	s.now = now
	return s
}

// Run 起 cfg.Workers 个消费者，外加回收 / 清理的定时轮，直到 ctx 被取消。
// 回收排在第一次出队之前，理由同 IndexService.Run（上一条命的残骸占着在途配额）。
func (s *NotificationDeliveryService) Run(ctx context.Context) {
	s.housekeep(ctx)
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.consume(ctx)
		}()
	}
	t := time.NewTicker(s.cfg.PurgeInterval)
	defer t.Stop()
	for {
		s.purgeOnce(ctx)
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "通知外发任务收到停止信号，等消费者收尾")
			wg.Wait()
			return
		case <-t.C:
			s.housekeep(ctx)
		}
	}
}

func (s *NotificationDeliveryService) consume(ctx context.Context) {
	for {
		rep, err := s.WorkOnce(ctx)
		switch {
		case err != nil:
			s.log.ErrorContext(ctx, "通知外发这一批没跑起来", "err", err)
		case rep.Jobs > 0:
			s.log.DebugContext(ctx, "投递完一批通知",
				"jobs", rep.Jobs, "sent", rep.Sent, "skipped", rep.Skipped, "failed", rep.Failed,
				"retried", rep.Retried, "dead", rep.Dead, "gone", rep.Gone)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

func (s *NotificationDeliveryService) housekeep(ctx context.Context) {
	if n, err := s.repo.ReapStuckJobs(ctx, repository.QueueNotificationDelivery, notificationStuckAfter); err != nil {
		s.log.ErrorContext(ctx, "回收卡死的通知投递任务失败", "err", err)
	} else if n > 0 {
		s.log.WarnContext(ctx, "回收了卡在执行中的通知投递任务", "count", n)
	}
	if _, err := s.repo.PurgeFinishedJobs(ctx, repository.QueueNotificationDelivery,
		notificationJobRetention, notificationJobPurge); err != nil {
		s.log.ErrorContext(ctx, "清理过期的通知投递任务失败", "err", err)
	}
}

func (s *NotificationDeliveryService) purgeOnce(ctx context.Context) {
	n, err := s.PurgeOnce(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "通知保留期清理这一轮没跑起来", "err", err)
		return
	}
	if n > 0 {
		s.log.InfoContext(ctx, "清理了过了保留期的通知", "count", n, "retention", s.cfg.Retention)
	}
}

// WorkOnce 出队一批、投递、记结果。导出是为了让测试不等轮询就能驱动它。
func (s *NotificationDeliveryService) WorkOnce(ctx context.Context) (NotificationDeliveryReport, error) {
	jobs, err := s.repo.DequeueJobs(ctx, repository.DequeueRequest{
		Queue:             repository.QueueNotificationDelivery,
		Limit:             s.cfg.BatchSize,
		PerTenantInflight: s.cfg.PerTenantInflight,
		WorkerID:          s.workerID,
	})
	if err != nil || len(jobs) == 0 {
		return NotificationDeliveryReport{}, err
	}
	rep := NotificationDeliveryReport{Jobs: len(jobs)}
	var done []int64
	for _, j := range jobs {
		finished := s.deliver(ctx, j, &rep)
		if finished {
			done = append(done, j.ID)
		}
	}
	if err := s.repo.FinishJobs(ctx, done); err != nil {
		return rep, err
	}
	rep.Finished = len(done)
	return rep, nil
}

// Drain 反复 WorkOnce 直到队列里没有到期的投递任务，返回合计。给测试用。
func (s *NotificationDeliveryService) Drain(ctx context.Context) (NotificationDeliveryReport, error) {
	var total NotificationDeliveryReport
	for {
		one, err := s.WorkOnce(ctx)
		total.Jobs += one.Jobs
		total.Sent += one.Sent
		total.Skipped += one.Skipped
		total.Failed += one.Failed
		total.Retried += one.Retried
		total.Dead += one.Dead
		total.Gone += one.Gone
		total.Finished += one.Finished
		if err != nil || one.Jobs == 0 {
			return total, err
		}
	}
}

// deliver 投递一条任务，返回它能不能标成成功（false = 已经 RetryJob 过）。
func (s *NotificationDeliveryService) deliver(ctx context.Context, j repository.Job,
	rep *NotificationDeliveryReport) bool {
	log := s.log.With("merchant_id", j.MerchantID, "job_id", j.ID)
	var p deliveryPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil || p.NotificationID <= 0 {
		s.retry(ctx, log, j, fmt.Errorf("payload 解不开或没有 notification_id: %s", j.Payload), rep)
		return false
	}
	tctx := tenant.NewContext(ctx, j.MerchantID)

	// ① 读通知与已有定论的渠道。
	var n repository.NotificationForDelivery
	var settled map[string]bool
	err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		var err error
		if n, err = tx.GetNotificationForDelivery(tctx, p.NotificationID); err != nil {
			return err
		}
		settled, err = tx.FinishedDeliveryChannels(tctx, p.NotificationID)
		return err
	})
	if errors.Is(err, repository.ErrNotificationNotFound) {
		// 过了保留期被删了（任务在队列里躺了 90 天以上 —— 多半是死信被人手工复活）。
		// 没有东西可投，标成功。
		rep.Gone++
		return true
	}
	if err != nil {
		s.retry(ctx, log, j, err, rep)
		return false
	}

	// ② 事务外逐路投递。
	msg := NotificationMessage{
		NotificationID: n.ID, MerchantID: j.MerchantID, Audience: n.Audience,
		UserID: n.UserID, StoreID: n.StoreID, Kind: n.Kind, Title: n.Title, Body: n.Body,
		TargetType: n.TargetType, OrderNo: n.OrderNo, RefundNo: n.RefundNo,
	}
	type outcome struct {
		channel string
		status  int16
		detail  string
	}
	var results []outcome
	var failures []string
	for _, ch := range s.channels {
		if settled[ch.Name()] {
			continue
		}
		if !ch.Configured() {
			detail := "未配置"
			if w, ok := ch.(interface{ UnconfiguredWhy() string }); ok {
				detail += "：" + w.UnconfiguredWhy()
			}
			results = append(results, outcome{ch.Name(), repository.NotificationDeliverySkipped, detail})
			continue
		}
		err := ch.Send(ctx, msg)
		switch {
		case err == nil:
			results = append(results, outcome{ch.Name(), repository.NotificationDeliverySent, ""})
		case errors.Is(err, ErrNoRecipient):
			results = append(results, outcome{ch.Name(), repository.NotificationDeliverySkipped, err.Error()})
		default:
			results = append(results, outcome{ch.Name(), repository.NotificationDeliveryFailed, err.Error()})
			failures = append(failures, ch.Name()+": "+err.Error())
		}
	}

	// ③ 记结果。
	if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		for _, r := range results {
			if err := tx.InsertNotificationDelivery(tctx, n.ID, r.channel, r.status, j.Attempts, r.detail); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		// 结果没记下来：整条重来。已经发出去的那一路会再发一次 —— 这是「至少一次」
		// 的代价，写清楚：渠道侧要按 notification_id 去重（接入文档草案里有这一条）。
		s.retry(ctx, log, j, fmt.Errorf("记投递结果失败: %w", err), rep)
		return false
	}
	for _, r := range results {
		switch r.status {
		case repository.NotificationDeliverySent:
			rep.Sent++
		case repository.NotificationDeliverySkipped:
			rep.Skipped++
		default:
			rep.Failed++
		}
	}
	if len(failures) > 0 {
		s.retry(ctx, log, j, errors.New(strings.Join(failures, "; ")), rep)
		return false
	}
	return true
}

func (s *NotificationDeliveryService) retry(ctx context.Context, log *slog.Logger, j repository.Job,
	cause error, rep *NotificationDeliveryReport) {
	err := s.repo.RetryJob(ctx, j.ID, cause.Error())
	switch {
	case errors.Is(err, repository.ErrJobDeadLettered):
		rep.Dead++
		log.ErrorContext(ctx, "通知投递重试次数用尽，已转死信", "cause", cause)
	case err != nil:
		log.ErrorContext(ctx, "通知投递放回队列失败（回收任务会接手）", "cause", cause, "err", err)
	default:
		rep.Retried++
		log.WarnContext(ctx, "通知投递失败，退避重试", "attempt", j.Attempts, "cause", cause)
	}
}

// PurgeOnce 跑一轮保留期清理：逐家删 Retention 之前的通知，返回删掉的条数。
//
// 公平调度照 fairRound（每家至多 notificationPurgeBatch 条、总预算十倍于此、轮转起点）：
// 一家积压了半年的店不该让别家的清理一轮都轮不到。
func (s *NotificationDeliveryService) PurgeOnce(ctx context.Context) (int64, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil || len(merchants) == 0 {
		return 0, err
	}
	cutoff := s.now().Add(-s.cfg.Retention)
	start := int(s.cursor % uint64(len(merchants)))
	s.cursor++
	var total int64
	fairRound(merchants, start, notificationPurgeBatch, notificationPurgeBatch*10,
		func(merchantID int64, limit int) int {
			tctx := tenant.NewContext(ctx, merchantID)
			var n int64
			if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
				var err error
				n, err = tx.PurgeExpiredNotifications(tctx, cutoff, int32(limit))
				return err
			}); err != nil {
				s.log.ErrorContext(ctx, "清理这家店过期的通知失败", "merchant_id", merchantID, "err", err)
				return 0
			}
			total += n
			return int(n)
		})
	return total, nil
}
