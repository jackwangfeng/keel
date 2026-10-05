package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/api"
	reqoutcome "github.com/keel/keel/internal/outcome"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/traceid"
)

// DefaultTimeout 是单次内网调用的上限。库存类调用是一两条 SQL，正常在毫秒级；
// 5 秒已经是「对面出事了」。它必须有界：调用方多半在一个公网请求里，
// 无界等待会把公网那一侧的连接一起拖住。
const DefaultTimeout = 5 * time.Second

// DefaultReadTimeout 是 KEEL_ROLE=core 装配库存客户端时读请求的默认上限（app 里配，
// 见 WithReadTimeout）。读的调用方是公网页面（商品列表的 in_stock、试算、购物车、检索），
// 库存服务卡住时它们要降级而不是陪着等：5 秒的写超时放在读上，就是每个页面请求多等 5 秒。
// 800ms 对一两条 SQL 的读仍然留了两个数量级的余量。
const DefaultReadTimeout = 800 * time.Millisecond

// 连接复用。默认 Transport 的 MaxIdleConnsPerHost 是 2：core 到库存服务只有一个 host，
// 并发一上来，第 3 个起的请求每次都新建 TCP 连接、用完就关，TIME_WAIT 堆在 core 一侧，
// 延迟里多一次握手。64 足够覆盖一个实例的正常并发。
const (
	maxIdleConnsPerHost = 64
	dialTimeout         = 2 * time.Second // 内网建连，超过这个数就是对面不在
	tlsHandshakeTimeout = 3 * time.Second
	idleConnTimeout     = 90 * time.Second
)

// maxResponse 是响应体的上限，理由与 maxBody 相同。
const maxResponse = 1 << 20

// 错误的哨兵值，用 errors.Is 判断。
//
// 分得这么细，是因为调用方对它们的处置完全不同，而最要紧的一刀是
// **ErrUnknown 与其余所有**：
//
//   - 4xx（包括下面几个）是**确定失败**：对面明确拒绝了，什么都没做。
//     调用方可以放心地告诉用户失败、或者在 SAGA 里回 Failure 触发补偿。
//   - ErrUnknown 是**不知道**：连不上、超时、5xx、2xx 却解不出响应。
//     请求可能根本没到，也可能对面已经提交了事务、只是回包丢了。
//     把它当成失败去补偿，补的可能是一件已经发生了两次的事，或者从未发生的事。
//     SAGA 里它对应 dtm.Unknown（让协调器重试），公网请求里它对应 5xx。
//
// 客户端**自己不重试**：重试要不要做、做几次、要不要换幂等键，只有调用方知道；
// 而协调器与 jobs 本来就在重试。两层都重试的话，一次抖动会被放大成
// 「重试次数的乘积」次调用。
var (
	ErrNotFound      = errors.New("rpc: 404 不存在")
	ErrConflict      = errors.New("rpc: 409 冲突")
	ErrUnprocessable = errors.New("rpc: 422 无法处理")
	ErrBadRequest    = errors.New("rpc: 400 请求不合法")
	ErrUnauthorized  = errors.New("rpc: 401 签名被拒（两边 KEEL_INTERNAL_SECRET 不一致？）")
	// ErrRejected 是其余所有 4xx：确定失败，但不属于上面任何一类。
	ErrRejected = errors.New("rpc: 请求被拒")
	ErrUnknown  = errors.New("rpc: 结果未知")
	// ErrCircuitOpen 是熔断器打开时的失败：请求**根本没发出去**，所以它是确定失败，
	// 不是 ErrUnknown（IsUnknown 为 false）。只有 ReadJSON 会返回它（breaker.go）。
	// 读的调用方把它与 ErrUnknown 一样当成「对面暂时不在」降级（inventory.Remote.read）。
	ErrCircuitOpen = errors.New("rpc: 熔断器打开，请求未发出")
	// ErrBusy 是对面回了 503 busy（problem type https://keel.dev/problems/busy）：对面的数据库
	// 等锁超时（55P03）或语句超时（57014），而那个请求此前什么都没落地 —— 对面的事务确定回滚了，
	// 所以它是**确定失败**，不是 ErrUnknown（IsUnknown 为假）。调用方可以放心地报「没有生效、可以重试」。
	//
	// 它包着 outcome.ErrPeerBusy：core 一侧的兜底（problem.Write）认的是那个哨兵，
	// 这样一次库存写撞上 busy，core 的公网接口不必逐个认识 rpc 也能回 busy。
	//
	// 只认 503 + 这个 type：别的 503（内网服务未就绪、反向代理回的 503）仍是结果未知 ——
	// 那些情况下请求到没到对面的业务代码是不知道的。
	ErrBusy = fmt.Errorf("rpc: 对面繁忙、这次没有生效: %w", reqoutcome.ErrPeerBusy)
)

// typeBusy 与 problem.TypeBusy 逐字相同。不 import problem 是为了不让客户端包依赖 gin 那一侧的包；
// rpc 的测试里有一条钉住两者相等。
const typeBusy = "https://keel.dev/problems/busy"

// Error 是一次失败调用的全部信息。
type Error struct {
	// Status 是 HTTP 状态码；传输层失败（连不上、超时）时为 0。
	Status int
	// Type / Title 取自对面的 Problem 响应（有的话）。409 靠 Type 区分是哪种冲突。
	Type  string
	Title string
	// Method / Path 只为日志与错误信息。
	Method, Path string

	kind error // 上面的某个哨兵
	err  error // 传输层的原始错误（Status 为 0 时）
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %v", e.Method, e.Path, e.kind)
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d", e.Status)
		if e.Type != "" {
			fmt.Fprintf(&b, " %s", e.Type)
		}
		b.WriteString(")")
	}
	if e.err != nil {
		fmt.Fprintf(&b, ": %v", e.err)
	}
	return b.String()
}

// Is 让 errors.Is(err, rpc.ErrConflict) 之类成立；哨兵自己包着的东西（ErrBusy 里的
// outcome.ErrPeerBusy）也算。
func (e *Error) Is(target error) bool { return target == e.kind || errors.Is(e.kind, target) }

// Unwrap 暴露传输层原始错误（比如 context.DeadlineExceeded），方便调用方记日志。
func (e *Error) Unwrap() error { return e.err }

// IsUnknown 回答「这次调用的结果是不是未知」。err == nil 时为 false。
func IsUnknown(err error) bool { return errors.Is(err, ErrUnknown) }

// Client 是签名 HTTP 客户端。零值不可用，用 NewClient 建。并发安全。
type Client struct {
	base   *url.URL
	secret string
	http   *http.Client

	timeout     time.Duration // PostJSON / GetJSON（写、以及没有分读写的调用）
	readTimeout time.Duration // ReadJSON
	brk         *breaker      // 只给 ReadJSON；nil = 不熔断
}

// Option 是 NewClient 的可选项。
type Option func(*Client)

// WithReadTimeout 给 ReadJSON 一个单独的（通常更短的）上限。d <= 0 时不改（沿用 timeout）。
func WithReadTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.readTimeout = d
		}
	}
}

// WithBreaker 给 ReadJSON 装熔断器：连续 failures 次结果未知就打开，cooldown 之后半开探测一次。
// failures <= 0 或 cooldown <= 0 时不装。语义见 breaker.go。
func WithBreaker(failures int, cooldown time.Duration) Option {
	return func(c *Client) {
		if failures > 0 && cooldown > 0 {
			c.brk = newBreaker(failures, cooldown)
		}
	}
}

// NewClient 建客户端。baseURL 形如 "http://inventory:8090"（KEEL_INVENTORY_URL）。
// timeout <= 0 时用 DefaultTimeout。不给选项时 ReadJSON 与 PostJSON 同一个超时、不熔断
// （测试与今天的行为）；KEEL_ROLE=core 的装配在 app 里按环境变量加上读超时与熔断器。
func NewClient(baseURL, secret string, timeout time.Duration, opts ...Option) (*Client, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("%s 至少要 %d 字节", EnvInternalSecret, MinSecretLen)
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("内网服务地址 %q 不是 http(s)://host[:port] 形式", baseURL)
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// 超时不放在 http.Client.Timeout 上，而是每次调用按读 / 写各自挂在 ctx 上（do）：
	// 同一个客户端要给读与写两个不同的上限，而 http.Client.Timeout 只有一个。
	c := &Client{base: u, secret: secret, http: &http.Client{Transport: newTransport()},
		timeout: timeout, readTimeout: timeout}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func newTransport() *http.Transport {
	return &http.Transport{
		// 与默认 Transport 一样认 HTTP(S)_PROXY / NO_PROXY：换 Transport 不该顺手改掉代理行为。
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        2 * maxIdleConnsPerHost,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,
		TLSHandshakeTimeout: tlsHandshakeTimeout,
	}
}

// ReadJSON 是**读**请求：与 PostJSON 同一个形状（POST + JSON，库存服务的读也是 POST，
// 见 inventory/http.go 文件头），但用读超时，并经过熔断器。
//
// 熔断器打开时直接返回 ErrCircuitOpen（请求没发出去）。错误分类与 PostJSON 相同：
// 结果未知仍是 ErrUnknown，4xx 仍是各自的确定失败 —— 熔断器只多了「没发」这一种。
func (c *Client) ReadJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("rpc: 编码请求体失败: %w", err)
	}
	if c.brk == nil {
		return c.do(ctx, c.readTimeout, http.MethodPost, path, nil, body, out)
	}
	if !c.brk.allow() {
		return &Error{Method: http.MethodPost, Path: path, kind: ErrCircuitOpen}
	}
	err = c.do(ctx, c.readTimeout, http.MethodPost, path, nil, body, out)
	switch {
	case err == nil:
		c.brk.done(outcomeSuccess)
	case ctx.Err() != nil:
		// 调用方自己取消 / 到了调用方的截止：不说明对面怎样。
		c.brk.done(outcomeIgnored)
	case IsUnknown(err):
		c.brk.done(outcomeFailure)
	default:
		// 4xx 与 busy：对面回答了。busy 不算失败：对面进程在、只是它的库里那一行正被锁着，
		// 熔断它只会把所有人的库存读一起降级掉，而那一行锁几秒就放了。（实际上读几乎碰不到
		// busy：读超时 800ms 远短于对面的 lock_timeout / statement_timeout，先到的是这边的超时。）
		c.brk.done(outcomeSuccess)
	}
	return err
}

// BreakerState 报告熔断器的状态（closed / open / half-open；没装时为空串）。只给测试与诊断。
func (c *Client) BreakerState() string {
	if c.brk == nil {
		return ""
	}
	return c.brk.state()
}

// CircuitOpen 报告熔断器是不是「打开」（冷却期内，一律不放行）。半开、关闭、没装熔断器都为假。
//
// 只读：不像 allow 那样占用半开时那一个探测名额。给写的调用方在发起一件昂贵的事之前问一句用
// （下单：熔断开着就不提交 SAGA，service/order.go）—— 写本身不经过熔断器（breaker.go 文件头）。
func (c *Client) CircuitOpen() bool {
	return c.brk != nil && c.brk.state() == "open"
}

// PostJSON 把 in 编成 JSON POST 到 path（形如 "/internal/v1/stock/deduct"），
// 2xx 时把响应解到 out（out 为 nil 则丢弃响应体）。
//
// ctx 里有租户就带上 X-Keel-Merchant-ID；没有就不带 —— 那样打到 Routes.Tenant
// 分组会得到 400（ErrBadRequest），这比在客户端这一侧猜一个租户安全。
func (c *Client) PostJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		// 编码失败时请求没有发出去，是确定失败 —— 所以不包成 ErrUnknown。
		return fmt.Errorf("rpc: 编码请求体失败: %w", err)
	}
	err = c.do(ctx, c.timeout, http.MethodPost, path, nil, body, out)
	// 写成功了、或者不知道写没写成：这个请求从此「可能已经留下了东西」（internal/outcome）。
	// 之后再撞上一次数据库超时，兜底就不会对它说「没有生效」。确定失败（4xx、busy）不记。
	if err == nil || IsUnknown(err) {
		reqoutcome.MarkDurable(ctx)
	}
	return err
}

// GetJSON 发 GET，query 可以为 nil。
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, c.timeout, http.MethodGet, path, query, nil, out)
}

func (c *Client) do(ctx context.Context, timeout time.Duration, method, path string, query url.Values, body []byte, out any) error {
	// 超时挂在 ctx 上并覆盖读响应体（下面的 ReadAll 在 cancel 之前）：与原先
	// http.Client.Timeout 的覆盖范围相同。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := *c.base
	u.Path = c.base.Path + path
	if query != nil {
		// Encode 按键排序，签名用的就是这串 —— 服务端拿到的 RawQuery 与它逐字节相同。
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("rpc: 构造请求失败: %w", err)
	}
	merchant := ""
	if id, err := tenant.FromContext(ctx); err == nil {
		merchant = strconv.FormatInt(id, 10)
		req.Header.Set(HeaderMerchantID, merchant)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set(HeaderTimestamp, ts)
	// 签的是 req.URL 上最终的 EscapedPath / RawQuery，而不是入参 path：
	// net/url 可能对路径做转义，服务端看到的是转义后的那串。
	req.Header.Set(HeaderSignature,
		Sign(c.secret, method, req.URL.EscapedPath(), req.URL.RawQuery, merchant, ts, body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if id := traceid.From(ctx); id != "" {
		req.Header.Set(traceid.Header, id)
	}

	fail := func(kind error, status int, cause error) *Error {
		return &Error{Status: status, Method: method, Path: path, kind: kind, err: cause}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// 连不上、超时、ctx 取消：请求可能到了也可能没到。
		return fail(ErrUnknown, 0, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fail(ErrUnknown, resp.StatusCode, err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			// 对面说成功了，我们却读不懂：事情多半做了，只是不知道结果长什么样。
			return fail(ErrUnknown, resp.StatusCode, fmt.Errorf("解析响应失败: %w", err))
		}
		return nil
	}

	var kind error
	switch {
	case resp.StatusCode == http.StatusNotFound:
		kind = ErrNotFound
	case resp.StatusCode == http.StatusConflict:
		kind = ErrConflict
	case resp.StatusCode == http.StatusUnprocessableEntity:
		kind = ErrUnprocessable
	case resp.StatusCode == http.StatusBadRequest:
		kind = ErrBadRequest
	case resp.StatusCode == http.StatusUnauthorized:
		kind = ErrUnauthorized
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		kind = ErrRejected
	case resp.StatusCode == http.StatusServiceUnavailable && problemType(raw) == typeBusy:
		// 对面明确说了「我的事务回滚了」：确定失败（见 ErrBusy）。
		kind = ErrBusy
	default:
		// 5xx 与任何意料之外的状态码（1xx / 3xx）：对面可能已经提交了。
		kind = ErrUnknown
	}
	e := fail(kind, resp.StatusCode, nil)
	var p api.Problem
	if json.Unmarshal(raw, &p) == nil {
		e.Type, e.Title = p.Type, p.Title
	}
	return e
}

// problemType 取响应体里 Problem 的 type；解不出来时为空串。
func problemType(raw []byte) string {
	var p api.Problem
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.Type
}
