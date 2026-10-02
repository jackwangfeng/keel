package shopify_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newStatusServer：换 token 总是成功，GraphQL 总是回 *code（429 带 Retry-After: 3）。
func newStatusServer(t *testing.T, code *int) string {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"shpat_x","expires_in":86399}`))
	})
	mux.HandleFunc("POST /admin/api/2026-10/graphql.json", func(w http.ResponseWriter, r *http.Request) {
		if *code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "3")
		}
		w.WriteHeader(*code)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s.URL
}
