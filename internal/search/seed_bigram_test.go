package search_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/keel/keel/internal/search"
)

// db/seed/single.sql 里那份预先算好的 bigram 串，必须与 Bigram 现在算出来的
// 一模一样。
//
// # 为什么会有「SQL 里的一份切分结果」这种东西
//
// 派生数据入库任务只在配了 KEEL_EMBED_ENDPOINT 时才启动，而 README 承诺的那条
// `docker compose up` 里没有推理引擎。没有这几行，默认那一栈里 search_text
// 永远是 NULL，关键词召回一条也回不了 —— 而 §8 要求「任何一环故障，
// 搜索都必须仍能返回结果」。完整理由写在种子文件那一段注释里。
//
// # 为什么必须有这条测试
//
// internal/search 的包注释说「两侧必须切得一模一样」，而那份 SQL 是**第三份**
// 切分结果，靠人眼维护。它跑偏的症状是「演示库里某个词搜不到」——
// 没有任何构建或测试会因此变红，而 db/seed/single.sql 自己的注释里写着
// 「这份文件没有任何自动化覆盖」。这条测试把那句话改小了一点：
// 至少这一列有覆盖。
//
// 它不连数据库：读文件、重算、比对。
func TestSeedBigramFixtureMatchesTheSplitter(t *testing.T) {
	path := filepath.Join("..", "..", "db", "seed", "single.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	const begin = "-- @keel:bigram-fixture:begin"
	const end = "-- @keel:bigram-fixture:end"
	i := strings.Index(body, begin)
	j := strings.Index(body, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("%s 里找不到 %q / %q 这对标记 —— 标记被改掉或删掉了，"+
			"这条测试因此没有在检查任何东西", path, begin, end)
	}
	block := body[i+len(begin) : j]

	// 每一行形如 ('标题', '副标题', 'bigram 串'),
	// 三列都不含单引号（商品标题里出现单引号时这条正则会漏掉那一行，
	// 而下面的条数对照会当场把它抓出来）。
	row := regexp.MustCompile(`\(\s*'([^']*)'\s*,\s*'([^']*)'\s*,\s*'([^']*)'\s*\)`)
	ms := row.FindAllStringSubmatch(block, -1)
	if len(ms) == 0 {
		t.Fatalf("标记之间一行都没解析出来 —— 这条测试恒绿。块内容：\n%s", block)
	}

	// 条数对照：块里出现了几个 '(' 开头的元组，就该解析出几行。
	// 少了它，一行写坏（比如少一个引号）会被正则安静地跳过。
	if n := strings.Count(block, "('"); n != len(ms) {
		t.Fatalf("块里有 %d 个元组，正则只解析出 %d 行 —— 有一行的写法这条测试跟不过去",
			n, len(ms))
	}

	for _, m := range ms {
		title, subtitle, want := m[1], m[2], m[3]
		got := search.ProductText{Title: title, Subtitle: subtitle}.SearchText()
		if got != want {
			t.Errorf("%q / %q：\n  种子里写的是 %q\n  现在算出来是 %q\n"+
				"—— 切分算法改了而种子没跟上。演示库里这件商品的关键词召回"+
				"会按旧切分工作，而不会有任何东西报错。", title, subtitle, want, got)
		}
	}
	t.Logf("核对了 %d 行 search_text 夹具", len(ms))
}
