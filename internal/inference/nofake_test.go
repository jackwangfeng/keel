package inference_test

import (
	"os/exec"
	"strings"
	"testing"
)

// 这两条守的是同一句话：**替身不许能在生产路径上被默认选中。**
//
// 它为什么值得两条测试：替身出的事不会报错。商品照常索引、向量照常入库、
// 搜索照常返回结果——只是返回的商品和搜的词毫无关系。这正是 M3 计划第二条
// 要消灭的那类东西（「一个哈希凑出来的伪 embedding 能让所有测试变绿，
// 而搜索结果毫无意义」）。
//
// 两条各守一半，缺一不可：
//   - 编译标签还在吗（有人删掉那行 //go:build 时红的是第一条）
//   - 生产二进制的依赖闭包里没有它吗（有人连标签一起加上去 import 时红的是第二条）

const (
	fakePkg = "github.com/keel/keel/internal/inference/fake"
	// 用导入路径而不是 ./cmd/keel：go list 的相对路径是相对**当前工作目录**解析的，
	// 而 go test 把工作目录设成被测包所在的目录。
	mainPkg     = "github.com/keel/keel/cmd/keel"
	embedderPkg = "github.com/keel/keel/internal/inference"
)

// goList 跑一次 go list，失败时把 stderr 一起报出来。
//
// 用真的 go list 而不是读源码找 `//go:build` 那一行：判据要和**构建系统实际的
// 判断**是同一个。grep 一行注释的话，把标签写错（比如 `//go:build keel_fake` 少
// 一截）会让 grep 绿而构建里那个包其实是**默认可见**的——正好反了。
func goList(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list %v 失败: %v\n%s", args, err, out)
	}
	return string(out)
}

// 默认构建里，替身包只有 doc.go（一个包声明），实现文件不在。
func TestFakeEmbedderIsExcludedFromDefaultBuild(t *testing.T) {
	out := strings.TrimSpace(goList(t, "-f", "{{join .GoFiles \" \"}}", fakePkg))
	files := strings.Fields(out)

	for _, f := range files {
		if f == "fake.go" {
			t.Fatalf("默认构建里能看到 %s/fake.go —— 那一行 //go:build keel_fake_embedder "+
				"没了或者写错了。替身一旦能被生产代码 import，"+
				"它出的事不会报错：搜索照常返回结果，只是结果和搜的词无关", fakePkg)
		}
	}
	// 反向自证：带上标签时它必须出现。否则上面那条断言可能是因为包名写错、
	// 文件被删、或者 go list 的输出格式变了而「碰巧绿」。
	withTag := strings.Fields(strings.TrimSpace(
		goList(t, "-tags", "keel_fake_embedder", "-f", "{{join .GoFiles \" \"}}", fakePkg)))
	var found bool
	for _, f := range withTag {
		if f == "fake.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("带 keel_fake_embedder 标签时也看不到 fake.go（默认: %v，带标签: %v）——"+
			"这条测试没有在测它以为在测的东西", files, withTag)
	}
}

// 生产二进制的依赖闭包里没有替身包。
//
// 这一条与上一条不重复：上一条守编译标签还在，这一条守「没人绕过它」。
// 绕法是存在的——给 cmd/keel 的构建也加上 keel_fake_embedder 标签，
// 那时上一条照样绿。
func TestProductionBinaryDoesNotDependOnFake(t *testing.T) {
	// 两个包都查：
	//   - cmd/keel 是生产二进制本身。**今天它还没有 import internal/inference**
	//     （接进索引路径是 M3 任务 3 的事），所以这一半现在是空跑的；
	//     任务 3 一接上去它就自动变成有牙的。这一点写出来，免得有人以为
	//     它今天已经在守着什么。
	//   - internal/inference 是任务 3 会 import 的那个包。这一半**今天就有效**：
	//     客户端反过来依赖替身的话（比如「找不到引擎就退回替身」这种好心），
	//     红的是它。
	for _, pkg := range []string{mainPkg, embedderPkg} {
		for _, tags := range []string{"", "keel_fake_embedder"} {
			args := []string{"-deps", "-f", "{{.ImportPath}}"}
			if tags != "" {
				args = append([]string{"-tags", tags}, args...)
			}
			args = append(args, pkg)
			for _, line := range strings.Split(goList(t, args...), "\n") {
				if strings.TrimSpace(line) == fakePkg {
					t.Fatalf("%s 的依赖闭包里有 %s（-tags %q）。"+
						"生产代码一旦握着替身，选中它只差一个配置项，"+
						"而选错了不会报错：搜索照常返回结果，只是结果和搜的词无关",
						pkg, fakePkg, tags)
				}
			}
		}
	}
}
