package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/keel/keel/internal/repository"
)

// 搜索行为回传：POST /search/events（语义检索层 §9.2，数据模型 §8 search_logs）。
//
// 一次检索写一行 search_logs（search.go 文件头第六节），这里把之后发生的
// 点击 / 加购 / 下单回填到那一行的 clicked_id / carted_id / ordered_id 上。
// 没有这三列，§9.2 的 CTR@10、搜索→加购率、搜索→下单转化率一个都算不出来
// （scripts/search_metrics.sql 就是算它们的那条查询）。
//
// 三条规矩，每条都有一个执行者（handler/search_event_test.go）：
//
//	· **只认这次检索真正返回过的商品**。接口是公开的（与 /search 一样，
//	  没登录的访客也在点），接受任意 product_id 的话，任何人拿一个 trace_id
//	  就能把某件商品的「点击」刷上去。判据是 search_logs.ranked_ids ——
//	  返回给客户端的那几条，不是 recall_ids（融合后的全部候选，客户端没见过）。
//	· **每一列首次写入为准**。再报一次同一种事件（不论是不是同一件商品）
//	  不覆盖、照样成功。于是重放与首次效果相同，重试是安全的 —— 天然幂等，
//	  契约因此不收 Idempotency-Key（理由在契约那条接口的描述与
//	  scripts/check_openapi.py 的豁免表里）。
//	· **租户隔离靠 RLS**。两条语句跑在同一个租户事务里、一个 merchant_id 都不写；
//	  别家店的 trace_id 在这里被过滤成「查无此行」，与不存在的 trace_id
//	  是同一个 ErrSearchTraceNotFound —— 分开报就是在替调用方做跨店枚举。

// SearchEvent 是契约里 /search/events 的 event 枚举。
type SearchEvent string

const (
	SearchEventClick   SearchEvent = "click"
	SearchEventAddCart SearchEvent = "add_cart"
	SearchEventOrder   SearchEvent = "order"
)

// behaviorOf 把事件翻成它要回填的那一列。一种事件只对一列，没有第四种。
func behaviorOf(e SearchEvent) (repository.SearchBehavior, bool) {
	switch e {
	case SearchEventClick:
		return repository.BehaviorClicked, true
	case SearchEventAddCart:
		return repository.BehaviorCarted, true
	case SearchEventOrder:
		return repository.BehaviorOrdered, true
	}
	return 0, false
}

var (
	// ErrSearchTraceNotFound：trace_id 不存在，或那一行属于别的租户。handler 映射成 404。
	ErrSearchTraceNotFound = errors.New("trace_id 不存在或不属于当前店铺")

	// ErrProductNotInSearchResults：product_id 不在这次检索返回的那几条里。
	// handler 映射成 422 —— 一列都不写，这是防刷指标的那道闸门。
	ErrProductNotInSearchResults = errors.New("product_id 不是这次检索返回过的商品")

	// ErrInvalidSearchEvent：event 不在枚举里，或 trace_id 的形状不对。handler 映射成 422。
	ErrInvalidSearchEvent = errors.New("回传请求不合法")
)

// traceIDPattern 与契约里 trace_id 的 pattern 逐字一致，也与 newTraceID 生成的形状一致。
//
// 先判形状再查库：一个形状都不对的 id 不可能是我们发出去的，
// 为它打一次数据库（哪怕是一次点查）没有意义。
var traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// RecordSearchEvent 把一次行为回填到 traceID 那一行上。
//
// 返回 (true, nil) 表示这一次写下了；(false, nil) 表示那一列此前已经有值、
// 这一次没有覆盖（首次为准）。handler 对两者回同一个 204 —— 契约写明不区分，
// 布尔值留给测试与日志。
func (s *SearchService) RecordSearchEvent(ctx context.Context, traceID string,
	event SearchEvent, productID int64) (bool, error) {

	col, ok := behaviorOf(event)
	if !ok {
		return false, fmt.Errorf("%w：event = %q，只认 click / add_cart / order",
			ErrInvalidSearchEvent, event)
	}
	if productID <= 0 {
		// 契约里 product_id 是必填；缺了的话生成类型解出来是 0，而 0 不是任何商品。
		return false, fmt.Errorf("%w：缺 product_id", ErrInvalidSearchEvent)
	}
	if !traceIDPattern.MatchString(traceID) {
		return false, fmt.Errorf("%w：trace_id 必须是 32 个小写十六进制字符", ErrInvalidSearchEvent)
	}

	var written bool
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		ranked, err := tx.SearchLogRankedIDs(ctx, traceID)
		if errors.Is(err, repository.ErrSearchLogNotFound) {
			return ErrSearchTraceNotFound
		}
		if err != nil {
			return err
		}
		// ranked_ids 在检索那一刻写定、之后没有任何路径改它，
		// 所以「先读再判再写」之间不存在它变了的窗口。
		if !slices.Contains(ranked, productID) {
			return fmt.Errorf("%w：product_id = %d", ErrProductNotInSearchResults, productID)
		}
		written, err = tx.SetSearchLogBehavior(ctx, traceID, col, productID)
		return err
	})
	if err != nil {
		return false, err
	}
	return written, nil
}
