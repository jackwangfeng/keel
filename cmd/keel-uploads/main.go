// Command keel-uploads 管上传文件的存储（数据模型 §13）。现在只有一个子命令：把存量从本地磁盘搬到对象存储。
//
//	KEEL_UPLOAD_ROOT=/var/lib/keel-uploads KEEL_S3_ENDPOINT=... KEEL_S3_BUCKET=... KEEL_S3_ACCESS_KEY=... KEEL_S3_SECRET_KEY=... \
//	    go run ./cmd/keel-uploads migrate [-dry-run] [-merchant 3] [-delete-source] [-batch 200]
//
// 推荐的切换顺序（全程不停服，部署指南「上传文件换到对象存储」）：
//
//  1. 应用改成 KEEL_UPLOAD_DRIVER=s3，**同时保留 KEEL_UPLOAD_ROOT**：新图进桶，磁盘上的老图照旧读得到（按行的 driver 路由）。
//  2. 跑 migrate -dry-run 看有多少、源文件全不全；再跑 migrate。可以中断、重跑（搬过的行 driver 已经变了，不会再扫到）。
//  3. 观察几天，确认没问题后 migrate -delete-source 清掉磁盘上的旧文件（已经搬过的行不会再扫到，所以这一步只对
//     还没搬的那些生效——要清已搬的，直接删磁盘目录），再撤掉 KEEL_UPLOAD_ROOT 与卷。
//
// 数据库连接与应用同一套（PG* 环境变量，应用角色，RLS 照常）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "migrate" {
		fmt.Fprintln(os.Stderr, "用法：keel-uploads migrate [-dry-run] [-merchant N] [-delete-source] [-batch N]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "只数、只检查源文件在不在，不写不改")
	merchant := fs.Int64("merchant", 0, "只搬这家店；0 = 全部（含停用的）")
	del := fs.Bool("delete-source", false, "改指之后删掉磁盘上的旧文件")
	batch := fs.Int("batch", 200, "每页几行")
	_ = fs.Parse(os.Args[2:])

	if err := migrate(*dry, *merchant, *del, int32(*batch)); err != nil {
		fmt.Fprintln(os.Stderr, "迁移失败:", err)
		os.Exit(1)
	}
}

func migrate(dry bool, merchant int64, del bool, batch int32) error {
	ctx := context.Background()
	root := strings.TrimSpace(os.Getenv(app.EnvUploadRoot))
	if root == "" {
		return fmt.Errorf("要配 %s（源：本地磁盘 driver 的根目录）", app.EnvUploadRoot)
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return fmt.Errorf("%s=%q 不是一个目录", app.EnvUploadRoot, root)
	}
	s3, err := service.NewS3Store(ctx, app.S3ConfigFromEnv())
	if err != nil {
		return fmt.Errorf("目标对象存储: %w", err)
	}
	pool, err := db.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("建连接池失败: %w", err)
	}
	defer pool.Close()

	m, err := service.NewUploadMigrator(repository.New(pool), service.NewLocalDiskStore(root), s3, nil)
	if err != nil {
		return err
	}
	start := time.Now()
	rep, err := m.Run(ctx, service.UploadMigrateOptions{Merchant: merchant, DryRun: dry, DeleteSource: del, Batch: batch})
	if err != nil {
		return err
	}
	mode := "迁移"
	if dry {
		mode = "试跑（什么都没改）"
	}
	fmt.Printf("%s完成（%s）：扫到 %d，搬完 %d，被别的进程搬走 %d，源文件缺失 %d，字节对不上 %d，其它失败 %d\n",
		mode, time.Since(start).Round(time.Millisecond), rep.Scanned, rep.Moved, rep.Raced, rep.Missing, rep.Mismatch, rep.Failed)
	fmt.Printf("目标：%s\n", s3.Describe())
	if rep.Missing+rep.Mismatch+rep.Failed > 0 {
		return fmt.Errorf("有 %d 行没搬成（见上面的日志）；可以直接重跑，搬过的不会再动", rep.Missing+rep.Mismatch+rep.Failed)
	}
	return nil
}
