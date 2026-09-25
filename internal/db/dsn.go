// Package db 提供数据库连接相关的公共设施。
package db

import (
	"fmt"
	"os"
)

// DSN 从环境变量拼出连接串。CI 与本地 docker compose 用同一套变量名。
func DSN() string {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		get("PGUSER", "keel"), get("PGPASSWORD", "keel"),
		get("PGHOST", "127.0.0.1"), get("PGPORT", "5432"),
		get("PGDATABASE", "keel"))
}
