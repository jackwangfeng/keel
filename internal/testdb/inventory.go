package testdb

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

// NewInventoryDB 建一个只跑过库存迁移目录（db/migrations-inventory，make migrate-inventory）的空库，
// 返回它的应用角色连接串、管理员连接串与收尾函数（微服务拆分阶段 1b 的两库测试用）。
//
// 库名是本包的库名加后缀（keel_test_<包名>_<suffix>），与本包的库一样由本包独占；
// 迁移与 Migrate 走同一把集群级迁移锁（keel_app 是集群级角色，两个库同时建它会撞）。
// 它**不**跑 core 的迁移：两库测试要证明的正是「库存服务在一个没有 core 任何一张表的库上跑得起来」。
func NewInventoryDB(ctx context.Context, suffix string) (appDSN, adminDSN string, cleanup func(), err error) {
	if maintenanceDSN == "" {
		return "", "", nil, errors.New("testdb.NewInventoryDB 必须在 testdb.Main 之后调用")
	}
	if !validName.MatchString(suffix) {
		return "", "", nil, fmt.Errorf("库名后缀 %q 不合法", suffix)
	}
	name := dbName + "_" + suffix
	ctl, err := pgx.Connect(ctx, maintenanceDSN)
	if err != nil {
		return "", "", nil, err
	}
	defer ctl.Close(context.Background())
	if err := recreate(ctx, ctl, name, ""); err != nil {
		return "", "", nil, err
	}
	cleanup = func() {
		c, err := pgx.Connect(context.Background(), maintenanceDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		dropQuiet(c, name)
	}

	lock, err := pgx.Connect(ctx, maintenanceDSN)
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	defer lock.Close(context.Background())
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1, 0)`, lockClassMigrate); err != nil {
		cleanup()
		return "", "", nil, err
	}
	root, err := repoRoot()
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, migrateTimeout)
	defer cancel()
	adminDSN = withDatabase(db.AdminDSN(), name)
	out, err := exec.CommandContext(cctx, "make", "-s", "-C", root, "migrate-inventory",
		"INVENTORY_GOOSE_DBSTRING="+adminDSN).CombinedOutput()
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("make migrate-inventory 失败: %w\n%s", err, out)
	}
	return withDatabase(db.DSN(), name), adminDSN, cleanup, nil
}

// withDatabase 把一个 postgres:// 连接串的库名换成 name（db.DSN 的形状：.../<库名>?sslmode=disable）。
func withDatabase(dsn, name string) string {
	q := ""
	if i := strings.Index(dsn, "?"); i >= 0 {
		dsn, q = dsn[:i], dsn[i:]
	}
	return dsn[:strings.LastIndex(dsn, "/")+1] + name + q
}
