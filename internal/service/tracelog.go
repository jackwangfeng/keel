package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/traceid"
)

// TraceLogService 是访问日志开关的后端（internal/traceid/access.go）。
//
// **里面没有任何业务逻辑** —— 采样、白名单、计数都在traceid 包里，那里有并发
// 原语与测试。这个 service 存在的唯一原因是**权限**：日志采样率是进程级的
// 状态，而 staffAuth 只验「这个人是谁」，不验「这个人能不能做这件事」。
// 少了这一层，任何一个店铺运营都能把采样率打到 100，把生产的日志量放大
// 几十倍—— 而这类动作没人看着。
type TraceLogService struct{}

// NewTraceLogService 造开关的后端。
func NewTraceLogService() *TraceLogService { return &TraceLogService{} }

// TraceLogState 是开关的当前状态。
type TraceLogState struct {
	traceid.Stats
	SampleRateHint string
	ForcedHint     string
}

// Get 读当前状态。**只给管理员**。
//
// 这是一个读动作，而读也要判权限：采样率与白名单是进程级状态，运营从它能推出
// 「这个进程现在在记什么、哪几个 trace_id 被点名了」—— 后者本身就是一份
// 「有人在查什么」的清单。所以它跟写放在同一个门后面，不因为「只是读」而放开。
func (s *TraceLogService) Get(ctx context.Context) (TraceLogState, error) {
	if _, err := requireTraceLogAdmin(ctx); err != nil {
		return TraceLogState{}, err
	}
	return TraceLogState{
		Stats:          traceid.GetStats(),
		SampleRateHint: "百分之一为单位：25 = 25%，100 = 全量，0 = 只记慢的、错的、被点名的",
		ForcedHint:     "白名单里的 trace_id 无论采样率多少都全量记；ForceOn=false 移除",
	}, nil
}

// Set 按请求体改开关。**只给管理员**。
//
// 两项都是可选的，只有带了的那个生效 —— 于是「只改白名单不改采样率」是一次
// 请求，而不是先 GET 再 POST 带上当前值（那在两次请求之间会被别人改掉）。
func (s *TraceLogService) Set(ctx context.Context, sampleRate *int64, traceID string, force *bool) (TraceLogState, error) {
	if _, err := requireTraceLogAdmin(ctx); err != nil {
		return TraceLogState{}, err
	}
	if sampleRate != nil {
		traceid.SetSampleRate(*sampleRate)
	}
	if force != nil {
		id := strings.TrimSpace(traceID)
		var ok bool
		if *force {
			ok = traceid.ForceOn(id)
		} else {
			ok = traceid.ForceOff(id)
		}
		if !ok {
			// 两种false 的原因不一样，而调用方需要知道是哪一种：
			// 号不合法（永远移不掉），还是它本来就不在白名单里（移了但没得可移）。
			detail := "trace_id 必须是 32 位小写十六进制"
			if traceid.Normalize(id) != "" {
				detail = "这个 trace_id 不在白名单里，没有可移除的"
			}
			return TraceLogState{}, fmt.Errorf("%w: trace_id 这一项没生效：%s", ErrStaffBadRequest, detail)
		}
	}
	return s.Get(ctx)
}

// requireTraceLogAdmin 放行管理员。
//
// 不用 requireMerchantAdmin（设置默认门店那一档）：那个函数顺带管了「设默认门店」
// 与「店铺设置」，而本条的理由是**影响面是整个进程而不是这家店** —— 白名单与
// 采样率都不按租户分。两者判据不同，所以单独写一个而不是复用。
func requireTraceLogAdmin(ctx context.Context) (auth.StaffIdentity, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if !id.IsAdmin() {
		return auth.StaffIdentity{}, fmt.Errorf("%w: 访问日志的开关只给管理员（它改的是整个进程的日志量）", ErrRoleForbidden)
	}
	return id, nil
}