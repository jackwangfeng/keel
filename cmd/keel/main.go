// Command keel 是单体形态的服务进程。
//
// 这里刻意只有一句调用：启动顺序与依赖装配都在 internal/app，
// 那样「Preflight 在监听之前跑」这条才测得到。写在 main() 里的顺序测不了 ——
// 而它恰恰是最容易在某次重构里被挪走、且挪走之后一切照常的那一行。
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/keel/keel/internal/app"
)

func main() {
	// SIGTERM（docker stop / k8s 滚动更新）与 SIGINT（终端 Ctrl-C）取消 ctx，Run 据此优雅停机：
	// 停止接新请求、等在途请求、停后台任务、关协调器与池（internal/app/lifecycle.go）。
	// 以前用 context.Background()，信号一来进程直接被杀，Run 里那几条收尾的 defer 一条都不执行。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, app.Listen); err != nil {
		log.Fatal(err)
	}
}
