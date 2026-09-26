package testdb_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 这两条守的是「每个包一个库」在源码层面的两个前提。它们不碰数据库。
//
// 运行期还有一道：testdb.Main 对库名抢咨询锁，同名的两个包同时跑时后到的
// 那个带着说明失败。但运行期那道只在两个包**恰好同时**跑的时候才看得见；
// 名字写重了、或者新包忘了接 testdb，在源码里当场就能查出来。

// touchesDB 认「这个测试文件会连库」的写法：经 internal/db 拿连接串或连接。
var touchesDB = regexp.MustCompile(`\bdb\.(AdminDSN|DSN|NewPool|Connect)\(`)

// mainCall 抓 testdb.Main(m, testdb.Package{Name: "xxx" ...}) 里的名字。
var mainCall = regexp.MustCompile(`testdb\.Main\(\s*\w+\s*,\s*testdb\.Package\{\s*Name:\s*"([^"]+)"`)

type pkgInfo struct {
	touches []string // 连库的测试文件
	names   []string // 这个目录里 testdb.Main 用的名字
}

func scan(t *testing.T) map[string]*pkgInfo {
	t.Helper()
	root := filepath.Join("..", "..")
	pkgs := map[string]*pkgInfo{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			dir, _ := filepath.Rel(root, filepath.Dir(path))
			p := pkgs[dir]
			if p == nil {
				p = &pkgInfo{}
				pkgs[dir] = p
			}
			if touchesDB.Match(b) {
				p.touches = append(p.touches, filepath.Base(path))
			}
			for _, m := range mainCall.FindAllSubmatch(b, -1) {
				p.names = append(p.names, string(m[1]))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return pkgs
}

// 连库的测试包必须经 testdb.Main 拿自己的库。
//
// 漏了的话它会连到 PGDATABASE 指向的维护库（默认 keel，也就是开发者自己那个库），
// 在上面建表、写种子；而 make test-db 已经不再 -p 1，两个漏了的包会同时
// 踩在那一个库上，回到并行化之前那种「冷库必红、暖库必绿」的竞态。
func TestEveryDBTestPackageHasItsOwnDatabase(t *testing.T) {
	pkgs := scan(t)
	var dirs []string
	for dir := range pkgs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	withDB := 0
	for _, dir := range dirs {
		p := pkgs[dir]
		if len(p.touches) == 0 {
			continue
		}
		withDB++
		if len(p.names) == 0 {
			t.Errorf("%s 的测试连数据库（%s），但没有一个 TestMain 调 testdb.Main。"+
				"没有它，这个包会跑在共享的维护库上，与别的包互相踩",
				dir, strings.Join(p.touches, ", "))
		}
	}
	// 阳性对照：一个都没认出来说明正则失效了，上面的循环什么也没查。
	if withDB < 5 {
		t.Fatalf("只认出 %d 个连库的测试包，至少应有 internal/{app,db,handler,repository,service,tenant}"+
			" —— touchesDB 的正则可能已经对不上现在的写法", withDB)
	}
}

// testdb.Main 的名字不许重复：同名就是同一个库。
func TestDatabaseNamesAreUnique(t *testing.T) {
	seen := map[string]string{}
	total := 0
	for dir, p := range scan(t) {
		for _, name := range p.names {
			total++
			if other, ok := seen[name]; ok && other != dir {
				t.Errorf("%s 与 %s 在 testdb.Main 里都用了 Name %q —— 它们会落到同一个库 keel_test_%s，"+
					"并行时互相 DROP、互相踩种子", other, dir, name, name)
			}
			seen[name] = dir
		}
	}
	if total == 0 {
		t.Fatal("一个 testdb.Main 调用都没找到 —— mainCall 的正则可能已经对不上现在的写法")
	}
}
