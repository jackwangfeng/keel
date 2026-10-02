package app

import (
	"testing"

	"github.com/keel/keel/internal/channel"
)

// 第二期起编进来的真实适配器：Shopify。
func TestChannelRegistryHasShopify(t *testing.T) {
	a, ok := channelRegistry().Lookup("shopify")
	if !ok {
		t.Fatal("渠道注册表里没有 shopify")
	}
	c := a.Caps()
	if c.Roles != channel.RoleCatalogSource|channel.RoleOutlet || c.PricePerStore || !c.OutOfOrderInbound {
		t.Fatalf("shopify 的 Caps = %+v", c)
	}
}
