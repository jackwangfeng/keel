// Package buildinfo 回答一个运维问题：**正在跑的这个进程是哪一版**。
//
// 开源仓库公开之后这个问题会变成日常。一份 issue 里写着「搜索返回 500」，
// 第一件要问的事就是版本，而「你 clone 的是哪天的」不是一个人回答得了的问题。
//
// # 为什么是一个包，而不是 cmd/keel 里的两个变量
//
// cmd/keel/main.go 的文件头写着它刻意只有一句调用：启动顺序与依赖装配都在
// internal/app，那样「Preflight 在监听之前跑」这条才测得到。版本号要被
// internal/app 的路由与启动日志用到，放在 main 里就得反向传进去 ——
// 要么给 app.Run 加参数，要么在 main 里写装配，两条都在破坏那句话。
//
// # 为什么不只靠 debug.ReadBuildInfo
//
// Go 1.18 起 `go build` 会把 vcs.revision / vcs.time 自动烧进二进制，看上去
// 不需要 ldflags。但**镜像里拿不到**：docker/Dockerfile 的构建上下文把 .git
// 排除在外（.dockerignore 第 3 行），没有 .git 就没有 VCS 信息，而发布出去的
// 恰恰是镜像。所以真正的来源是 ldflags，ReadBuildInfo 只当本地开发时的兜底 ——
// 那时 .git 在，`go build ./...` 不带任何 ldflags 也能说出自己是哪个 commit。
//
// 两条路的优先级因此是「ldflags 覆盖兜底」，不是反过来。
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// 这三个由 -ldflags -X 注入，见 Makefile 的 LDFLAGS 与 docker/Dockerfile 的
// KEEL_VERSION / KEEL_COMMIT / KEEL_DATE 三个 ARG。
//
// 默认值刻意是 "dev" 而不是空串或者某个具体版本号：一个没被注入的构建
// 应当**说自己是开发构建**，而不是冒充某一版。把默认值写成 "0.1.0" 那种，
// 本地随手 go build 出来的二进制会在 issue 里以正式版本自称。
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info 是对外的形状。字段名与 /version 的 JSON 键一致。
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

var (
	once   sync.Once
	cached Info
)

// Get 返回这个二进制的版本信息。结果只算一次 —— ReadBuildInfo 要遍历模块表，
// 而这个函数会挂在一条 HTTP 路由上。
func Get() Info {
	once.Do(func() {
		cached = Info{
			Version: Version,
			Commit:  Commit,
			Date:    Date,
			Go:      runtime.Version(),
		}
		vcs := readVCS()
		// 只在 ldflags 没给的位置上兜底。反过来（VCS 覆盖 ldflags）会让
		// 打过 tag 的发布构建在本地重编时把版本号丢回 commit sha。
		if cached.Commit == "" {
			cached.Commit = vcs.revision
		}
		if cached.Date == "" {
			cached.Date = vcs.time
		}
	})
	return cached
}

// String 是给启动日志与 --version 用的一行式表示。
func String() string {
	i := Get()
	var b strings.Builder
	b.WriteString(i.Version)
	if i.Commit != "" {
		b.WriteString(" (")
		b.WriteString(shortCommit(i.Commit))
		if i.Date != "" {
			b.WriteString(", ")
			b.WriteString(i.Date)
		}
		b.WriteString(")")
	}
	b.WriteString(" ")
	b.WriteString(i.Go)
	return b.String()
}

// shortCommit 截到 12 位。**只影响显示，不影响 /version 返回的完整值** ——
// 日志里一行要读得完，而机器读的那一处要能直接拿去 `git show`。
func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

type vcsInfo struct {
	revision string
	time     string
}

// readVCS 从 Go 自己烧进来的构建信息里取 commit 与时间。
//
// `vcs.modified` 为 true 时（工作区有未提交改动）给 revision 加一个
// "-dirty" 后缀。这不是装饰：本地改了代码再编出来的二进制，它的 sha 指向的
// 源码和它自己**不是同一份**，而这正是「照着 sha 去看代码、看了半天对不上」
// 那类排查的起点。
func readVCS() vcsInfo {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return vcsInfo{}
	}
	var out vcsInfo
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			out.revision = s.Value
		case "vcs.time":
			out.time = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && out.revision != "" {
		out.revision += "-dirty"
	}
	return out
}
