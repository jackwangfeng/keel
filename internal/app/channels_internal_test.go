package app

import (
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/demotakeout"
)

// 第二期起编进来的真实适配器：Shopify。
func TestChannelRegistryHasShopify(t *testing.T) {
	t.Setenv(EnvChannelDemo, "")
	r, err := channelRegistry()
	if err != nil {
		t.Fatal(err)
	}
	a, ok := r.Lookup("shopify")
	if !ok {
		t.Fatal("渠道注册表里没有 shopify")
	}
	c := a.Caps()
	if c.Roles != channel.RoleCatalogSource|channel.RoleOutlet || c.PricePerStore || !c.OutOfOrderInbound {
		t.Fatalf("shopify 的 Caps = %+v", c)
	}
}

// 演示外卖只在 KEEL_CHANNEL_DEMO=on 时登记（/admin/channel-kinds 列的就是注册表的 Kinds）；拼错拒绝启动。
func TestChannelRegistryDemoTakeoutBehindFlag(t *testing.T) {
	for _, v := range []string{"", "off", "0"} {
		t.Setenv(EnvChannelDemo, v)
		r, err := channelRegistry()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := r.Lookup(demotakeout.Kind); ok {
			t.Fatalf("%s=%q 时注册表里有演示外卖", EnvChannelDemo, v)
		}
		if ks := r.Kinds(); len(ks) != 1 || ks[0] != "shopify" {
			t.Fatalf("%s=%q 时 Kinds = %v", EnvChannelDemo, v, ks)
		}
	}
	t.Setenv(EnvChannelDemo, "on")
	r, err := channelRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if ks := r.Kinds(); len(ks) != 2 || ks[0] != demotakeout.Kind || ks[1] != "shopify" {
		t.Fatalf("开着时 Kinds = %v", ks)
	}
	t.Setenv(EnvChannelDemo, "yes-please")
	if _, err := channelRegistry(); err == nil {
		t.Fatal("开关拼错没有拒绝")
	}
}
