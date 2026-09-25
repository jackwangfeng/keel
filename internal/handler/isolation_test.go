package handler_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// handler 与 service 必须无法 import sqlc 产物。
//
// 这条不是风格偏好：sqlc 的 DBTX 接口同时被 *pgxpool.Pool 和 pgx.Tx 满足，
// 所以 db.New(pool) 是能编译过的一句话 —— 而那条路径没有事务，也就没有
// SET LOCAL app.merchant_id。把产物放进 repository/internal 之后，这个错误的
// 写法在编译期就不存在了。下面两个测试守住那个结构。
//
// 为什么探针写在本模块内（而不是计划里写的仓库外临时目录）：
// 在一个自建的临时 module 里 import 本仓库的包，go build 报的是
// "no required module provides package ..." —— 那是**模块解析失败，不是
// internal 违规**。即便 internal 隔离完全没生效，err != nil 也恒成立，
// 测试永远绿。只有把探针放进本模块，go build 才会真正走到 internal 规则。
//
// 探针残留会让 go build ./... 失败，所以清理必须可靠：注册 t.Cleanup 而不是
// defer（同一个测试里两者混用时 defer 先跑），并且断言清理本身成功。

const (
	probeDir = "isolationprobe" // internal/isolationprobe，本包外、repository 外
	probePkg = "github.com/keel/keel/internal/repository/internal/db"
	// tenant 是个普通包，谁都 import 得到。用它做阳性对照。
	publicPkg = "github.com/keel/keel/internal/tenant"
)

// TestProbeHarnessCanBuild 是阳性对照，必须排在隔离断言前面。
//
// 没有它的话，「go build 失败」这个观察一文不值：路径写错、模块解析不了、
// 工具链缺失，任何一个都会让 build 失败，而隔离测试照样绿 —— 这正是计划里
// 那版探针的失效方式。这个测试证明同样的探针机制在 import 普通包时会成功，
// 于是隔离测试里的失败只可能来自 internal 规则。
func TestProbeHarnessCanBuild(t *testing.T) {
	out, err := buildProbe(t, publicPkg)
	if err != nil {
		t.Fatalf("阳性对照失败：探针 import 普通包 %s 也编译不过，"+
			"说明这套探针机制本身有问题，隔离测试的「失败」不能证明任何事。\n%s",
			publicPkg, out)
	}
}

func TestGeneratedDBIsUnreachableFromHandler(t *testing.T) {
	out, err := buildProbe(t, probePkg)
	if err == nil {
		t.Fatalf("internal/isolationprobe 竟然 import 得到 %s —— "+
			"internal 隔离没生效，db.New(pool) 这条绕过 SET LOCAL 的路又回来了", probePkg)
	}
	if !strings.Contains(string(out), "use of internal package") {
		t.Fatalf("构建确实失败了，但不是因为 internal 规则 —— "+
			"这种绿是假的，说明探针没跑到该跑的地方。实际输出：\n%s", out)
	}
	t.Logf("go build 如期拒绝：\n%s", strings.TrimSpace(string(out)))
}

// buildProbe 在模块内写一个只 import pkg 的探针包，对它跑 go build，返回合并输出。
func buildProbe(t *testing.T, pkg string) ([]byte, error) {
	t.Helper()
	return buildProbeSrc(t, "package "+probeDir+"\n\nimport _ \""+pkg+"\"\n")
}

// buildProbeSrc 同上，但探针的源码由调用方给全。
func buildProbeSrc(t *testing.T, src string) ([]byte, error) {
	t.Helper()

	// 测试的工作目录是本包目录，仓库根在上两级（与 internal/db 的迁移测试同惯例）。
	root := filepath.Join("..", "..")
	dir := filepath.Join(root, "internal", probeDir)

	// 上一次崩溃留下的残留会让这次的断言读到旧结果，先清干净。
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("清理旧探针失败: %v", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("建探针目录失败: %v", err)
	}
	// 目录一建出来就登记清理，中间任何一步 t.Fatal 都不会漏掉它。
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("删除探针目录 %s 失败: %v —— 残留会让 go build ./... 一直红", dir, err)
			return
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("探针目录 %s 仍然存在（stat: %v）", dir, err)
		}
	})

	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("写探针文件失败: %v", err)
	}

	cmd := exec.Command("go", "build", "./internal/"+probeDir+"/")
	cmd.Dir = root
	return cmd.CombinedOutput()
}

// WithTenant 交给业务层的 Tx 上不能有 WithTx。
//
// *db.Queries 有一个导出方法 WithTx(pgx.Tx) *Queries。只要业务层握着的是那个
// 具体类型，「自己 Begin 一个事务再 WithTx 过去」就是一句能编译的话 —— 那条路上
// 没有 set_config('app.merchant_id')，查询会撞上 current_merchant() 抛的 42501。
// 比 db.New(pool) 那个缺口窄，但一样是缺口。
//
// 上面两条测试守的是「看不见生成代码这个包」，这一条守的是「看得见的那个类型上
// 没有那个方法」—— 把 repository.Tx 换回 *db.Queries 的类型别名，这条会红，
// 而上面两条不会：别名指向的包名根本不出现在业务代码里，internal 规则不响。
func TestTenantTxDoesNotExposeWithTx(t *testing.T) {
	src := `package ` + probeDir + `

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository"
)

func probe(r *repository.Repo, ctx context.Context, tx pgx.Tx) {
	_ = r.WithTenant(ctx, func(q repository.Tx) error {
		_ = q.WithTx(tx)
		return nil
	})
}
`
	out, err := buildProbeSrc(t, src)
	if err == nil {
		t.Fatal("业务层拿到的 repository.Tx 上竟然有 WithTx —— " +
			"「自己 Begin 一个没设租户的事务」这条路又回来了")
	}
	if !strings.Contains(string(out), "WithTx undefined") {
		t.Fatalf("构建确实失败了，但不是因为 WithTx 不存在 —— "+
			"这种绿是假的。实际输出：\n%s", out)
	}
	t.Logf("go build 如期拒绝：\n%s", strings.TrimSpace(string(out)))
}
