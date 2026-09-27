package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/tenant"
)

// DefaultTimeout 是单次内网调用的上限。库存类调用是一两条 SQL，正常在毫秒级；
// 5 秒已经是「对面出事了」。它必须有界：调用方多半在一个公网请求里，
// 无界等待会把公网那一侧的连接一起拖住。
const DefaultTimeout = 5 * time.Second

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
)

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

// Is 让 errors.Is(err, rpc.ErrConflict) 之类成立。
func (e *Error) Is(target error) bool { return target == e.kind }

// Unwrap 暴露传输层原始错误（比如 context.DeadlineExceeded），方便调用方记日志。
func (e *Error) Unwrap() error { return e.err }

// IsUnknown 回答「这次调用的结果是不是未知」。err == nil 时为 false。
func IsUnknown(err error) bool { return errors.Is(err, ErrUnknown) }

// Client 是签名 HTTP 客户端。零值不可用，用 NewClient 建。并发安全。
type Client struct {
	base   *url.URL
	secret string
	http   *http.Client
}

// NewClient 建客户端。baseURL 形如 "http://inventory:8090"（KEEL_INVENTORY_URL）。
// timeout <= 0 时用 DefaultTimeout。
func NewClient(baseURL, secret string, timeout time.Duration) (*Client, error) {
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
	return &Client{base: u, secret: secret, http: &http.Client{Timeout: timeout}}, nil
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
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

// GetJSON 发 GET，query 可以为 nil。
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, out any) error {
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
