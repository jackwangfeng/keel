// Package dtmtest 是测试里给嵌入式协调器（internal/dtm）用的 sqlite 存储夹具。
//
// 只给 _test.go 用，生产路径不 import 它。
//
// # 它守的是什么：Close 返回时，存储必须已经关干净
//
// dtmrs v0.11.0 的 dtmrs_close 只 drop 句柄：tokio 运行时先析构，连接池后被丢弃、
// 从没 close 过；sqlx-sqlite 每条连接一个 OS 线程，sqlite3_close 在 Close 返回之后
// 才发生。WAL 模式下最后一条连接关闭时会 checkpoint 并删掉 dtm.db-wal /
// dtm.db-shm，于是 Close 返回之后目录还在变，t.TempDir 的 RemoveAll 偶发撞上
// `directory not empty`（TestCloseIsIdempotent，600 次约 6 次）。
//
// 那时 Go 这一侧没有句柄可等，这里只能在测试侧数 sqlx-sqlite 线程、等它们退出。
// 根因报给了 dtmrs（问题单 docs/issues/2026-09-26-dtmrs_close-不等-sqlite-连接关完.md），
// v0.11.1 修掉了两个洞：推进器改成在安全点自行退出而不是被 abort 在建连中途；
// 连接池循环 close 直到连接数归零（sqlx 0.8.6 的 Pool::close 调一次会漏掉
// 正在归还的连接）。keel 侧复核：start 后立刻 close，4 进程 × 60 轮目录变化 0 次。
//
// 所以这里**不再等**，改成断言：清理时目录里只允许剩主库文件。-wal / -shm 还在，
// 说明 Close 返回时连接没关完 —— 要么是 dtmrs 的关闭行为退化了，要么是测试忘了
// Close 协调器。两种都该红，而不是被一次等待或一次重试盖过去。
//
// 看目录而不数线程：同一个进程里并行的其他测试也在开 sqlite，线程数是全进程的，
// 目录是这一条测试自己的。
package dtmtest

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// dbFile 是存储的主库文件名。目录里除了它（或者什么都没有，协调器没启动成功时）
// 之外出现任何东西，都说明 Close 返回时存储没关干净。
const dbFile = "dtm.db"

// SQLiteDSN 返回一个 t.TempDir() 里的 sqlite 存储地址（"sqlite:<dir>/dtm.db"），
// 并注册一个清理函数：在 t.TempDir 删目录之前，断言目录里只剩主库文件。
//
// 用法：在 Start 之前调用，并且在测试结束前 Close 掉协调器（defer 或显式都行）。
// 清理顺序由 t.Cleanup 的后进先出保证：t.TempDir 的删除在这里注册之前就挂上了，
// 所以断言一定排在删目录之前。
func SQLiteDSN(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("dtmtest: 读临时目录 %s 失败：%v", dir, err)
			return
		}
		var extra []string
		for _, e := range entries {
			if e.Name() != dbFile {
				extra = append(extra, e.Name())
			}
		}
		if len(extra) > 0 {
			sort.Strings(extra)
			t.Errorf("dtmtest: 测试结束时 %s 里还有 %v —— 协调器的 sqlite 存储没有关干净。"+
				"要么是这条测试没有 Close 协调器，要么是 dtmrs_close 又回到了「返回时连接"+
				"还没关完」的行为（v0.11.0 的问题，v0.11.1 修掉了，见本包文档）", dir, extra)
		}
	})
	return "sqlite:" + filepath.Join(dir, dbFile)
}
