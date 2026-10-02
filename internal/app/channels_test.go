package app_test

// 渠道适配层的进程开关（app/channels.go）：关着时一条渠道路由都不注册；拼错的值拒绝启动。

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/app"
)

type routeSpy struct {
	called bool
	routes []string
}

func (s *routeSpy) listen(_ context.Context, _ string, h http.Handler) error {
	s.called = true
	if e, ok := h.(*gin.Engine); ok {
		for _, r := range e.Routes() {
			s.routes = append(s.routes, r.Method+" "+r.Path)
		}
	}
	return nil
}

func (s *routeSpy) channelRoutes() []string {
	var out []string
	for _, r := range s.routes {
		if strings.Contains(r, "/channels") || strings.Contains(r, "channel-") {
			out = append(out, r)
		}
	}
	return out
}

func TestRunWithoutChannelsRegistersNoChannelRoute(t *testing.T) {
	env(t, "", "example.com")
	var s routeSpy
	if err := app.Run(context.Background(), s.listen); err != nil {
		t.Fatal(err)
	}
	if !s.called || len(s.routes) == 0 {
		t.Fatal("没走到监听、或路由表是空的 —— 这条测试没在检查任何东西")
	}
	if got := s.channelRoutes(); len(got) != 0 {
		t.Fatalf("%s 没开却注册了渠道路由：%v", app.EnvChannels, got)
	}
}

func TestRunWithChannelsRegistersChannelRoutes(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvChannels, "on")
	var s routeSpy
	if err := app.Run(context.Background(), s.listen); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(s.channelRoutes(), "\n")
	for _, want := range []string{"POST /api/v1/webhooks/channels/:binding_id", "GET /api/v1/admin/channel-bindings",
		"PUT /api/v1/admin/channel-bindings/:binding_id/secrets"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s=on 却没有 %s；注册了的渠道路由：\n%s", app.EnvChannels, want, got)
		}
	}
}

func TestRunRefusesUnrecognizedChannelsSwitch(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvChannels, "yes please")
	var s routeSpy
	err := app.Run(context.Background(), s.listen)
	if err == nil || !strings.Contains(err.Error(), app.EnvChannels) {
		t.Fatalf("认不出来的开关值：err = %v，期望拒绝启动并点名 %s", err, app.EnvChannels)
	}
	if s.called {
		t.Fatal("拒绝启动却走到了监听")
	}
}
