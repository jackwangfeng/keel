package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/keel/keel/internal/app"
)

// KEEL_BACKGROUND 写错拒绝启动（在监听之前），而不是回落成 on 或 off —— 两种回落各有一个
// 安静的坏结局（background.go 的 EnvBackground 注释）。
func TestRunRefusesUnknownBackgroundValue(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvBackground, "of")

	var s spy
	err := app.Run(context.Background(), s.listen)
	if err == nil || !strings.Contains(err.Error(), app.EnvBackground) {
		t.Fatalf("KEEL_BACKGROUND=of 应当拒绝启动并点名这个变量，实得 %v", err)
	}
	if s.called {
		t.Fatal("配置不合法却已经开始监听")
	}
}

// off 时照常起来、照常监听（只是不跑后台任务）。
func TestRunStartsWithBackgroundOff(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvBackground, "off")

	var s spy
	if err := app.Run(context.Background(), s.listen); err != nil {
		t.Fatalf("KEEL_BACKGROUND=off 启动失败：%v", err)
	}
	if !s.called {
		t.Fatal("KEEL_BACKGROUND=off 时没走到监听")
	}
}
