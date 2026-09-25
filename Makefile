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
	contract-check schema-check sdk-smoke migrate migrate-down migrate-status test-db \
	dtmrs-deps build

help:
	@echo "make generate       生成 Go + TS 两侧契约产物"
	@echo "make generate-go    只生成 Go 侧（GO_OUT / GO_PACKAGE / GO_MODE 可覆盖）"
	@echo "make generate-ts    只生成 TS 侧（TS_OUT 可覆盖）"
	@echo "make generate-sql   跑 sqlc，重生成 internal/repository/internal/db（SQLC_CONFIG 可覆盖）"
	@echo "make contract-check 校验 3.1 可空语义没有被生成器悄悄改掉"
	@echo "make schema-check   用 tsc --strict 检查整个 web/src（含契约产物与 SDK）"
	@echo "make sdk-smoke      用 TS SDK 对跑着的服务真打一次 GET /products"
	@echo "make tools-versions 打印钉住的工具版本"
	@echo "make migrate        把 db/migrations 迁到最新（GOOSE_DBSTRING 可覆盖）"
	@echo "make migrate-down   回滚一个版本"
	@echo "make migrate-status 打印各版本的应用状态"
	@echo "make test-db      跑需要数据库的测试（强制不吃缓存）"
	@echo "make dtmrs-deps     取回 dtmrs 并编出 libdtmrs.so（需要 Rust 1.88+）"
	@echo "make build          编译主模块（会先确保 libdtmrs.so 在）"

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

# TS 侧能不能编译。契约产物入库之后，守着这 176KB 的只有 contract-check 里
# 的一条 grep —— 它只能证明其中一行长什么样。tsc --noEmit 是只读的，秒级。
#
# 范围是**整个 web/src**，不是单个 schema.d.ts。只编译产物的话，从产物推导出
# 类型的那个 SDK（web/src/api/client.mts）不在任何闸门的视野里 —— 契约改了、
# 产物跟着变了，而 SDK 的调用点已经对不上了，构建照样绿。
# web/src/api/type-tests.mts 里那批 @ts-expect-error 也只有在这个范围里才会被执行到。
#
# 刻意仍然不建 web/package.json：那会凭空引入一棵没人维护的 npm 依赖树。
# web/tsconfig.json 不是脚手架的开端，它只是这条命令的参数表 —— 全程零 node_modules。
# 范围由 check_ts_scope.py 核对：tsc 对「范围里没有这个文件」是静默的，
# 把 include 写成 `src/**/*.ts` 就能悄悄漏掉所有 .mts 而照样退出 0。
schema-check:
	python3 $(ROOT)/scripts/check_ts_scope.py $(TSC)

# 用 SDK 对**真的跑起来的**服务打一次 GET /products。
#
# 需要栈起着：`docker compose up -d --build`。它与 scripts/smoke.sh 不重复 ——
# 那个用 curl 证明链路通，这个证明 SDK 自己能把契约描述的响应在真实网络上读出来。
#
# 直接 `node xxx.ts`：Node 24 自带类型剥离，不需要 tsx、不需要构建步骤，
# 也就不需要一个 package.json。类型由上面的 schema-check 检查，这里只管跑。
sdk-smoke:
	node $(ROOT)/web/src/api/smoke.mts

# sqlc 的配置路径必须是绝对的，理由同 MIGRATIONS：GORUN 用 `go -C $(TOOLS)`，
# sqlc 的工作目录是 tools/，裸 `sqlc generate` 会在那里找 sqlc.yaml 并报找不到。
# 产物路径（schema / queries / out）由 sqlc 按 sqlc.yaml 所在目录解析，不受此影响。
#
# SQLC_CONFIG 可覆盖，和 GO_OUT / TS_OUT 是同一个用途：让 scripts/check-all.sh
# 能把产物生成到临时目录去比对，而**一个字节都不碰工作区里入库的那一份**。
# 那个脚本会自己拼一份改了 out 路径的临时配置，再从这里传进来。
SQLC_CONFIG ?= $(ROOT)/sqlc.yaml

generate-sql:
	$(GORUN) github.com/sqlc-dev/sqlc/cmd/sqlc -f $(SQLC_CONFIG) generate
	@echo "generated from $(SQLC_CONFIG)"

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

# -p 1：包级串行。
#
# 各个包的 TestMain 都会对**同一个库**跑一遍 goose 迁移。go test 默认按包
# 并行，于是五个包会同时建 goose_db_version、同时跑 00001_init.sql，互相
# 撞成 `relation "goose_db_version" does not exist` / `duplicate key value
# violates unique constraint "pg_type_typname_nsp_index"` / `relation
# "merchants" already exists`。
#
# 这个竞态的方向和一般的抖动相反：**库是暖的就必绿，库是冷的就必红**。
# 迁移过一次之后 goose 看到版本已是最新，什么都不做，窗口根本不存在——
# 所以本地反复跑永远看不到它，而 CI 每一次都是全新空库，每一次都会踩。
# 实测冷库 3/3 全红，加上 -p 1 后 3/3 全绿。
#
# 不用 -p 1 的另一条路是让 TestMain 抢一把咨询锁再迁移，那是把并发正确性
# 做进测试基建；在只有一个共享库的前提下不值当，理由和 -count=1 是同一类：
# 宁可慢一点，也不要一个「只在该报警时失灵」的测试。
test-db: $(DTMRS_LIB)
	go test -count=1 -p 1 $(TEST_PKGS)

# ---------------------------------------------------------------------------
# dtmrs：嵌入式事务协调器的 C ABI 动态库
# ---------------------------------------------------------------------------
#
# M2 起主模块经 cgo 嵌入 dtmrs（internal/dtm），所以 `go build ./...` 需要这个
# .so 与它的头文件。没有它的时候，报错停在链接器的一句
# `cannot find -ldtmrs` 上 —— 那句话不会告诉任何人该跑什么。
#
# 产物不入库（.gitignore 里 /third_party/）：.so 是平台相关的，而头文件必须与
# .so 同版本，分开管理迟早对不上。
#
# 版本钉在 scripts/fetch-dtmrs.sh 里（v0.11.0），examples/dtmrs-embedded 调的
# 也是同一份脚本 —— 两份脚本就是两个版本，而它们错开时的症状是
# 「例子绿、服务红」，报错停在 C ABI 的某个符号上，不指向真因。
DTMRS_DIR := $(ROOT)/third_party/dtmrs
DTMRS_LIB := $(DTMRS_DIR)/lib/libdtmrs.so

dtmrs-deps:
	$(ROOT)/scripts/fetch-dtmrs.sh $(DTMRS_DIR)

# 缺了就自动建一次，而不是报一句链接错。
#
# 代价是第一次 `make test-db` 会多等一分钟左右（本机实测 cargo 缓存是暖的时
# 49 秒）。这个代价是值得的：另一条路是让每个新来的人先撞一次 `cannot find
# -ldtmrs`，再去翻文档找到该跑哪个目标。
$(DTMRS_LIB):
	@echo "==> 没找到 $(DTMRS_LIB)，先建它（需要 Rust 1.88+，约 1 分钟）"
	@$(MAKE) dtmrs-deps

build: $(DTMRS_LIB)
	go build ./...
