package inference_test

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
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
	// 用导入路径而不是 ./...：go list 的相对路径是相对**当前工作目录**解析的，
	// 而 go test 把工作目录设成被测包所在的目录 —— 在 internal/inference 下
	// `./...` 只能列出这个子树，一个 cmd 都看不到。`github.com/keel/keel/...`
	// 是同一个模式的绝对写法，从模块里任何一个目录跑结果都一样。
	modulePattern = "github.com/keel/keel/..."
	embedderPkg   = "github.com/keel/keel/internal/inference"
)

// productionMainPackages 枚举这个模块里**全部** main 包。
//
// 它原先是一个字符串常量（只有 cmd/keel）。M3 新增的 cmd/keel-index 因此
// 不在名单里，而它是直接 import internal/inference 的生产二进制：在
// cmd/keel-index/ 下放一个带 //go:build keel_fake_embedder 的装配文件
// （「引擎连不上就退回替身」这种好心），两条 nofake 测试都不会红 ——
// 然后一条全量索引命令会把伪随机向量灌进 product_text_vectors，
// 检索照常返回结果，只是结果和搜的词无关。
//
// 所以判据改成**枚举**：下一个二进制不用回来改这条测试。这与文件头那条
// 「判据要和构建系统实际的判断是同一个」是同一个理由 —— 一份手写的名单
// 是又一处要靠人记的东西，而漏记不会报错。
func productionMainPackages(t *testing.T, tags string) []string {
	t.Helper()
	args := []string{"-f", `{{if eq .Name "main"}}{{.ImportPath}}{{end}}`}
	if tags != "" {
		args = append([]string{"-tags", tags}, args...)
	}
	var out []string
	for _, line := range strings.Split(goList(t, append(args, modulePattern)...), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// goList 跑一次 go list，失败时把 stderr 一起报出来。
//
// 用真的 go list 而不是读源码找 `//go:build` 那一行：判据要和**构建系统实际的
// 判断**是同一个。grep 一行注释的话，把标签写错（比如 `//go:build keel_fake` 少
// 一截）会让 grep 绿而构建里那个包其实是**默认可见**的——正好反了。
//
// 结果只取 stdout。stderr 另收，只在失败时报：模块缓存是冷的时候（CI 上推理引擎那个
// job 就是），go 会往 stderr 打 `go: downloading ...`，用 CombinedOutput 的话这一行
// 会混进包列表，下一次 go list 把它当成包路径 —— "malformed import path ... invalid
// char ':'"。这条测试就这样在冷缓存上红了，而它守的那件事其实完好。
//
// 会重试几次：CI 的 test-db 把 handler 切十几片同时编，fake-embedder 那组也在同时
// 跑，偶发 `go list … github.com/keel/keel/...` 回一句光秃秃的 `cannot find package`
// （exit 1、没有包路径）。判据本身没坏，是工具在写缓存高峰上绊了一跤——
// 2026-10-08 加 cmd/keel-admin 之后 CI 红过一次，本机单跑复现不出来。
func goList(t *testing.T, args ...string) string {
	t.Helper()
	var out, stderrBytes []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		cmd := exec.Command("go", append([]string{"list"}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err = cmd.Output()
		stderrBytes = stderr.Bytes()
		if err == nil {
			return string(out)
		}
		time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
	}
	t.Fatalf("go list %v 失败: %v\n%s%s", args, err, out, stderrBytes)
	return ""
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
// 绕法是存在的——给某个 cmd 的构建也加上 keel_fake_embedder 标签，
// 那时上一条照样绿。
//
// 查的包是**枚举出来的全部 main 包**加上 internal/inference 本身：
//
//   - 每一个 main 包都是一个生产二进制。cmd/keel 是服务，cmd/keel-index 是
//     全量索引命令（M3 新增，它直接 import internal/inference）。名单写死的
//     时候，后者整个不在视野里 —— 在它下面放一个带标签的装配文件，
//     两条 nofake 测试都不会红，然后一条命令就能把伪随机向量灌满整张
//     product_text_vectors。
//   - internal/inference 是客户端自己。这一半守的是另一个方向：
//     客户端反过来依赖替身（比如「找不到引擎就退回替身」这种好心）。
func TestProductionBinaryDoesNotDependOnFake(t *testing.T) {
	// main 包名单只枚举一次（默认构建）：本仓库没有「只在带标签时才存在的 main」，
	// 带着 keel_fake_embedder 再 list 一遍整个模块，只是多一次写缓存高峰上的 go list，
	// 判据不变、失败面却翻倍。依赖闭包仍然对两种 tags 各查一次——那才是要守的。
	mains := productionMainPackages(t, "")

	// 自证：枚举真的枚举到了东西，而且包含今天已知的那两个二进制。
	//
	// 少了这一段，把 modulePattern 写错（或者 go list 的 -f 模板被改坏）
	// 会让 mains 变成空列表，而**空列表上的循环永远不会失败**——
	// 这条测试于是在看上去最正常的样子下彻底失去牙齿。
	if len(mains) == 0 {
		t.Fatal("一个 main 包都没枚举到 —— go list 的模式或 -f 模板坏了，这条测试正在空跑")
	}
	for _, want := range []string{
		"github.com/keel/keel/cmd/keel",
		"github.com/keel/keel/cmd/keel-index",
	} {
		if !slices.Contains(mains, want) {
			t.Fatalf("枚举出的 main 包 %v 里没有 %s —— "+
				"要么这个二进制被删了（那这里该跟着改），"+
				"要么枚举漏了它（那这条测试正在漏掉一个生产二进制）",
				mains, want)
		}
	}

	for _, tags := range []string{"", "keel_fake_embedder"} {
		for _, pkg := range append(slices.Clone(mains), embedderPkg) {
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
		t.Logf("-tags %q：查过 %d 个 main 包 %v，加 %s", tags, len(mains), mains, embedderPkg)
	}
}
