package rpc_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/testdb"
)

// 租户透传那条要真的走到 RLS（「handler 看得见租户」不够，要证明 WithTenant
// 在内网请求上照样把别家的行挡在外面），所以本包有自己的库。
// 验签与客户端那几条不碰库，但同一个测试二进制只能有一个 TestMain。

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(testdb.Main(m, testdb.Package{
		Name:  "rpc",
		Setup: setup,
		Teardown: func() {
			if pool != nil {
				pool.Close()
			}
		},
	}))
}

func setup(ctx context.Context) error {
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	seed, err := os.ReadFile(filepath.Join("..", "..", "db", "seed", "dev.sql"))
	if err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, string(seed)); err != nil {
		return err
	}
	pool, err = db.NewPool(ctx)
	return err
}
