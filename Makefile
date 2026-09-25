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
# 只用来 typecheck 入库的 schema.d.ts，同样钉死版本
TSC := typescript@5.9.2

# 产物落在源码树里，并且**入库**。
#
# 一开始它们默认落在 .codegen/（被 gitignore），理由是「放进主模块会把
# oapi-codegen/runtime 写进 go.mod，于是 go.mod 的内容取决于你跑没跑过
# make generate」。那个顾虑只在产物是临时的时候成立：产物入库之后，go.mod 里
# 那条依赖和产物本身一样是版本库的一部分，谁 clone 下来都一模一样。
#
# 而不入库的代价要大得多：仓库里没有任何契约产物，前后端都没有编译期绑定，
# 「契约是唯一真相源」就只剩一句口号 —— 契约里把 min_price_cents 改个名字，
# 构建照样通过，直到线上客户端解析失败。入库之后 go build 会当场失败。
#
# 想生成到别处（比如临时对比两个生成器的输出）就覆盖这几个变量。
GO_OUT     ?= $(ROOT)/internal/api/openapi.gen.go
GO_PACKAGE ?= api
GO_MODE    ?= types
TS_OUT     ?= $(ROOT)/web/src/api/schema.d.ts

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

.PHONY: help generate generate-go generate-ts generate-sql tools-versions \
	contract-check schema-check migrate migrate-down migrate-status test-db

help:
	@echo "make generate       生成 Go + TS 两侧契约产物"
	@echo "make generate-go    只生成 Go 侧（GO_OUT / GO_PACKAGE / GO_MODE 可覆盖）"
	@echo "make generate-ts    只生成 TS 侧（TS_OUT 可覆盖）"
	@echo "make generate-sql   跑 sqlc，重生成 internal/repository/internal/db"
	@echo "make contract-check 校验 3.1 可空语义没有被生成器悄悄改掉"
	@echo "make schema-check   用 tsc --strict 检查入库的 TS 契约产物编译得过"
	@echo "make tools-versions 打印钉住的工具版本"
	@echo "make migrate        把 db/migrations 迁到最新（GOOSE_DBSTRING 可覆盖）"
	@echo "make migrate-down   回滚一个版本"
	@echo "make migrate-status 打印各版本的应用状态"
	@echo "make test-db      跑需要数据库的测试（强制不吃缓存）"

generate: generate-go generate-ts

# 先写临时文件，成功了才 mv 覆盖目标 —— 不要写成 `oapi-codegen … > $(GO_OUT)`。
#
# shell 的重定向在生成器跑起来**之前**就把目标清成 0 字节了。产物落在被 gitignore
# 的目录里时这无所谓，入库之后就不是了：任何一次失败都当场毁掉版本库里的文件。
# 触发条件不止「没装 Go」—— 契约写崩、生成器 panic、磁盘满、Ctrl-C 都算。
# 而症状极具误导性：别处以 `undefined: api.ProductSummary` 爆炸，不指向真凶。
generate-go:
	@mkdir -p $(dir $(GO_OUT))
	@tmp=$$(mktemp "$(GO_OUT).XXXXXX"); \
	trap 'rm -f "$$tmp"' EXIT INT TERM; \
	$(GORUN) github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen \
		-package $(GO_PACKAGE) -generate $(GO_MODE) "$(CONTRACT)" > "$$tmp" || exit 1; \
	chmod 0644 "$$tmp"; \
	mv "$$tmp" "$(GO_OUT)"
	@echo "generated $(abspath $(GO_OUT))"

# 用生成器自己的 -o，不要改成 `> $(TS_OUT)`：那会把上面 generate-go 里那个
# 「失败即清空入库产物」的坑原样复制到 TS 侧。
generate-ts:
	@mkdir -p $(dir $(TS_OUT))
	npx --yes $(OPENAPI_TS) "$(CONTRACT)" -o $(TS_OUT)
	@echo "generated $(abspath $(TS_OUT))"

# TS 侧产物能不能编译。契约产物入库之后，守着这 176KB 的只有 contract-check 里
# 的一条 grep —— 它只能证明其中一行长什么样。tsc --noEmit 是只读的，且单文件秒级。
#
# 刻意不建 web/package.json：那会凭空引入一棵没人维护的 npm 依赖树。
# 前端脚手架是前端任务的事，这里只要一句「它编译得过」。
schema-check:
	npx --yes -p $(TSC) tsc --noEmit --strict "$(TS_OUT)"
	@echo "schema-check OK: $(abspath $(TS_OUT)) 在 --strict 下编译通过"

# sqlc 的配置路径必须是绝对的，理由同 MIGRATIONS：GORUN 用 `go -C $(TOOLS)`，
# sqlc 的工作目录是 tools/，裸 `sqlc generate` 会在那里找 sqlc.yaml 并报找不到。
# 产物路径（schema / queries / out）由 sqlc 按 sqlc.yaml 所在目录解析，不受此影响。
generate-sql:
	$(GORUN) github.com/sqlc-dev/sqlc/cmd/sqlc -f $(ROOT)/sqlc.yaml generate
	@echo "generated $(ROOT)/internal/repository/internal/db"

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

# 跑碰数据库的测试。-count=1 不是可选项：这些测试真正依赖的输入是数据库状态，
# 而那在 Go 的视野之外。源码和环境变量没变时 `go test` 会直接回放上次的成功结果，
# 于是把 RLS 策略删掉、把谓词改成 USING (true)，测试照样 `ok (cached)`。
# 一个「本地跑两遍就永远绿」的测试，恰恰只在它该报警的时候失灵。
TEST_PKGS ?= ./...

test-db:
	go test -count=1 $(TEST_PKGS)
