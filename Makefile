# Keel 构建入口
#
# 所有构建期工具都经 tools/ 子模块调用，版本钉在 tools/go.mod。
# 不要在这里写 @latest —— CI 与本地装到不同版本是最难查的一类问题。
#
# 为什么工具单独一个模块：sqlc 会把 ClickHouse、Azure SDK 等 270+ 个模块
# 拖进依赖图。放在主模块里，供应链扫描和依赖审计从第一天起就全是噪音。

SHELL := /bin/bash
ROOT     := $(CURDIR)
TOOLS    := $(ROOT)/tools
CONTRACT := $(ROOT)/docs/电商系统-OpenAPI.yaml

# 经 tools 子模块跑工具：版本由 tools/go.mod 锁定。
# 用 `go -C` 而不是 `cd $(TOOLS) && go run` —— 后者会把 shell 也带进 tools/，
# 于是配方里的重定向和相对路径全部相对 tools/ 解析。GO_OUT 给个裸文件名时，
# 产物会静默落在 tools/ 下并照样报 exit 0，是最难查的那种错。
GORUN := go -C $(TOOLS) run

# TS 侧生成器没有 Go 那样的模块锁，只能在这里钉死版本号
OPENAPI_TS := openapi-typescript@7.13.0

# 输出位置由后续任务按实际骨架覆盖，例如：
#   make generate GO_OUT=internal/api/openapi.gen.go TS_OUT=web/src/api/schema.d.ts
GO_OUT     ?= $(ROOT)/build/codegen/openapi.gen.go
GO_PACKAGE ?= api
GO_MODE    ?= types
TS_OUT     ?= $(ROOT)/build/codegen/schema.d.ts

# 迁移目录必须是绝对路径：GORUN 用的 `go -C $(TOOLS)` 让 goose 的工作目录是
# tools/，相对路径会从那里解析。
MIGRATIONS := $(ROOT)/db/migrations

# 连接串的默认值与 internal/db.DSN() 的默认值一一对应。调用方（比如
# internal/db 的迁移测试）应当显式传 GOOSE_DBSTRING，让 Go 侧那份保持唯一权威。
GOOSE_DBSTRING ?= postgres://$(or $(PGUSER),keel):$(or $(PGPASSWORD),keel)@$(or $(PGHOST),127.0.0.1):$(or $(PGPORT),5432)/$(or $(PGDATABASE),keel)?sslmode=disable

# 用 GOOSE_* 环境变量而不是 `goose -dir X postgres DSN up` 那套位置参数：
# goose 的用法是 `goose DRIVER DBSTRING [OPTIONS] COMMAND`，选项必须排在两个
# 位置参数之后，把 -dir 写在前面它会把 DSN 当成命令名，报 "no such command"。
# 环境变量形式没有顺序问题。
GOOSE := GOOSE_DRIVER=postgres GOOSE_DBSTRING="$(GOOSE_DBSTRING)" \
	GOOSE_MIGRATION_DIR=$(MIGRATIONS) \
	$(GORUN) github.com/pressly/goose/v3/cmd/goose

.PHONY: help generate generate-go generate-ts tools-versions contract-check \
	migrate migrate-down migrate-status

help:
	@echo "make generate       生成 Go + TS 两侧契约产物"
	@echo "make generate-go    只生成 Go 侧（GO_OUT / GO_PACKAGE / GO_MODE 可覆盖）"
	@echo "make generate-ts    只生成 TS 侧（TS_OUT 可覆盖）"
	@echo "make contract-check 校验 3.1 可空语义没有被生成器悄悄改掉"
	@echo "make tools-versions 打印钉住的工具版本"
	@echo "make migrate        把 db/migrations 迁到最新（GOOSE_DBSTRING 可覆盖）"
	@echo "make migrate-down   回滚一个版本"
	@echo "make migrate-status 打印各版本的应用状态"

generate: generate-go generate-ts

generate-go:
	@mkdir -p $(dir $(GO_OUT))
	$(GORUN) github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen \
		-package $(GO_PACKAGE) -generate $(GO_MODE) "$(CONTRACT)" > $(GO_OUT)
	@echo "generated $(abspath $(GO_OUT))"

generate-ts:
	@mkdir -p $(dir $(TS_OUT))
	npx --yes $(OPENAPI_TS) "$(CONTRACT)" -o $(TS_OUT)
	@echo "generated $(abspath $(TS_OUT))"

tools-versions:
	@go -C $(TOOLS) list -m -f "{{.Path}} {{.Version}}" \
		github.com/oapi-codegen/oapi-codegen/v2 \
		github.com/pressly/goose/v3 \
		github.com/sqlc-dev/sqlc
	@echo "$(OPENAPI_TS)"

# 契约是 OpenAPI 3.1，用了 3.0 没有的写法。这个检查把 2026-09-25 的选型结论
# 变成一道回归闸：Staff.merchant_id 必须两侧都还是可空的。
# 它会悄悄退化 —— 换生成器版本、或有人把 merchant_id 挪出 required，都会中招。
contract-check: generate
	@grep -q 'MerchantId \*int64  *`json:"merchant_id"`' $(GO_OUT) \
		|| { echo "FAIL: Go 侧 merchant_id 不再是无 omitempty 的 *int64"; exit 1; }
	@grep -q 'merchant_id: number | null;' $(TS_OUT) \
		|| { echo "FAIL: TS 侧 merchant_id 不再是必填的 number | null"; exit 1; }
	@echo "contract-check OK: 3.1 可空语义两侧都在"

# goose 与 sqlc、oapi-codegen 一样钉在 tools/go.mod，不装全局二进制：
# 本地和 CI 装到不同版本的迁移工具，代价是生产库上一次不一致的 schema。
migrate:
	$(GOOSE) up

migrate-down:
	$(GOOSE) down

migrate-status:
	$(GOOSE) status
