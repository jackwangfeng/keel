package shopify

import (
	"os/exec"
	"strings"
	"testing"
)

// 适配器只依赖 internal/channel（spec §6：写一个新渠道只要读那一个包）。
func TestOnlyDependsOnChannelPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list 跑不了：%v", err)
	}
	for _, p := range strings.Fields(string(out)) {
		if strings.HasPrefix(p, "github.com/keel/keel/") && p != "github.com/keel/keel/internal/channel" &&
			p != "github.com/keel/keel/internal/channel/shopify" {
			t.Errorf("shopify 适配器依赖了 %s", p)
		}
	}
}
