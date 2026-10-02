package demotakeout

import (
	"os/exec"
	"strings"
	"testing"
)

// 演示适配器只依赖标准库与 internal/channel（plan Global Constraints）。
func TestOnlyDependsOnStdlibAndChannel(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list 跑不了：%v", err)
	}
	for _, p := range strings.Fields(string(out)) {
		switch p {
		case "github.com/keel/keel/internal/channel", "github.com/keel/keel/internal/channel/demotakeout":
			continue
		}
		// 标准库的包路径第一段不带点（vendor/golang.org/x/… 是标准库自带的）。
		if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
			t.Errorf("演示外卖适配器依赖了 %s", p)
		}
	}
}
