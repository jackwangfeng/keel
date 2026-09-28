package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 事件 webhook 的投递（AI 经营 M10 §3「推」）。机制照抄消息通知的外发（notification_delivery.go）：
// jobs 队列（agent.event.deliver）、每租户在途上限、指数退避、回收卡死任务、清理七天前的成功任务。
//
// 一条任务 = 一个（事件，webhook）。一次投递分三段，**网络调用不在任何数据库事务里**：
//
//	① 租户事务：读 webhook、事件、AI 员工与它现在的管辖范围；webhook 删了 / 停了、AI 员工停用了、
//	   事件不在它现在的范围里 → 任务直接结束，不投（范围按投递这一刻判，与 list_events 同一条过滤）；
//	② 事务外：POST <url>，5 秒超时，不跟随重定向；
//	③ 租户事务：记一行 agent_webhook_deliveries（第几次、状态码、错误）。
//
// 2xx 即成功；其余（连不上、超时、非 2xx）退避重试，第 6 次仍失败转死信。
// 这是「至少一次」：接收方按 X-Keel-Event-Id（= 请求体的 id）去重。
//
// 请求体：{"id","type","store_id","payload","created_at"}。
// 请求头：X-Keel-Event（类型）、X-Keel-Event-Id、X-Keel-Signature: sha256=<hex(HMAC-SHA256(secret, 原始请求体))>。

// AgentWebhookTimeout 是一次投递的超时。
const AgentWebhookTimeout = 5 * time.Second

const (
	agentWebhookStuckAfter   = 2 * time.Minute
	agentWebhookJobRetention = 7 * 24 * time.Hour
	agentWebhookJobPurge     = 1000
	// agentWebhookErrMax 是投递记录里错误文本的上限（响应体只截这么多）。
	agentWebhookErrMax = 300
)

// AgentWebhookDeliveryRepository 是投递 worker 要的仓储能力。
type AgentWebhookDeliveryRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	DequeueJobs(ctx context.Context, req repository.DequeueRequest) ([]repository.Job, error)
	FinishJobs(ctx context.Context, ids []int64) error
	RetryJob(ctx context.Context, id int64, reason string) error
	ReapStuckJobs(ctx context.Context, queue string, olderThan time.Duration) (int64, error)
	PurgeFinishedJobs(ctx context.Context, queue string, retain time.Duration, limit int) (int64, error)
}

// AgentWebhookDeliveryReport 是一批投递的结果。
type AgentWebhookDeliveryReport struct {
	Jobs      int
	Delivered int // 2xx
	Skipped   int // webhook 没了 / 停了、AI 员工停用、不在范围里
	Retried   int
	Dead      int
	Finished  int
}

// AgentWebhookDeliveryService 是投递 worker。
type AgentWebhookDeliveryService struct {
	repo     AgentWebhookDeliveryRepository
	client   *http.Client
	log      *slog.Logger
	workerID string
	batch    int
	poll     time.Duration
}

// NewAgentWebhookDeliveryService 建投递 worker。client 为 nil 时用 5 秒超时、不跟随重定向的客户端。
func NewAgentWebhookDeliveryService(repo AgentWebhookDeliveryRepository, client *http.Client,
	log *slog.Logger) *AgentWebhookDeliveryService {
	if log == nil {
		log = slog.Default()
	}
	if client == nil {
		client = &http.Client{Timeout: AgentWebhookTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	host, _ := os.Hostname()
	return &AgentWebhookDeliveryService{repo: repo, client: client, log: log, batch: 20, poll: 2 * time.Second,
		workerID: fmt.Sprintf("agent-webhook@%s:%d", host, os.Getpid())}
}

// Run 起一个消费者，外加回收 / 清理的定时轮，直到 ctx 被取消。
func (s *AgentWebhookDeliveryService) Run(ctx context.Context) {
	s.housekeep(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			rep, err := s.WorkOnce(ctx)
			if err != nil {
				s.log.ErrorContext(ctx, "AI 员工事件投递这一批没跑起来", "err", err)
			} else if rep.Jobs > 0 {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.poll):
			}
		}
	}()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			s.housekeep(ctx)
		}
	}
}

func (s *AgentWebhookDeliveryService) housekeep(ctx context.Context) {
	if n, err := s.repo.ReapStuckJobs(ctx, repository.QueueAgentEventDelivery, agentWebhookStuckAfter); err != nil {
		s.log.ErrorContext(ctx, "回收卡死的事件投递任务失败", "err", err)
	} else if n > 0 {
		s.log.WarnContext(ctx, "回收了卡在执行中的事件投递任务", "count", n)
	}
	if _, err := s.repo.PurgeFinishedJobs(ctx, repository.QueueAgentEventDelivery,
		agentWebhookJobRetention, agentWebhookJobPurge); err != nil {
		s.log.ErrorContext(ctx, "清理过期的事件投递任务失败", "err", err)
	}
}

// WorkOnce 出队一批、投递、记结果。导出给测试驱动。
func (s *AgentWebhookDeliveryService) WorkOnce(ctx context.Context) (AgentWebhookDeliveryReport, error) {
	jobs, err := s.repo.DequeueJobs(ctx, repository.DequeueRequest{Queue: repository.QueueAgentEventDelivery,
		Limit: s.batch, PerTenantInflight: s.batch, WorkerID: s.workerID})
	if err != nil || len(jobs) == 0 {
		return AgentWebhookDeliveryReport{}, err
	}
	rep := AgentWebhookDeliveryReport{Jobs: len(jobs)}
	var done []int64
	for _, j := range jobs {
		if s.deliver(ctx, j, &rep) {
			done = append(done, j.ID)
		}
	}
	if err := s.repo.FinishJobs(ctx, done); err != nil {
		return rep, err
	}
	rep.Finished = len(done)
	return rep, nil
}

// Drain 反复 WorkOnce 直到没有到期的任务。给测试用。
func (s *AgentWebhookDeliveryService) Drain(ctx context.Context) (AgentWebhookDeliveryReport, error) {
	var total AgentWebhookDeliveryReport
	for {
		one, err := s.WorkOnce(ctx)
		total.Jobs += one.Jobs
		total.Delivered += one.Delivered
		total.Skipped += one.Skipped
		total.Retried += one.Retried
		total.Dead += one.Dead
		total.Finished += one.Finished
		if err != nil || one.Jobs == 0 {
			return total, err
		}
	}
}

// AgentWebhookBody 是投递的请求体。
type AgentWebhookBody struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"`
	StoreID   *int64          `json:"store_id"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// SignAgentWebhook 是签名：hex(HMAC-SHA256(secret, body))。请求头里是 "sha256=" + 它。
func SignAgentWebhook(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// deliver 投递一条任务，返回能不能标成成功（false = 已经 RetryJob 过）。
func (s *AgentWebhookDeliveryService) deliver(ctx context.Context, j repository.Job, rep *AgentWebhookDeliveryReport) bool {
	log := s.log.With("merchant_id", j.MerchantID, "job_id", j.ID)
	var p agentEventJob
	if err := json.Unmarshal(j.Payload, &p); err != nil || p.EventID <= 0 || p.WebhookID <= 0 {
		log.ErrorContext(ctx, "事件投递任务的 payload 解不开，丢弃", "payload", string(j.Payload))
		rep.Skipped++
		return true
	}
	tctx := tenant.NewContext(ctx, j.MerchantID)

	// ① 读 webhook、事件、AI 员工现在的身份；判范围。
	var (
		hook repository.AgentWebhook
		ev   repository.AgentEvent
		skip bool
	)
	err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		var err error
		if hook, err = tx.FindAgentWebhook(tctx, p.WebhookID); errors.Is(err, repository.ErrAgentWebhookNotFound) {
			skip = true
			return nil
		} else if err != nil {
			return err
		}
		if !hook.Enabled {
			skip = true
			return nil
		}
		if ev, err = tx.FindAgentEvent(tctx, p.EventID); errors.Is(err, repository.ErrAgentEventNotFound) {
			skip = true
			return nil
		} else if err != nil {
			return err
		}
		a, err := tx.FindAgent(tctx, hook.AgentStaffID)
		if errors.Is(err, repository.ErrAgentNotFound) {
			skip = true
			return nil
		} else if err != nil {
			return err
		}
		if a.Status != auth.StaffStatusActive {
			skip = true
			return nil
		}
		sc, err := tx.ListStaffScopes(tctx, a.ID)
		if err != nil {
			return err
		}
		// 与 list_events 同一条过滤：从这条事件的前一个 id 往后取一条，取到的就是它才算在范围里。
		scope := agentEventScope(auth.StaffIdentity{StaffID: a.ID, Role: a.Role, Status: a.Status,
			RegionIDs: sc.RegionIDs, StoreIDs: sc.StoreIDs})
		got, err := tx.ListAgentEvents(tctx, ev.ID-1, scope, 1)
		if err != nil {
			return err
		}
		skip = len(got) == 0 || got[0].ID != ev.ID
		return nil
	})
	if err != nil {
		s.retry(ctx, log, j, err, rep)
		return false
	}
	if skip {
		rep.Skipped++
		return true
	}

	// ② 事务外投递。
	body, err := json.Marshal(AgentWebhookBody{ID: ev.ID, Type: ev.Type, StoreID: ev.StoreID,
		Payload: json.RawMessage(ev.Payload), CreatedAt: ev.CreatedAt})
	if err != nil {
		s.retry(ctx, log, j, err, rep)
		return false
	}
	code, sendErr := s.post(ctx, hook, ev, body)

	// ③ 记结果。
	d := repository.AgentWebhookDelivery{EventID: ev.ID, WebhookID: hook.ID, Attempt: j.Attempts}
	if code != 0 {
		c := int32(code)
		d.StatusCode = &c
	}
	if sendErr != nil {
		d.Error = truncateRunes(sendErr.Error(), agentWebhookErrMax)
	}
	if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		return tx.InsertAgentWebhookDelivery(tctx, d)
	}); err != nil {
		log.ErrorContext(ctx, "事件投递记录没写进去", "err", err)
	}
	if sendErr != nil {
		s.retry(ctx, log, j, sendErr, rep)
		return false
	}
	rep.Delivered++
	return true
}

// post 发一次。返回 HTTP 状态码（没拿到响应为 0）与错误（非 2xx 也是错误）。
func (s *AgentWebhookDeliveryService) post(ctx context.Context, hook repository.AgentWebhook, ev repository.AgentEvent,
	body []byte) (int, error) {
	rctx, cancel := context.WithTimeout(ctx, AgentWebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Keel-Webhook/1")
	req.Header.Set("X-Keel-Event", ev.Type)
	req.Header.Set("X-Keel-Event-Id", strconv.FormatInt(ev.ID, 10))
	req.Header.Set("X-Keel-Signature", "sha256="+SignAgentWebhook(hook.Secret, body))
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, agentWebhookErrMax))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}
	return resp.StatusCode, nil
}

func (s *AgentWebhookDeliveryService) retry(ctx context.Context, log *slog.Logger, j repository.Job, cause error,
	rep *AgentWebhookDeliveryReport) {
	err := s.repo.RetryJob(ctx, j.ID, truncateRunes(cause.Error(), agentWebhookErrMax))
	switch {
	case errors.Is(err, repository.ErrJobDeadLettered):
		rep.Dead++
		log.ErrorContext(ctx, "事件投递重试次数用尽，已转死信", "cause", cause)
	case err != nil:
		log.ErrorContext(ctx, "事件投递放回队列失败（回收任务会接手）", "cause", cause, "err", err)
	default:
		rep.Retried++
		log.WarnContext(ctx, "事件投递失败，退避重试", "attempt", j.Attempts, "cause", cause)
	}
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
