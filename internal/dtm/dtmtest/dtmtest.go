// Package dtmtest 是测试里给嵌入式协调器（internal/dtm）用的 sqlite 存储夹具。
//
// 只给 _test.go 用，生产路径不 import 它。
//
// # 它为什么存在：Close 返回之后，dtmrs 还在往目录里写
//
// internal/dtm 的 TestCloseIsIdempotent 偶发失败：
//
//	TempDir RemoveAll cleanup: unlinkat .../001: directory not empty
//
// 真因在 dtmrs（仓库外的 Rust 代码，v0.11.0）与它依赖的 sqlx 0.8.6，
// 逐层查下来是这样的：
//
//  1. dtmrs_close 只做一件事：drop(Box<DtmrsTc>)。DtmrsTc 的字段按声明顺序
//     析构，第一个是 tokio 运行时 rt，Embedded（里面有 sqlx 的连接池）排在后面。
//     运行时先没了，连接池就再也没有机会被 `pool.close().await` —— 它是在
//     没有运行时的情况下被直接 drop 的。
//  2. sqlx-sqlite 给**每条连接**开一个专属的 OS 线程（名字 sqlx-sqlite-worker-N），
//     spawn 之后把 JoinHandle 丢掉了（sqlx-sqlite 0.8.6
//     connection/worker.rs 的 ConnectionWorker::establish）。连接被 drop 时
//     只是关掉给那个线程的命令通道，线程**自己**随后去 sqlite3_close ——
//     没有任何人等它。
//  3. sqlite 在 WAL 模式下（dtmrs 的 after_connect 设了 journal_mode=WAL），
//     最后一条连接关闭时会做一次 checkpoint 并删掉 dtm.db-wal / dtm.db-shm；
//     而关闭那一刻还在建立中的连接会打开库、重新建出 -wal / -shm。
//
// 于是 dtmrs_close 返回时，这个进程里还活着十几个 sqlx-sqlite 线程，正在往
// 临时目录里删文件、建文件；t.TempDir 的 RemoveAll 恰好与它们交错时，
// 列完目录再 rmdir 就会撞上刚建出来的文件。实测：Close 刚返回时 tokio 线程
// 已经是 0 个，sqlx-sqlite 线程还有 16 个，300 毫秒后才全部退出；30 次里
// 8 次能看到 Close 返回之后目录内容还在变。TestCloseIsIdempotent 最容易中，
// 因为它 Start 完立刻 Close，连接池正在建连接。4 个进程各跑 150 次，
// 600 次里失败 6 次。
//
// 所以这不是「Close 没做完」能在 Go 这一侧修掉的事：Go 拿到的只有一个
// 已经返回的 C 函数，没有可以等的句柄。正经的修法在 dtmrs：先 block_on 关掉
// 连接池、再析构运行时（或者把 rt 字段挪到最后）。在那之前，这里在测试侧
// 等「那些线程真的退出了」这件事本身，再让 t.TempDir 删目录。
//
// 刻意不做的：吞掉 RemoveAll 的 ENOTEMPTY、或者删不掉就重试几次。那样掩盖的
// 是同一件事，而且会把「真的有一个协调器没关」也一起吞掉 —— 这里等不到线程
// 退出会报错，报错里说清楚在等什么。
package dtmtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// threadPrefix 是 sqlx-sqlite 给连接线程起的名字 sqlx-sqlite-worker-N 在
// /proc/<pid>/task/<tid>/comm 里的样子（Linux 把线程名截断到 15 个字节）。
//
// 这是对 sqlx 内部命名的依赖。它哪天改了名字，这里会数到 0 个线程、立刻返回，
// 于是退化回原来的偶发失败 —— 不会误报红，也不会卡住。
const threadPrefix = "sqlx-sqlite-wor"

// waitLimit 是等线程退出的上限。实测 300 毫秒内全部退出；等满 10 秒说明
// 不是「还没来得及退」，而是有协调器没关。
const waitLimit = 10 * time.Second

// SQLiteDSN 返回一个 t.TempDir() 里的 sqlite 存储地址（"sqlite:<dir>/dtm.db"），
// 并注册一个清理函数：在 t.TempDir 删目录之前，等本进程里的 sqlx-sqlite 线程
// 回落到调用本函数之前的数量。
//
// 用法：在 Start 之前调用，并且在测试结束前 Close 掉协调器（defer 或显式都行）。
// 清理顺序由 t.Cleanup 的后进先出保证：t.TempDir 的删除在这里注册之前就挂上了，
// 所以一定排在等待之后。
func SQLiteDSN(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	baseline, ok := countStoreThreads()
	t.Cleanup(func() {
		if !ok {
			// 没有 /proc（非 Linux）。数不了线程，就不等 —— 行为与以前一样。
			return
		}
		deadline := time.Now().Add(waitLimit)
		for {
			n, _ := countStoreThreads()
			if n <= baseline {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("dtmtest: 等了 %s，本进程里还有 %d 个 sqlx-sqlite 线程（开始时 %d 个）。"+
					"正常情况下协调器 Close 之后几百毫秒内它们就会退出；等不到说明有协调器"+
					"没有 Close，或者 dtmrs 的关闭行为变了。临时目录 %s 这时删不干净",
					waitLimit, n, baseline, dir)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	return "sqlite:" + filepath.Join(dir, "dtm.db")
}

// countStoreThreads 数本进程里名字以 threadPrefix 开头的线程。
// 第二个返回值为 false 表示这个平台上数不了（没有 /proc/self/task）。
func countStoreThreads() (int, bool) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return 0, false
	}
	n := 0
	for _, task := range tasks {
		comm, err := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "comm"))
		if err != nil {
			continue // 线程恰好在读的时候退出了
		}
		if strings.HasPrefix(string(comm), threadPrefix) {
			n++
		}
	}
	return n, true
}
