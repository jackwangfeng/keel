// Package testdb 给每个碰数据库的测试包一个只属于它自己的库。
//
// 只给 _test.go 用。它不在任何生产路径上：cmd/ 与 internal/ 的非测试代码
// 一行都不 import 它（guard_test.go 顺带钉住这一条）。
//
// # 为什么要有它
//
// 以前所有测试包共用 PGDATABASE 指向的那一个库（默认 keel），于是 make test-db
// 只能 `go test -p 1` 包级串行：并行的话五个包同时 DROP SCHEMA、同时跑 goose，
// 互相撞成 `relation "goose_db_version" does not exist` 一类的错。
// 串行的代价是总耗时等于各包之和（main 0a97f45 实测 170 秒起步）。
//
// 现在每个包在自己的 TestMain 里调 Main：建一个 keel_test_<名字> 库，
// 在它上面迁移、跑测试，结束时删掉。包与包之间没有共享的 schema，
// -p 1 就可以去掉了。
//
// 顺带的好处：测试不再 DROP 开发者自己那个 keel 库的 public。
//
// # 三件并行之后才冒出来的事
//
// ① 同名：两个包传了同一个名字，就又回到了共用一个库。Main 对每个名字抢一把
// 会话级咨询锁并持有到进程结束，抢不到就带着一句说明失败 —— 而不是两个包
// 悄悄在一个库里互相 DROP。guard_test.go 另外在源码层面查名字是否重复。
//
// ② 角色是集群级的。00003 会 CREATE ROLE keel_app（已存在则 ALTER ROLE），
// 两个库同时迁移时这一步会撞：冷集群上两边都看见「不存在」、都去 CREATE；
// 热集群上两个 ALTER ROLE 同时改 pg_authid 的同一行。后者实测过：
// 8 个会话并发 ALTER ROLE keel_app × 20 轮，140 次
// `ERROR: tuple concurrently updated`。所以 Migrate 在整个集群范围内串行
// （一把挂在维护库上的咨询锁）。每包只迁一次、每次约 2 秒，串行的代价很小。
//
// ③ 残留：go test -timeout 杀掉进程时 TestMain 的收尾不会执行，库会留下来。
// 下一次 Main 抢到同名锁之后先 DROP 再 CREATE，残留自己就没了；
// 锁保证了被 DROP 的一定不是别人正在用的那个。
//
// # 连接串
//
// Main 在迁移之前 os.Setenv("PGDATABASE", 本包的库)。internal/db 的 DSN() /
// AdminDSN() 本来就读 PGDATABASE，所以本进程里之后的每一条连接 ——
// 池、裸 pgx.Connect、app.Run 里建的池 —— 都自动落到本包的库上，
// 测试代码不用改一个连接串。调用方式也没变：还是 PGHOST / PGPORT 指向集群，
// PGDATABASE（默认 keel）现在只是「维护库」：建库、删库、拿锁都在它上面做，
// 测试数据一行都不往里写。
package testdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

// Package 描述一个测试包要的库。
type Package struct {
	// Name 拼成库名 keel_test_<Name>。惯例是包名。只许小写字母、数字、下划线。
	Name string

	// Template 为 true 时，迁移完之后把这个库存成模板（keel_test_<Name>_tmpl），
	// 之后 Reset 从模板克隆出一个全新的库。只有「每条测试都要一个刚迁完的库」
	// 的包需要它（internal/db）。理由与取舍写在 Reset 上。
	Template bool

	// Setup 在迁移之后、m.Run 之前调用：加载种子、建包级的池与路由。
	// 这时 PGDATABASE 已经指向本包的库。
	Setup func(ctx context.Context) error

	// Teardown 在 m.Run 之后、删库之前调用：关池、关协调器。
	// 不关也删得掉（DROP DATABASE ... WITH (FORCE) 会踢掉残留连接），
	// 但先关干净，删库时被踢掉的就只可能是真正漏关的连接。
	Teardown func()
}

// 咨询锁的键。两参数形式 (int4, int4)：第一个数是 'keel' 的 ASCII，
// 避免和别的东西（dtmrs、应用自己）在同一个集群里用到的键撞上。
//
// 锁都拿在**维护库**上：PostgreSQL 的咨询锁按库区分（锁标签里带库 OID），
// 各包自己的库各不相同，在那上面拿锁互相看不见。
const (
	lockClassMigrate = 1801807212 // 'keel'
	lockClassName    = 1801807213
)

// 迁移锁最多等多久。六个包各迁一次、每次约 2 秒，正常情况下等不到十秒；
// 等满这么久说明有一个迁移卡住了，报错比陪它一起卡着强。
const migrateLockTimeout = 3 * time.Minute

// 一次 make migrate 最多跑多久。理由同上：卡住的迁移要变成一句报错。
const migrateTimeout = 3 * time.Minute

var validName = regexp.MustCompile(`^[a-z0-9_]+$`)

var (
	// maintenanceDSN 是 Main 改 PGDATABASE 之前的 AdminDSN()，即维护库。
	maintenanceDSN string
	current        Package
	dbName         string
)

func templateName() string { return dbName + "_tmpl" }

// Main 是 TestMain 的全部内容：
//
//	func TestMain(m *testing.M) {
//		os.Exit(testdb.Main(m, testdb.Package{Name: "handler", Setup: setup}))
//	}
//
// 返回值交给 os.Exit。准备阶段任何一步失败都打印原因并返回 1。
func Main(m *testing.M, p Package) int {
	code, err := run(m, p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb(%s): %v\n", p.Name, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func run(m *testing.M, p Package) (int, error) {
	if !validName.MatchString(p.Name) {
		return 1, fmt.Errorf("库名片段 %q 不合法：只许小写字母、数字、下划线", p.Name)
	}
	ctx := context.Background()
	current = p
	dbName = "keel_test_" + p.Name
	maintenanceDSN = db.AdminDSN()

	ctl, err := pgx.Connect(ctx, maintenanceDSN)
	if err != nil {
		return 1, fmt.Errorf("连不上维护库（PGHOST/PGPORT/PGDATABASE 指向的那个）: %w", err)
	}
	// ctl 活到进程结束：它持有同名锁。连接一断（包括进程被杀），锁就放了。
	defer ctl.Close(context.Background())

	var got bool
	if err := ctl.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`,
		lockClassName, dbName).Scan(&got); err != nil {
		return 1, fmt.Errorf("抢库名锁失败: %w", err)
	}
	if !got {
		return 1, fmt.Errorf("测试库 %s 正被另一个进程占着（它持有这个库名的咨询锁）。"+
			"要么两个测试包在 testdb.Main 里用了同一个 Name —— 那样它们会在同一个库里"+
			"互相 DROP、互相踩种子，所以这里拒绝继续；要么同一个包正被两个 go test "+
			"同时跑在这个集群上。前者改名字，后者等另一个跑完", dbName)
	}

	if err := recreate(ctx, ctl, dbName, ""); err != nil {
		return 1, err
	}
	if p.Template {
		defer dropQuiet(ctl, templateName())
	}
	defer dropQuiet(ctl, dbName)

	if err := os.Setenv("PGDATABASE", dbName); err != nil {
		return 1, err
	}

	if out, err := Migrate(ctx); err != nil {
		return 1, fmt.Errorf("迁移失败: %w\n%s", err, out)
	}

	if p.Template {
		// 迁移刚跑完，goose 进程已经退出，这个库上没有连接 —— 这正是
		// CREATE DATABASE ... TEMPLATE 的前提（源库上有连接它会拒绝）。
		if err := recreate(ctx, ctl, templateName(), dbName); err != nil {
			return 1, err
		}
	}

	if p.Setup != nil {
		if err := p.Setup(ctx); err != nil {
			return 1, fmt.Errorf("准备测试环境失败: %w", err)
		}
	}

	code := m.Run()

	if p.Teardown != nil {
		p.Teardown()
	}
	return code, nil
}

// recreate 删掉（若在）再建 name。template 非空时从它克隆。
func recreate(ctx context.Context, conn *pgx.Conn, name, template string) error {
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("删除 %s 失败: %w", name, err)
	}
	stmt := `CREATE DATABASE ` + ident
	if template != "" {
		// STRATEGY 用默认的 WAL_LOG：本机实测一个迁完的库（47 MB，大头是 PostGIS）
		// 克隆约 0.3 秒；FILE_COPY 要先做一次 checkpoint，约 0.9 秒。
		stmt += ` TEMPLATE ` + pgx.Identifier{template}.Sanitize()
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("建库 %s 失败: %w", name, err)
	}
	// synchronous_commit = off：提交时不等 WAL 落盘。只挂在测试库上（库级设置，
	// 不碰集群、不碰维护库），也不需要改起库命令。
	//
	// 它只放弃一件事：数据库**进程崩溃**时最后几百毫秒已提交的事务可能丢。
	// 这些库跑完就删，没有任何一条测试断言崩溃持久性；可见性、锁、约束、RLS
	// 的语义与 on 完全相同（它不是 fsync = off，不会把库弄坏）。
	//
	// 换来的：handler 包单独跑 57 秒 → 27 秒（本机实测，同一台机器前后脚）。
	// 这台机器的盘上一次 fsync 常常要几毫秒到几十毫秒，而 handler 的测试是
	// 成百上千个小事务。迁移本身几乎不受影响（实测 on/off 都是 1.8 秒上下，
	// 那里的时间花在服务端 CPU 上）。
	//
	// 克隆出来的库不继承库级设置，所以每次建库（包括从模板克隆）都要设一次。
	if _, err := conn.Exec(ctx, `ALTER DATABASE `+ident+` SET synchronous_commit = off`); err != nil {
		return fmt.Errorf("设置 %s 失败: %w", name, err)
	}
	return nil
}

func dropQuiet(conn *pgx.Conn, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx,
		`DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
		// 不算测试失败：残留的库下一次 Main 会先删掉再建。
		fmt.Fprintf(os.Stderr, "testdb: 收尾时删除 %s 失败（下一次运行会先清掉它）: %v\n", name, err)
	}
}

// Migrate 对本包的库跑一次 `make migrate`，在整个集群范围内与别的包串行。
//
// 走 make 而不是直接 exec bin/goose：调用方式（GOOSE_* 环境变量、迁移目录、
// 二进制在哪、怎么编出来）只在根 Makefile 里写一份。抄一份到这里，
// 以后改调用方式就要改两个地方，而漏改的那一个会以「本地能过 CI 红」的形式暴露。
// 二进制已是最新时，make 那一层的额外开销约 0.15 秒（见 Makefile 的 goose-bin）。
//
// 为什么要串行，见包注释第 ② 条。
func Migrate(ctx context.Context) ([]byte, error) {
	if maintenanceDSN == "" {
		return nil, errors.New("testdb.Migrate 必须在 testdb.Main 之后调用")
	}
	lock, err := pgx.Connect(ctx, maintenanceDSN)
	if err != nil {
		return nil, fmt.Errorf("连维护库拿迁移锁失败: %w", err)
	}
	defer lock.Close(context.Background())

	// lock_timeout 对咨询锁同样生效。设它是为了「不能卡住」：别的进程的迁移
	// 卡死时，这里在有限时间内报错，而不是陪着等到 go test -timeout。
	if _, err := lock.Exec(ctx, fmt.Sprintf(`SET lock_timeout = '%dms'`,
		migrateLockTimeout.Milliseconds())); err != nil {
		return nil, err
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1, 0)`, lockClassMigrate); err != nil {
		return nil, fmt.Errorf("等集群级迁移锁失败（%s 内没等到，另一个包的迁移可能卡住了）: %w",
			migrateLockTimeout, err)
	}
	// 不显式 unlock：lock 连接在 defer 里关掉，会话级咨询锁随之释放。

	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, migrateTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "make", "-s", "-C", root, "migrate",
		"GOOSE_DBSTRING="+db.AdminDSN())
	out, err := cmd.CombinedOutput()
	if cctx.Err() != nil {
		return out, fmt.Errorf("make migrate 超过 %s 没跑完: %w", migrateTimeout, cctx.Err())
	}
	return out, err
}

// Reset 把本包的库换成一份刚迁完的新库：删掉，再从模板克隆。
//
// 只给 Package.Template 为 true 的包用（internal/db）。那个包的测试全是
// 「拿系统目录核对迁移写了什么」，每条都要一个没被别的测试碰过的、
// 刚迁完的库。
//
// # 为什么克隆模板，而不是每条测试从空库重跑一遍迁移
//
// 每条都重迁要一个前提：库必须反映工作区里**当前**的迁移文件 —— 暖库上
// goose 看到版本已是最新就什么都不做，改了一份已应用的迁移会假绿（实测过：
// 删掉 orders.user_id 的复合外键，暖库上 TestForeignKeysAreNotSilentlyMissing
// 照样 ok）。模板满足同一个前提，因为它的来历是固定的：
//
//   - 模板**每次运行都现建**：Main 先建一个空库，对它跑一遍 make migrate
//     （读的就是工作区里此刻的 db/migrations），再把它存成模板；
//     进程结束时删掉。它从不跨运行复用，所以不存在「上一次的迁移」。
//   - CREATE DATABASE ... TEMPLATE 是物理拷贝，克隆出来的库与「空库 + 当前
//     迁移」逐字节相同，不是近似。
//   - 克隆之后调用方还会再跑一次 make migrate（见 internal/db 的 migrate）。
//     正常情况它什么都不做；万一模板落后于文件，它会把缺的迁移补上 ——
//     结果仍然是「当前文件的迁移结果」，不会停在旧状态上。
//
// 换来的是：一次完整迁移本机实测约 1.8 秒（26 条，CPU 花在服务端，
// synchronous_commit 开关实测无差别），克隆约 0.3 秒；internal/db 有二十几次
// 迁移，这一项就是三四十秒。只把 goose 预编成二进制省不掉这部分 ——
// 那只省每次 0.5～1 秒的链接。
func Reset(ctx context.Context) error {
	if !current.Template {
		return fmt.Errorf("testdb.Reset 要求 Package.Template = true（当前包 %q 没开）", current.Name)
	}
	conn, err := pgx.Connect(ctx, maintenanceDSN)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return recreate(ctx, conn, dbName, templateName())
}

// repoRoot 从当前目录（go test 把它设成包目录）往上找本仓库的 go.mod。
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(b), "module github.com/keel/keel\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("从当前目录往上找不到 github.com/keel/keel 的 go.mod")
		}
		dir = parent
	}
}
