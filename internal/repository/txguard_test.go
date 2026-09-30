package repository_test

import (
	"context"
	"strings"
	"testing"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 事务里又去同一个池上拿连接：测试构建里当场 panic，信息点名方法（txguard.go）。
// 这条是那道防线自己的阳性对照 —— 没有它，「全量测试里没抓到违规」可能只是防线没生效。
func TestSecondPoolConnInsideTxPanics(t *testing.T) {
	r := repository.New(pool(t))
	ctx := tenant.NewContext(context.Background(), 1)

	cases := map[string]func() error{
		// 直接走池的读。
		"ChannelNotifySecret": func() error { _, err := r.ChannelNotifySecret(ctx, "wechat"); return err },
		// 嵌套开第二个事务，同样是第二条连接。
		"withTenantTx": func() error { return r.WithTenant(ctx, func(repository.Tx) error { return nil }) },
	}
	for method, call := range cases {
		t.Run(method, func(t *testing.T) {
			var got any
			_ = r.WithTenant(ctx, func(repository.Tx) error {
				defer func() { got = recover() }()
				return call()
			})
			msg, _ := got.(string)
			if !strings.Contains(msg, "repository."+method) || !strings.Contains(msg, "又从同一个连接池") {
				t.Fatalf("事务里调 %s 应当 panic 并点名方法，实得 %v", method, got)
			}
		})
	}

	// 事务外照常；逃生口 OutsideTx 在事务里放行。
	if _, err := r.ChannelNotifySecret(ctx, "wechat"); err != nil {
		t.Fatalf("事务外读密钥失败：%v", err)
	}
	if err := r.WithTenant(ctx, func(repository.Tx) error {
		_, err := r.ChannelNotifySecret(repository.OutsideTx(ctx), "wechat")
		return err
	}); err != nil {
		t.Fatalf("OutsideTx 没放行：%v", err)
	}
	// 回调结束之后标记撤掉：同一个 goroutine 再用池不该被当成违规。
	if _, err := r.ChannelNotifySecret(ctx, "wechat"); err != nil {
		t.Fatalf("事务结束之后标记没撤：%v", err)
	}
}
