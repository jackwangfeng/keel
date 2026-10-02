package service

// 渠道层的任务循环：推对外可售数（channel.listing.push）、整店重算（channel.listing.recompute）。
// 与库存 outbox 同一处境：跑在任何 HTTP 请求之外，按出队那一行的 jobs.merchant_id 建租户上下文
// （tenant_context_test.go 的放行清单）。
//
// 推送一批的做法：一次出队至多 channelBatch 条，按（商家, binding）分组；每组读出 binding 与凭据，
// 对每家门店**重新算一遍**当时的值（入队时算的数不进载荷，见 channel_listing.go 文件头），和上次推出去的一样的
// 直接标完成，其余交给适配器一次推完，逐条回写 channel_listings、标完成或退避重试。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

const (
	channelBatch             = 100
	channelPerTenantInflight = 20
	channelMaxBackoff        = 10 * time.Minute
	channelPollInterval      = time.Second
	channelStuckAfter        = 10 * time.Minute
	channelRetention         = 7 * 24 * time.Hour
	channelPurge             = 1000
)

// RunWorkers 跑渠道层的全部任务循环，直到 ctx 结束。多实例并行安全（SKIP LOCKED）。
func (s *ChannelService) RunWorkers(ctx context.Context) {
	housekeep := time.NewTicker(time.Minute)
	defer housekeep.Stop()
	for {
		n, err := s.WorkOnce(ctx)
		if err != nil {
			s.log.ErrorContext(ctx, "渠道任务这一批没跑起来", "err", err)
		}
		if err == nil && n > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-housekeep.C:
			s.housekeep(ctx)
		case <-time.After(channelPollInterval):
		}
	}
}

func (s *ChannelService) housekeep(ctx context.Context) {
	for _, q := range []string{QueueChannelListingPush, QueueChannelListingRecompute, QueueChannelInbound} {
		if n, err := s.repo.ReapStuckJobs(ctx, q, channelStuckAfter); err != nil {
			s.log.ErrorContext(ctx, "回收卡死的渠道任务失败", "queue", q, "err", err)
		} else if n > 0 {
			s.log.WarnContext(ctx, "回收了卡在执行中的渠道任务", "queue", q, "count", n)
		}
		if _, err := s.repo.PurgeFinishedJobs(ctx, q, channelRetention, channelPurge); err != nil {
			s.log.ErrorContext(ctx, "清理过期的渠道任务失败", "queue", q, "err", err)
		}
	}
}

// WorkOnce 各队列取一批跑完，返回处理的任务数。导出给测试驱动（不等轮询）。
func (s *ChannelService) WorkOnce(ctx context.Context) (int, error) {
	total := 0
	for _, step := range []func(context.Context) (int, error){s.workRecompute, s.workPush, s.workInbound} {
		n, err := step(ctx)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Drain 一直跑到没有到期的任务（测试用）。
func (s *ChannelService) Drain(ctx context.Context) error {
	for i := 0; i < 100; i++ {
		n, err := s.WorkOnce(ctx)
		if err != nil || n == 0 {
			return err
		}
	}
	return errors.New("渠道任务跑了 100 轮还没跑完")
}

func (s *ChannelService) dequeue(ctx context.Context, queue string) ([]repository.Job, error) {
	return s.repo.DequeueJobs(ctx, repository.DequeueRequest{Queue: queue, Limit: channelBatch,
		PerTenantInflight: channelPerTenantInflight, WorkerID: s.workerID})
}

func (s *ChannelService) finish(ctx context.Context, ids ...int64) {
	if len(ids) == 0 {
		return
	}
	if err := s.repo.FinishJobs(ctx, ids); err != nil {
		s.log.WarnContext(ctx, "渠道任务做完了但标完成失败（回收任务会接手，重跑无副作用）", "err", err)
	}
}

func (s *ChannelService) retry(ctx context.Context, j repository.Job, cause error) {
	err := s.repo.RetryJobCapped(ctx, j.ID, cause.Error(), channelMaxBackoff)
	log := s.log.With("merchant_id", j.MerchantID, "job_id", j.ID, "queue", j.Queue, "job_key", j.JobKey, "attempt", j.Attempts)
	switch {
	case errors.Is(err, repository.ErrJobDeadLettered):
		log.ErrorContext(ctx, "渠道任务重试次数用尽，已转死信 —— 这一格没有推上去，请人工处理", "err", cause)
	case err != nil:
		log.ErrorContext(ctx, "渠道任务放回队列失败（回收任务会接手）", "err", cause, "retry_err", err)
	default:
		log.WarnContext(ctx, "渠道任务失败，退避重试", "err", cause)
	}
}

func (s *ChannelService) workRecompute(ctx context.Context) (int, error) {
	jobs, err := s.dequeue(ctx, QueueChannelListingRecompute)
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		var p channelRecomputeJob
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			s.retry(ctx, j, fmt.Errorf("整店重算的载荷解不开: %w", err))
			continue
		}
		if err := s.recomputeStore(tenant.NewContext(ctx, j.MerchantID), p.BindingID, p.StoreID); err != nil {
			s.retry(ctx, j, err)
			continue
		}
		s.finish(ctx, j.ID)
	}
	return len(jobs), nil
}

func (s *ChannelService) workPush(ctx context.Context) (int, error) {
	jobs, err := s.dequeue(ctx, QueueChannelListingPush)
	if err != nil {
		return 0, err
	}
	type key struct{ merchant, binding int64 }
	groups := map[key][]repository.Job{}
	var order []key
	for _, j := range jobs {
		var p channelPushJob
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			s.retry(ctx, j, fmt.Errorf("推送任务的载荷解不开: %w", err))
			continue
		}
		k := key{j.MerchantID, p.BindingID}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], j)
	}
	for _, k := range order {
		s.pushGroup(tenant.NewContext(ctx, k.merchant), k.merchant, k.binding, groups[k])
	}
	return len(jobs), nil
}

// pushGroup 推一个 binding 的一批格子。
func (s *ChannelService) pushGroup(ctx context.Context, merchantID, bindingID int64, jobs []repository.Job) {
	retryAll := func(err error) {
		for _, j := range jobs {
			s.retry(ctx, j, err)
		}
	}
	var b repository.ChannelBinding
	var ab channel.Binding
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		if b, e = tx.GetChannelBinding(ctx, bindingID); e != nil {
			return e
		}
		ab, e = adapterBinding(ctx, tx, merchantID, b)
		return e
	})
	if errors.Is(err, repository.ErrChannelNotFound) || (err == nil && !b.IsActiveOutlet()) {
		// binding 删了或停用了：这些格子不用推了。重新启用时会整店重算。
		ids := make([]int64, len(jobs))
		for i, j := range jobs {
			ids[i] = j.ID
		}
		s.finish(ctx, ids...)
		return
	}
	if err != nil {
		retryAll(err)
		return
	}
	a, ok := s.reg.Lookup(b.Channel)
	outlet, isOutlet := a.(channel.Outlet)
	if !ok || !isOutlet {
		retryAll(fmt.Errorf("%w：%q（或它不是销售渠道）", ErrChannelUnknownKind, b.Channel))
		return
	}

	byStore := map[int64][]repository.Job{}
	var stores []int64
	for _, j := range jobs {
		var p channelPushJob
		_ = json.Unmarshal(j.Payload, &p)
		if _, seen := byStore[p.StoreID]; !seen {
			stores = append(stores, p.StoreID)
		}
		byStore[p.StoreID] = append(byStore[p.StoreID], j)
	}
	for _, store := range stores {
		s.pushStore(ctx, outlet, ab, store, byStore[store])
	}
}

func (s *ChannelService) pushStore(ctx context.Context, outlet channel.Outlet, ab channel.Binding, storeID int64, jobs []repository.Job) {
	jobBySKU := map[int64]repository.Job{}
	skus := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		var p channelPushJob
		_ = json.Unmarshal(j.Payload, &p)
		jobBySKU[p.SKUID] = j
		skus = append(skus, p.SKUID)
	}
	targets, err := s.computeTargets(ctx, storeID, skus, ab.ID)
	if err != nil {
		for _, j := range jobs {
			s.retry(ctx, j, err)
		}
		return
	}
	var ls []channel.Listing
	var pending []listingTarget
	var done []int64
	have := map[int64]bool{}
	for _, t := range targets {
		have[t.skuID] = true
		if t.unchanged() {
			done = append(done, jobBySKU[t.skuID].ID)
			continue
		}
		var prevQty *int32
		version := int64(1)
		if t.prev != nil {
			q := t.prev.PublishedQty
			prevQty, version = &q, t.prev.Version+1
		}
		ls = append(ls, channel.Listing{StoreID: storeID, SKUID: t.skuID, ExternalStoreID: t.binding.ExternalStoreID,
			ExternalSKUID: t.external.ExternalID, Extra: t.external.Extra, Qty: t.qty, PrevQty: prevQty, PriceCents: t.price,
			IdemKey: fmt.Sprintf("%d:%d:%d:%d", ab.ID, storeID, t.skuID, version)})
		pending = append(pending, t)
	}
	// 算不出来的格子（门店映射删了、SKU 映射删了）：没东西可推。
	for sku, j := range jobBySKU {
		if !have[sku] {
			done = append(done, j.ID)
		}
	}
	s.finish(ctx, done...)
	if len(ls) == 0 {
		return
	}
	results, err := outlet.PushListings(ctx, ab, ls)
	if err == nil && len(results) != len(ls) {
		err = fmt.Errorf("适配器回了 %d 条结果，推了 %d 条", len(results), len(ls))
	}
	if err != nil {
		if errors.Is(err, channel.ErrCredentials) {
			s.markCredentialsBroken(ctx, ab.ID, err)
		}
		for _, t := range pending {
			s.retry(ctx, jobBySKU[t.skuID], err)
		}
		return
	}
	for i, r := range results {
		t, j := pending[i], jobBySKU[pending[i].skuID]
		if r.Err == nil {
			if werr := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
				_, e := tx.RecordChannelListing(ctx, repository.ChannelListing{BindingID: ab.ID, StoreID: storeID,
					SKUID: t.skuID, PublishedQty: t.qty, PublishedCents: t.price})
				return e
			}); werr != nil {
				// 推上去了但没记下：重推一次同样的值（幂等键相同），无害。
				s.retry(ctx, j, werr)
				continue
			}
			s.finish(ctx, j.ID)
			continue
		}
		msg := r.Err.Error()
		if r.Conflict && r.ObservedQty != nil {
			// 渠道上的数被人改过：记下渠道上的现值（下一次 CAS 以它为准），对销售渠道 keel 是权威，重推覆盖。
			msg = fmt.Sprintf("渠道上的可售数被改成了 %d，按 keel 的 %d 覆盖", *r.ObservedQty, t.qty)
			_ = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
				if _, e := tx.RecordChannelListing(ctx, repository.ChannelListing{BindingID: ab.ID, StoreID: storeID,
					SKUID: t.skuID, PublishedQty: *r.ObservedQty, PublishedCents: t.price}); e != nil {
					return e
				}
				return tx.SetChannelListingError(ctx, ab.ID, storeID, t.skuID, msg)
			})
		} else {
			_ = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
				return tx.SetChannelListingError(ctx, ab.ID, storeID, t.skuID, msg)
			})
		}
		s.retry(ctx, j, errors.New(msg))
	}
}

// markCredentialsBroken 把 binding 标成「凭据失效」：停推送（不再是启用中的销售渠道），等人重新配凭据再启用。
func (s *ChannelService) markCredentialsBroken(ctx context.Context, bindingID int64, cause error) {
	st := repository.ChannelBindingCredentialsX
	if _, err := s.UpdateBinding(ctx, bindingID, ChannelBindingUpdate{Status: &st}); err != nil {
		s.log.ErrorContext(ctx, "标记渠道凭据失效失败", "binding_id", bindingID, "err", err)
		return
	}
	s.log.ErrorContext(ctx, "渠道凭据失效，已停推送，请重新配置凭据后启用", "binding_id", bindingID, "err", cause)
}

// workInbound 取一批回调事件按类别分发。
func (s *ChannelService) workInbound(ctx context.Context) (int, error) {
	jobs, err := s.dequeue(ctx, QueueChannelInbound)
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		s.handleInbound(tenant.NewContext(ctx, j.MerchantID), j)
	}
	return len(jobs), nil
}

