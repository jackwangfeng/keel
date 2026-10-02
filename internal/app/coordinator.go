package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/keel/keel/internal/dtm"
)

// 事务协调器的两种形态（docs/电商系统-微服务部署方案.md）：
//
//	· 嵌入式（KEEL_DTM_DSN）：协调器跑在本进程里，分支全是 local:// 函数。单体（KEEL_ROLE=all）用。
//	· 独立部署（KEEL_DTM_SERVER + KEEL_DTM_TOKEN）：dtmrs 集群单独部署，本进程只是客户端；协调器经内网
//	  回调各服务的 /internal/v1/saga/<名字>，所以本服务要告诉它自己的内网地址（KEEL_SELF_URL，写服务名，
//	  不写实例 IP——多实例共用一个地址，由平台负载均衡）。微服务（KEEL_ROLE=core / inventory）只许用这一种。
const (
	EnvDTMServer = "KEEL_DTM_SERVER"
	EnvDTMToken  = "KEEL_DTM_TOKEN"
	EnvSelfURL   = "KEEL_SELF_URL"
)

// remoteDTM 报告是否用独立部署的协调器。
func (s SplitConfig) remoteDTM() bool { return s.DTMServer != "" }

// validateDTM 是 validate 里协调器那一段。
func (s SplitConfig) validateDTM() error {
	if s.DTMServer != "" && s.DTMDSN != "" {
		return fmt.Errorf("%s 与 %s 只能配一个：前者是独立部署的协调器，后者是嵌在本进程里的协调器的存储", EnvDTMServer, EnvDTMDSN)
	}
	switch s.Role {
	case RoleCore:
		if s.DTMServer == "" {
			return fmt.Errorf("%s=core 必须用独立部署的协调器（%s + %s）：微服务形态下每个服务各嵌一个协调器，"+
				"订阅表、事务表散在各服务里，多实例时还得各自共用一个库（docs/电商系统-微服务部署方案.md 第三节）", EnvRole, EnvDTMServer, EnvDTMToken)
		}
	case RoleInventory:
		if s.DTMDSN != "" {
			return fmt.Errorf("%s=inventory 不再嵌协调器（%s 删掉）：跨 0 通知经独立部署的协调器发（%s），"+
				"docs/电商系统-微服务部署方案.md", EnvRole, EnvDTMDSN, EnvDTMServer)
		}
	}
	if s.DTMServer == "" {
		return nil
	}
	if strings.TrimSpace(s.DTMToken) == "" {
		return fmt.Errorf("配了 %s 就必须配 %s（dtmrs 的 DTMRS_AUTH_TOKEN）", EnvDTMServer, EnvDTMToken)
	}
	if s.InternalAddr == "" {
		return fmt.Errorf("配了 %s 就必须配 %s：协调器要经内网端口回调本服务的分支", EnvDTMServer, EnvInternalAddr)
	}
	u, err := url.Parse(s.SelfURL)
	if s.SelfURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return fmt.Errorf("配了 %s 就必须配 %s（本服务内网端口的服务地址，如 http://app:8091；写服务名，不写实例 IP），现在是 %q",
			EnvDTMServer, EnvSelfURL, s.SelfURL)
	}
	if _, err := dtm.NewRemote(dtm.RemoteConfig{Endpoint: s.DTMServer, Token: s.DTMToken}); err != nil {
		return fmt.Errorf("%s: %w", EnvDTMServer, err)
	}
	return nil
}

// selfResolver 解析本服务自己的分支：嵌入式是 local://，独立协调器是 KEEL_SELF_URL 上的 HTTP 地址（带分支令牌）。
func (s SplitConfig) selfResolver() (dtm.BranchResolver, error) {
	if !s.remoteDTM() {
		return dtm.BranchResolver{}, nil
	}
	return dtm.NewBranchResolver(s.SelfURL, s.InternalSecret)
}

func (s SplitConfig) remoteCoordinator() (*dtm.Remote, error) {
	return dtm.NewRemote(dtm.RemoteConfig{Endpoint: s.DTMServer, Token: s.DTMToken})
}

// subscribeUntilDone 在后台把 url 订阅到 topic 上，失败退避重试（1 秒起翻倍、封顶 1 分钟），直到成功或 ctx 结束。
// 不阻塞启动：没订上之前发布方的 allow_empty_topic 让消息直接完成，漏的由全量刷新兜住。
func subscribeUntilDone(ctx context.Context, r *dtm.Remote, topic, subscriberURL, remark string) {
	go func() {
		wait := time.Second
		for {
			err := r.Subscribe(topic, subscriberURL, remark)
			if err == nil {
				slog.InfoContext(ctx, "已在协调器上订阅主题", "topic", topic, "remark", remark)
				return
			}
			slog.WarnContext(ctx, "订阅主题失败，稍后重试（这期间发到该主题的消息没人收，由全量刷新兜住）",
				"topic", topic, "err", err, "retry_in", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(2*wait, time.Minute)
		}
	}()
}

// exBranches 把 BranchFunc 表转成 BranchFuncEx 表（嵌入式注册与内网挂载都统一走 Ex）。
func exBranches(m map[string]dtm.BranchFunc) map[string]dtm.BranchFuncEx {
	out := make(map[string]dtm.BranchFuncEx, len(m))
	for k, fn := range m {
		out[k] = dtm.Ex(fn)
	}
	return out
}

// mergeEx 合并几张分支表，重名直接 panic（装配期的编程错误）。
func mergeEx(ms ...map[string]dtm.BranchFuncEx) map[string]dtm.BranchFuncEx {
	out := map[string]dtm.BranchFuncEx{}
	for _, m := range ms {
		for k, fn := range m {
			if _, dup := out[k]; dup {
				panic("分支 " + k + " 重名")
			}
			out[k] = fn
		}
	}
	return out
}
