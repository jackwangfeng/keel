package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/channel"
)

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

type gqlCost struct {
	RequestedQueryCost float64 `json:"requestedQueryCost"`
	ThrottleStatus     struct {
		MaximumAvailable   float64 `json:"maximumAvailable"`
		CurrentlyAvailable float64 `json:"currentlyAvailable"`
		RestoreRate        float64 `json:"restoreRate"`
	} `json:"throttleStatus"`
}

type gqlResponse struct {
	Data       json.RawMessage `json:"data"`
	Errors     []gqlError      `json:"errors"`
	Extensions struct {
		Cost *gqlCost `json:"cost"`
	} `json:"extensions"`
}

// gql 发一条带名字的 GraphQL 操作（op 与 query 里的操作名一致），把 data 解进 out。
func (a *Adapter) gql(ctx context.Context, b channel.Binding, op, query string, vars any, out any) error {
	shop, sec, err := parseBinding(b)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars, "operationName": op})
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		tok, err := a.token(ctx, b.ID, shop, sec, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			a.o.BaseURL(shop)+"/admin/api/"+APIVersion+"/graphql.json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Shopify-Access-Token", tok)
		resp, err := a.o.HTTPClient.Do(req)
		if err != nil {
			return &channel.RetryableError{Err: fmt.Errorf("Shopify %s 没连上：%w", op, err)}
		}
		var r gqlResponse
		decErr := json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			continue // token 过期或被撤：重换一次
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
			return fmt.Errorf("%w：Shopify %s 回了 HTTP %d（token 被拒或缺权限）", channel.ErrCredentials, op, resp.StatusCode)
		case resp.StatusCode == http.StatusTooManyRequests:
			return &channel.RetryableError{RateLimited: true, After: retryAfter(resp), Err: fmt.Errorf("Shopify %s 被限流（HTTP 429）", op)}
		case resp.StatusCode >= 500:
			return &channel.RetryableError{Err: fmt.Errorf("Shopify %s 回了 HTTP %d", op, resp.StatusCode)}
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("Shopify %s 回了 HTTP %d", op, resp.StatusCode)
		case decErr != nil:
			return &channel.RetryableError{Err: fmt.Errorf("Shopify %s 的响应解不开：%w", op, decErr)}
		}
		if len(r.Errors) > 0 {
			for _, e := range r.Errors {
				if e.Extensions.Code == "THROTTLED" {
					return &channel.RetryableError{RateLimited: true, After: throttleWait(r.Extensions.Cost),
						Err: fmt.Errorf("Shopify %s 被限流", op)}
				}
			}
			msgs := make([]string, len(r.Errors))
			for i, e := range r.Errors {
				msgs[i] = e.Message
			}
			return fmt.Errorf("Shopify %s 出错：%s", op, truncate(strings.Join(msgs, "；"), 300))
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(r.Data, out); err != nil {
			return fmt.Errorf("Shopify %s 的 data 解不开：%w", op, err)
		}
		return nil
	}
}

// throttleWait 按 throttleStatus 估要等多久：缺的点数 / 每秒回复的点数，下限 1 秒、上限 60 秒；拿不到 cost 时 2 秒。
func throttleWait(c *gqlCost) time.Duration {
	if c == nil || c.ThrottleStatus.RestoreRate <= 0 {
		return 2 * time.Second
	}
	need := c.RequestedQueryCost - c.ThrottleStatus.CurrentlyAvailable
	d := time.Duration(need / c.ThrottleStatus.RestoreRate * float64(time.Second))
	return min(max(d, time.Second), time.Minute)
}

func retryAfter(resp *http.Response) time.Duration {
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if f, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && f > 0 {
		return time.Duration(f * float64(time.Second))
	}
	return 2 * time.Second
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// userError 是 mutation 的 userErrors 一条。
type userError struct {
	Field   []string `json:"field"`
	Message string   `json:"message"`
	Code    string   `json:"code"`
}

// index 取 field 路径里 list 名后面那个下标（["input","quantities","3","changeFromQuantity"] 对 "quantities" 是 3）。
func (u userError) index(list string) (int, bool) {
	for i := 0; i+1 < len(u.Field); i++ {
		if u.Field[i] == list {
			n, err := strconv.Atoi(u.Field[i+1])
			return n, err == nil
		}
	}
	return 0, false
}

var errBatchNotApplied = errors.New("同一批里有别的条目出错，这一条没有生效")
