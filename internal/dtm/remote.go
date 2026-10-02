package dtm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Remote 是独立部署的 dtmrs 集群的客户端（微服务形态，docs/电商系统-微服务部署方案.md 第四节）。
//
// 走 dtmrs 的 HTTP API（与 DTM 同协议）：submit / prepare / abort / query / subscribe，请求带
// `Authorization: Bearer <令牌>`。协调器本身无状态（状态在它的库里），挂了重启、换实例都不影响这里——
// 地址写的是服务名（负载均衡后的那个），不是某个实例。
//
// 与嵌入式 *TC 的语义对齐：
//
//	· 每实例的在途上限照旧在客户端这一侧（与 *TC 的两个信号量同样的预算），协调器卡住时请求在这里排队，
//	  而不是无界地堆到协调器上；
//	· WaitFinal 用 query 轮询终态（嵌入式也是等终态），超时返回错误；
//	· HTTP 200 + {"dtm_result":"FAILURE"} 是「状态不允许」（dtmrs 与 DTM 兼容的历史行为），同样当错误。
type Remote struct {
	base   string
	token  string
	hc     *http.Client
	sem    chan struct{}
	msgSem chan struct{}
}

// RemoteConfig 是 Remote 的配置。
type RemoteConfig struct {
	Endpoint    string        // 形如 http://dtmrs:36789
	Token       string        // DTMRS_AUTH_TOKEN；空着拒绝构造（独立部署必须开认证）
	MaxInflight int           // 0 用 DefaultMaxInflight
	Timeout     time.Duration // 单次 HTTP 请求的上限，0 用 10 秒
	HTTPClient  *http.Client  // 测试注入
}

// NewRemote 建客户端。
func NewRemote(cfg RemoteConfig) (*Remote, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return nil, fmt.Errorf("协调器地址 %q 不是 http(s)://host[:port] 形式", cfg.Endpoint)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("独立部署的协调器必须带令牌（dtmrs 的 DTMRS_AUTH_TOKEN）：内网里任何人都能提交、作废事务的协调器不能上线")
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = DefaultMaxInflight
	}
	hc := cfg.HTTPClient
	if hc == nil {
		t := cfg.Timeout
		if t <= 0 {
			t = 10 * time.Second
		}
		hc = &http.Client{Timeout: t, Transport: remoteTransport(2 * cfg.MaxInflight)}
	}
	return &Remote{base: u.String(), token: cfg.Token, hc: hc,
		sem: make(chan struct{}, cfg.MaxInflight), msgSem: make(chan struct{}, cfg.MaxInflight)}, nil
}

// remoteTransport 给协调器客户端一个够大的空闲连接池。
//
// 默认 Transport 每个 host 只留 2 条空闲连接：在途请求一多（两个信号量合起来最多 2×MaxInflight 条），
// 用完的连接大半被关掉、下一条再新建，关掉的那一头进 TIME_WAIT。拆分形态压测下单 c=32 / 64 时，
// 本机临时端口被耗光（dial: cannot assign requested address），最多 8% 的下单回 500
// （docs/性能压测-2026-10.md 第十二节）。空闲池按在途上限开，连接就一直复用。
// 代理行为与默认 Transport 相同（认 HTTP(S)_PROXY / NO_PROXY），与 rpc.newTransport 同一个写法。
func remoteTransport(perHost int) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = perHost
	t.MaxIdleConns = perHost
	return t
}

type dtmResult struct {
	Result  string `json:"dtm_result"`
	Message string `json:"message"`
}

// do 发一次请求。2xx 且（有 dtm_result 时）不是 FAILURE 才算成功；out 非 nil 时把响应体解进去。
func (r *Remote) do(method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, r.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("调协调器 %s %s 失败: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("读协调器响应失败: %w", err)
	}
	var res dtmResult
	_ = json.Unmarshal(raw, &res)
	if resp.StatusCode/100 != 2 || res.Result == "FAILURE" {
		msg := res.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
			if len(msg) > 200 {
				msg = msg[:200] + "…"
			}
		}
		return resp.StatusCode, fmt.Errorf("协调器拒绝 %s（HTTP %d）: %s", path, resp.StatusCode, msg)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("解析协调器响应失败: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (r *Remote) SubmitSaga(gid, stepsJSON string) error {
	r.sem <- struct{}{}
	defer func() { <-r.sem }()
	if !json.Valid([]byte(stepsJSON)) {
		return fmt.Errorf("提交 %s 失败: 步骤不是合法 JSON", gid)
	}
	if _, err := r.do(http.MethodPost, "/api/dtmsvr/submit", map[string]any{
		"gid": gid, "trans_type": "saga", "steps": json.RawMessage(stepsJSON),
	}, nil); err != nil {
		return fmt.Errorf("提交 %s 失败: %w", gid, err)
	}
	return nil
}

func (r *Remote) SubmitSagaSteps(gid string, steps ...Step) error {
	s, err := StepsJSON(steps...)
	if err != nil {
		return fmt.Errorf("提交 %s 失败: %w", gid, err)
	}
	return r.SubmitSaga(gid, s)
}

// WaitFinal 轮询到终态（succeed / failed）或超时。间隔从 10 ms 起翻倍、封顶 200 ms：
// 下单的 SAGA 通常几十毫秒就完，开头密一点不让请求白等。
func (r *Remote) WaitFinal(gid string, timeoutMS int) (string, error) {
	r.sem <- struct{}{}
	defer func() { <-r.sem }()
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)
	wait := 10 * time.Millisecond
	for {
		st, err := r.status(gid)
		if err == nil && (st == "succeed" || st == "failed") {
			return st, nil
		}
		if time.Now().Add(wait).After(deadline) {
			if err != nil {
				return "", fmt.Errorf("等 %s 的终态超时（%d ms），最后一次查询出错: %w", gid, timeoutMS, err)
			}
			return "", fmt.Errorf("等 %s 的终态超时（%d ms），当前 %s", gid, timeoutMS, st)
		}
		time.Sleep(wait)
		wait = min(2*wait, 200*time.Millisecond)
	}
}

func (r *Remote) PrepareMsg(gid string, actions []string, queryPrepared string, graceSecs int) error {
	return r.PrepareMsgEx(gid, actions, nil, queryPrepared, graceSecs, false)
}

func (r *Remote) PrepareMsgEx(gid string, actions, payloads []string, queryPrepared string, graceSecs int,
	allowEmptyTopic bool) error {
	if err := checkMsg(actions, payloads, queryPrepared); err != nil {
		return err
	}
	r.msgSem <- struct{}{}
	defer func() { <-r.msgSem }()
	body := map[string]any{
		"gid": gid, "trans_type": "msg", "actions": actions, "query_prepared": queryPrepared,
		"allow_empty_topic": allowEmptyTopic,
	}
	if graceSecs > 0 {
		body["grace_secs"] = graceSecs
	}
	if payloads != nil {
		body["payloads"] = payloads
	}
	if _, err := r.do(http.MethodPost, "/api/dtmsvr/prepare", body, nil); err != nil {
		return fmt.Errorf("登记消息 %s 失败: %w", gid, err)
	}
	return nil
}

func (r *Remote) SubmitMsg(gid string) error {
	r.msgSem <- struct{}{}
	defer func() { <-r.msgSem }()
	if _, err := r.do(http.MethodPost, "/api/dtmsvr/submit", map[string]any{"gid": gid, "trans_type": "msg"}, nil); err != nil {
		return fmt.Errorf("提交消息 %s 失败: %w", gid, err)
	}
	return nil
}

func (r *Remote) AbortMsg(gid string) error {
	r.msgSem <- struct{}{}
	defer func() { <-r.msgSem }()
	if _, err := r.do(http.MethodPost, "/api/dtmsvr/abort", map[string]any{"gid": gid}, nil); err != nil {
		return fmt.Errorf("作废消息 %s 失败: %w", gid, err)
	}
	return nil
}

func (r *Remote) Status(gid string) (string, error) {
	r.msgSem <- struct{}{}
	defer func() { <-r.msgSem }()
	return r.status(gid)
}

func (r *Remote) status(gid string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	if _, err := r.do(http.MethodGet, "/api/dtmsvr/query?gid="+url.QueryEscape(gid), nil, &out); err != nil {
		return "", err
	}
	if out.Status == "" {
		return "", fmt.Errorf("协调器对 %s 的查询没有返回 status", gid)
	}
	return out.Status, nil
}

// Close 无事可做：客户端不持有任何需要释放的东西。
func (r *Remote) Close() {}

// Subscribe 把 url 订阅到 topic 上（幂等：已经订过当成功）。remark 记谁订的，方便在管理台里认。
func (r *Remote) Subscribe(topic, subscriberURL, remark string) error {
	q := url.Values{"topic": {topic}, "url": {subscriberURL}, "remark": {remark}}
	_, err := r.do(http.MethodGet, "/api/dtmsvr/subscribe?"+q.Encode(), nil, nil)
	if err != nil && strings.Contains(err.Error(), "this url exists") {
		return nil
	}
	return err
}
