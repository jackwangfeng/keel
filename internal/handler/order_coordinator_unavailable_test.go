package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 协调器联系不上时，POST /orders 要回 503 + Retry-After，而不是裸 500。
//
// 2026-10-08 破坏性测试的现场：停掉 dtmrs 之后下单拿到的是
// `{"type":".../problems/internal"}`，grep 过没有 Retry-After。
// 那个 type 的语义是「服务端写错了代码」，而协调器挂掉是一个会自己恢复的
// 依赖故障 —— 同一故障域里库存服务不可用回的是 503/409，两种自愈性一样的
// 依赖给出互斥的语义，客户端没法用一套逻辑处理。
//
// 用注入一个坏协调器来模拟，而不是真停容器：被验的是**错误映射**，
// 真停容器会把网络与容器编排也掺进来，那是另一件事。

// wrapUnavailable 造一个「联系不上协调器」的错误，包法与 Remote.do 网络失败那条路一致。
func wrapUnavailable(what string) error {
	return fmt.Errorf("%w: %s 协调器 httpdial tcp: connection refused",
		dtm.ErrUnavailable, what)
}

// unreachableTC 是一个「联系不上」的协调器：两条方法都以网络层失败告终。
type unreachableTC struct {
	lastGID string
}

func (u *unreachableTC) SubmitSaga(gid, stepsJSON string) error {
	u.lastGID = gid
	// %w 包 dtm.ErrUnavailable —— 与 Remote.do 网络失败那条路的包法一致。
	return wrapUnavailable("提交")
}

func (u *unreachableTC) WaitFinal(gid string, timeoutMS int) (string, error) {
	u.lastGID = gid
	return "", wrapUnavailable("等终态")
}

// swapCoordinator 临时把订单服务的协调器换成 tc，并在测试结束时还原。
func swapCoordinator(t *testing.T, tc service.Coordinator) {
	t.Helper()
	old := testTC
	testOrders.AttachCoordinator(tc)
	t.Cleanup(func() { testOrders.AttachCoordinator(old) })
}

// 协调器不可用 → 503 + Retry-After + coordinator-unavailable。
func TestCoordinatorUnavailableIsRetryable503Not500(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, before := anySKUWithStock(t, "shop-a", 3)

	tc := &unreachableTC{}
	swapCoordinator(t, tc)

	w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 1, ""), tok, "coord-"+uniqueKey())

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("协调器联系不上时回 %d，期望 503。响应体：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Fatalf("503 没有 Retry-After：客户端不知道要退避多久 —— " +
			"这正是 2026-10-08 那次实测的缺陷")
	}
	var p api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("响应体不是 Problem: %v\n%s", err, w.Body.String())
	}
	if p.Type == problem.TypeInternal {
		t.Fatalf("回的是 internal（500 的 type），那是「服务端写错了代码」—— " +
			"协调器挂掉不是写错代码，客户端照它处理就不会重试")
	}
	if p.Type != problem.TypeCoordinatorUnavailable {
		t.Fatalf("problem type 是 %q，期望 %q", p.Type, problem.TypeCoordinatorUnavailable)
	}
	// 这一笔没成立，库存不能动。
	if after := availableOf(t, sku); after != before {
		t.Fatalf("协调器联系不上却扣了库存：水位 %d → %d", before, after)
	}
	t.Logf("协调器不可用 → 503 %s，Retry-After=%s，水位未动 %d",
		p.Type, w.Header().Get("Retry-After"), before)
}

// 协调器不可用时**不能**对外说「这笔一定没下」—— 它可能落库了才断的连接。
//
// 让detail 明确带上「原样重试」，客户端才不会去换一把新键；
// 换键才是重复下单的起点（2026-10-08 的实测后果）。
func TestCoordinatorUnavailableTellsTheClientToRetryTheSameKey(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 3)
	key := "coord-retry-" + uniqueKey()

	swapCoordinator(t, &unreachableTC{})
	w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 1, ""), tok, key)

	var p api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("响应体不是 Problem: %v", err)
	}
	detail := ""
	if p.Detail != nil {
		detail = *p.Detail
	}
	if !strings.Contains(detail, key) && !strings.Contains(detail, "同一个 Idempotency-Key") {
		t.Fatalf("detail 没有告诉客户端带同一把键原样重试：%q —— "+
			"客户端换一把新键就会重复下单", detail)
	}
}

// IsUnavailable 的判据要真能认出网络层错误，且**不**把普通业务错误误判成依赖故障。
//
// 后半句更重要：误判会把「协调器拒绝了」报成 503，让客户端退避重试一件
// 永远不会成功的事。
func TestIsUnavailableRecognisesNetworkFailuresOnly(t *testing.T) {
	if dtm.IsUnavailable(nil) {
		t.Fatal("nil 被判成协调器不可用")
	}
	if dtm.IsUnavailable(wrapUnavailable("x")) {
		t.Log("包装过的网络错误认得出来")
	}
	if dtm.IsUnavailable(errPlain) {
		t.Fatal("普通业务错误被判成协调器不可用 —— 那会把「协调器拒绝了」" +
			"报成 503，客户端退避重试一件永远不会成功的事")
	}
}

var errPlain = errString("协调器拒绝了该请求（HTTP 400）")

type errString string

func (e errString) Error() string { return string(e) }
