// Command keel 是单体形态的服务进程。
//
// 这里刻意只有一句调用：启动顺序与依赖装配都在 internal/app，
// 那样「Preflight 在监听之前跑」这条才测得到。写在 main() 里的顺序测不了 ——
// 而它恰恰是最容易在某次重构里被挪走、且挪走之后一切照常的那一行。
package main

import (
	"context"
	"log"

	"github.com/keel/keel/internal/app"
)

func main() {
	if err := app.Run(context.Background(), app.Listen); err != nil {
		log.Fatal(err)
	}
}
