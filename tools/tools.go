//go:build tools

// Package tools 把构建期用到的 Go 工具钉进 go.mod，
// 这样 CI 与本地装的是同一个版本，不靠 @latest 碰运气。
package tools

import (
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
	_ "github.com/pressly/goose/v3/cmd/goose"
	_ "github.com/sqlc-dev/sqlc/cmd/sqlc"
)
