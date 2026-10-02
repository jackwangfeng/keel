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
	mu sync.Mutex // 换 token 时也攥着：同一时刻只换一次（换 token 很少发生，串行无所谓）
	m  map[int64]tokenEntry
}

func fingerprint(shop string, s secrets) string {
	h := sha256.Sum256([]byte(shop + "\x00" + s.ClientID + "\x00" + s.ClientSecret))
	return hex.EncodeToString(h[:8])
}

// token 取一个能用的 access token。force 为真时丢掉缓存重换（上一次用它被回了 401）。
func (a *Adapter) token(ctx context.Context, bindingID int64, shop string, s secrets, force bool) (string, error) {
	a.tokens.mu.Lock()
	defer a.tokens.mu.Unlock()
	fp := fingerprint(shop, s)
	if e, ok := a.tokens.m[bindingID]; ok && !force && e.fingerprint == fp && a.o.Now().Before(e.renewAt) {
		return e.token, nil
	}
	delete(a.tokens.m, bindingID)
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
	a.tokens.m[bindingID] = tokenEntry{token: out.AccessToken, fingerprint: fp, renewAt: a.o.Now().Add(life)}
	return out.AccessToken, nil
}
