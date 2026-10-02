package shopify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/keel/keel/internal/channel"
)

// token 的缓存：按 binding id，连同换它用的凭据指纹（后台换了凭据，旧 token 不再用）。
// 用到有效期的 80% 就换新的，免得一个请求发出去的路上过期。

type tokenEntry struct {
	token       string
	fingerprint string
	renewAt     time.Time
}

type tokenCache struct {
	mu    sync.Mutex // 只护 m 与 locks，不跨网络调用
	m     map[int64]tokenEntry
	locks map[int64]*sync.Mutex // 每个 binding 一把：同一个 binding 同一时刻只换一次，一家店的 token 接口卡住不拖累别家
}

func (c *tokenCache) lockFor(id int64) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = map[int64]*sync.Mutex{}
	}
	l, ok := c.locks[id]
	if !ok {
		l = &sync.Mutex{}
		c.locks[id] = l
	}
	return l
}

func (c *tokenCache) get(id int64) (tokenEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	return e, ok
}

func (c *tokenCache) set(id int64, e *tokenEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e == nil {
		delete(c.m, id)
		return
	}
	c.m[id] = *e
}

func fingerprint(shop string, s secrets) string {
	h := sha256.Sum256([]byte(shop + "\x00" + s.ClientID + "\x00" + s.ClientSecret))
	return hex.EncodeToString(h[:8])
}

// token 取一个能用的 access token。force 为真时丢掉缓存重换（上一次用它被回了 401）。
func (a *Adapter) token(ctx context.Context, bindingID int64, shop string, s secrets, force bool) (string, error) {
	l := a.tokens.lockFor(bindingID)
	l.Lock()
	defer l.Unlock()
	fp := fingerprint(shop, s)
	if e, ok := a.tokens.get(bindingID); ok && !force && e.fingerprint == fp && a.o.Now().Before(e.renewAt) {
		return e.token, nil
	}
	a.tokens.set(bindingID, nil)
	body, _ := json.Marshal(map[string]string{"grant_type": "client_credentials", "client_id": s.ClientID, "client_secret": s.ClientSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.o.BaseURL(shop)+"/admin/oauth/access_token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.o.HTTPClient.Do(req)
	if err != nil {
		return "", &channel.RetryableError{Err: fmt.Errorf("Shopify 换 token 没连上：%w", err)}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", &channel.RetryableError{RateLimited: true, After: retryAfter(resp), Err: fmt.Errorf("Shopify 换 token 被限流")}
	case resp.StatusCode >= 500:
		return "", &channel.RetryableError{Err: fmt.Errorf("Shopify 换 token 回了 HTTP %d", resp.StatusCode)}
	default:
		// 400 invalid_client / 401 / 403：凭据不对或应用被卸载。不拼响应体。
		return "", fmt.Errorf("%w：Shopify 拒绝了 client credentials（HTTP %d）", channel.ErrCredentials, resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		return "", &channel.RetryableError{Err: fmt.Errorf("Shopify 换 token 的响应解不开")}
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600
	}
	life := time.Duration(out.ExpiresIn) * time.Second * 8 / 10
	a.tokens.set(bindingID, &tokenEntry{token: out.AccessToken, fingerprint: fp, renewAt: a.o.Now().Add(life)})
	return out.AccessToken, nil
}
