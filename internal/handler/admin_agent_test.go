package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
)

// AI 员工与接入密钥（AI 经营 M9 任务 1，docs/AI经营-M9设计.md §0 验收 1 与 5）。

func agentGet(t *testing.T, host, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = host
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, r)
	return w
}

func createAgent(t *testing.T, sh adminShop, body string) api.AdminAgent {
	t.Helper()
	var a api.AdminAgent
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/agents", body, sh.Token), http.StatusCreated, "建 AI 员工", &a)
	return a
}

func issueAgentKey(t *testing.T, sh adminShop, agentID int64, body string) api.AgentKeyCreated {
	t.Helper()
	var k api.AgentKeyCreated
	decodeInto(t, post(t, sh.Host, fmt.Sprintf("/api/v1/admin/agents/%d/keys", agentID), body, sh.Token),
		http.StatusCreated, "发密钥", &k)
	return k
}

// 主路径：建 AI 员工 → 发密钥（明文只回一次）→ whoami → 吊销即刻 401；停用即刻 403。
func TestAgentKeyLifecycle(t *testing.T) {
	sh := newAdminShop(t)
	a := createAgent(t, sh, `{"name":"AI 店长","role":2}`)
	if a.Role != 2 || a.Status != 1 || a.LiveKeys != 0 {
		t.Fatalf("新建的 AI 员工：%+v", a)
	}
	// 幂等：同一把键重发回放首次那一名，不建第二名。
	key := freshIdemKey()
	var first, again api.AdminAgent
	decodeInto(t, postWithKey(t, sh.Host, "/api/v1/admin/agents", `{"name":"幂等","role":2}`, sh.Token, key),
		http.StatusCreated, "建（首次）", &first)
	w0 := postWithKey(t, sh.Host, "/api/v1/admin/agents", `{"name":"幂等","role":2}`, sh.Token, key)
	decodeInto(t, w0, http.StatusCreated, "建（重放）", &again)
	if again.Id != first.Id || w0.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重放建出了新的 AI 员工：%d vs %d（Replayed=%q）", again.Id, first.Id, w0.Header().Get("Idempotency-Replayed"))
	}
	k := issueAgentKey(t, sh, a.Id, `{"name":"演示站 Claude Code"}`)
	if !strings.HasPrefix(k.Secret, "kagt_") || len(k.Secret) < 40 || !strings.HasPrefix(k.Secret, k.Prefix) {
		t.Fatalf("密钥形状不对：prefix=%q secret 长 %d", k.Prefix, len(k.Secret))
	}
	// 明文不落库：库里只有哈希。
	var n int
	if err := admin(t).QueryRow(context.Background(),
		`SELECT count(*) FROM agent_keys WHERE secret_hash = $1 OR prefix = $1`, k.Secret).Scan(&n); err != nil || n != 0 {
		t.Fatalf("库里能按明文查到密钥（n=%d err=%v）", n, err)
	}
	// 详情里有这把密钥的元数据，没有明文。
	w := getAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/agents/%d", a.Id), sh.Token)
	if strings.Contains(w.Body.String(), k.Secret) {
		t.Fatal("详情里出现了密钥明文")
	}
	var detail api.AdminAgent
	decodeInto(t, w, http.StatusOK, "AI 员工详情", &detail)
	if detail.Keys == nil || len(*detail.Keys) != 1 || detail.LiveKeys != 1 {
		t.Fatalf("详情里的密钥：%+v live=%d", detail.Keys, detail.LiveKeys)
	}

	var me api.AgentWhoAmI
	decodeInto(t, agentGet(t, sh.Host, "/api/v1/agent/whoami", k.Secret), http.StatusOK, "whoami", &me)
	if me.StaffId != a.Id || me.KeyId != k.Id || me.Role != 2 {
		t.Fatalf("whoami：%+v", me)
	}
	// 没带、带错、带了人的会话令牌：一律 401。
	for what, key := range map[string]string{"没带": "", "带错": k.Secret + "x", "人的会话令牌": sh.Token} {
		if w := agentGet(t, sh.Host, "/api/v1/agent/whoami", key); w.Code != http.StatusUnauthorized {
			t.Errorf("%s：whoami 回 %d，期望 401", what, w.Code)
		}
	}

	// 停用：403 account-disabled；恢复后又能用。
	wantStatus(t, patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/agents/%d", a.Id), `{"status":2}`, sh.Token), http.StatusOK, "停用")
	if w := agentGet(t, sh.Host, "/api/v1/agent/whoami", k.Secret); w.Code != http.StatusForbidden {
		t.Errorf("停用后 whoami 回 %d，期望 403", w.Code)
	}
	wantStatus(t, patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/agents/%d", a.Id), `{"status":1}`, sh.Token), http.StatusOK, "恢复")

	// 吊销：即刻 401；再吊销一次 404。
	del := reqAs(t, http.MethodDelete, sh.Host, fmt.Sprintf("/api/v1/admin/agents/%d/keys/%d", a.Id, k.Id), "", sh.Token)
	wantStatus(t, del, http.StatusNoContent, "吊销")
	if w := agentGet(t, sh.Host, "/api/v1/agent/whoami", k.Secret); w.Code != http.StatusUnauthorized {
		t.Errorf("吊销后 whoami 回 %d，期望 401", w.Code)
	}
	del = reqAs(t, http.MethodDelete, sh.Host, fmt.Sprintf("/api/v1/admin/agents/%d/keys/%d", a.Id, k.Id), "", sh.Token)
	wantStatus(t, del, http.StatusNotFound, "重复吊销")
}

// 安全边界：AI 员工不能是管理员；不出现在人的员工列表里、也不能被 /admin/staff 改；
// A 店的密钥打 B 店的域名查不到。
func TestAgentBoundaries(t *testing.T) {
	sh := newAdminShop(t)
	if w := postIdem(t, sh.Host, "/api/v1/admin/agents", `{"name":"想当管理员","role":1}`, sh.Token); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("建管理员角色的 AI 员工回 %d，期望 422", w.Code)
	}
	a := createAgent(t, sh, `{"name":"AI","role":2}`)
	if w := patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/agents/%d", a.Id), `{"role":1}`, sh.Token); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("把 AI 员工改成管理员回 %d，期望 422", w.Code)
	}
	// 库上的 CHECK 是第二道：绕过 service 直接改也不行。
	if _, err := admin(t).Exec(context.Background(), `UPDATE staff SET role = 1 WHERE id = $1`, a.Id); err == nil {
		t.Error("库上没有挡住 AI 员工当管理员（chk_staff_agent_not_admin）")
	}

	var list struct {
		Items []api.Staff `json:"items"`
	}
	decodeInto(t, getAs(t, sh.Host, "/api/v1/admin/staff", sh.Token), http.StatusOK, "人的员工列表", &list)
	for _, s := range list.Items {
		if s.Id == a.Id {
			t.Error("AI 员工出现在 GET /admin/staff 里")
		}
	}
	if w := patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/staff/%d", a.Id), `{"role":1}`, sh.Token); w.Code == http.StatusOK {
		t.Error("/admin/staff 能改 AI 员工")
	}
	if w := post(t, sh.Host, fmt.Sprintf("/api/v1/admin/staff/%d/login-token", a.Id), "", sh.Token); w.Code == http.StatusCreated || w.Code == http.StatusOK {
		t.Error("能给 AI 员工签后台登录令牌")
	}

	k := issueAgentKey(t, sh, a.Id, `{"name":"k","expires_in_days":30}`)
	if k.ExpiresAt == nil {
		t.Error("expires_in_days=30 的密钥没有过期时间")
	}
	other := newAdminShop(t)
	if w := agentGet(t, other.Host, "/api/v1/agent/whoami", k.Secret); w.Code != http.StatusUnauthorized {
		t.Errorf("A 店密钥打 B 店域名回 %d，期望 401", w.Code)
	}
	// B 店管理员看不到、管不了 A 店的 AI 员工。
	if w := getAs(t, other.Host, fmt.Sprintf("/api/v1/admin/agents/%d", a.Id), other.Token); w.Code != http.StatusNotFound {
		t.Errorf("B 店读 A 店 AI 员工回 %d，期望 404", w.Code)
	}
}

// 列表带管辖范围（2026-09-28 修）：之前 GET /admin/agents 对每一名都回空的 region_ids / store_ids，
// 后台「管辖范围」一列对门店管理员身份的 AI 员工永远显示「—」。
func TestAgentListCarriesScopes(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	var out struct {
		Items []api.AdminAgent `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/admin/agents", cs.Token), http.StatusOK, "AI 员工列表", &out)
	for _, it := range out.Items {
		if it.Id == a.Id {
			if len(it.StoreIds) != 1 || it.StoreIds[0] != cs.NorthStore {
				t.Fatalf("列表里的管辖范围应是北京门店，实得 store_ids=%v region_ids=%v", it.StoreIds, it.RegionIds)
			}
			return
		}
	}
	t.Fatal("列表里没有刚建的 AI 员工")
}

// AI 员工密钥的 last_used_at 1 分钟内节流（db/queries/agents.sql 的 TouchAgentKey），
// 与后台会话 last_seen_at 的节流同一个理由（压测报告 docs/性能压测-2026-10.md §6）。
// 这条写在这里之前这个节流已经实现了，这是补上的回归测试——避免将来有人改动
// TouchAgentKey 时把它悄悄退化成每次调用都写（又变回每次工具调用都是一次写事务）。
func TestAgentKeyLastUsedIsThrottled(t *testing.T) {
	sh := newAdminShop(t)
	a := createAgent(t, sh, `{"name":"节流 AI","role":2}`)
	k := issueAgentKey(t, sh, a.Id, `{"name":"节流密钥"}`)

	if n := adminQueryInt64(t, `SELECT count(*) FROM agent_keys
	                             WHERE id = $1 AND last_used_at IS NOT NULL`, k.Id); n != 0 {
		t.Fatal("刚发的密钥 last_used_at 已经不是 NULL 了，这条测试的前提没建起来")
	}

	if w := agentGet(t, sh.Host, "/api/v1/agent/whoami", k.Secret); w.Code != http.StatusOK {
		t.Fatalf("第一次 whoami 失败：%d", w.Code)
	}
	firstSeen := adminQueryText(t, `SELECT last_used_at::text FROM agent_keys WHERE id = $1`, k.Id)
	if firstSeen == "" {
		t.Fatal("调过一次之后 last_used_at 还是 NULL")
	}

	// 1 分钟内连续调用：last_used_at 必须纹丝不动。
	for i := 0; i < 3; i++ {
		if w := agentGet(t, sh.Host, "/api/v1/agent/whoami", k.Secret); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次连续调用失败：%d", i+2, w.Code)
		}
	}
	if got := adminQueryText(t, `SELECT last_used_at::text FROM agent_keys WHERE id = $1`, k.Id); got != firstSeen {
		t.Errorf("1 分钟内的连续调用又写了 last_used_at：%s -> %s", firstSeen, got)
	}

	// 拨到阈值之外（61 秒前），模拟「上一次用是一分钟多以前」。
	adminExec(t, `UPDATE agent_keys SET last_used_at = now() - interval '61 seconds' WHERE id = $1`, k.Id)
	stale := adminQueryText(t, `SELECT last_used_at::text FROM agent_keys WHERE id = $1`, k.Id)

	if w := agentGet(t, sh.Host, "/api/v1/agent/whoami", k.Secret); w.Code != http.StatusOK {
		t.Fatalf("超过阈值之后的调用失败了：%d", w.Code)
	}
	if got := adminQueryText(t, `SELECT last_used_at::text FROM agent_keys WHERE id = $1`, k.Id); got == stale {
		t.Error("超过 1 分钟阈值之后，last_used_at 没有被刷新")
	}
}
