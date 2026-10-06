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
#
# goose 先编成二进制（$(GOOSE_BIN)，见 goose-bin 目标）再调，不走 $(GORUN)。
# `go run` 每次都要重新链接一遍 goose（它把 mysql、sqlite、clickhouse 等驱动
# 全链进来，二进制 60 MB），本机实测每次多花 0.5～1 秒，机器忙的时候更多；
# internal/db 的测试每条都要迁一次，这笔钱乘上测试条数就是几十秒。
# 版本仍然只由 tools/go.mod 决定：goose-bin 每次都执行 `go build -o`，
# 由 go 自己的构建缓存判断要不要重链 —— 不靠 make 的时间戳，
# 所以 tools/go.mod 改了版本之后不会有一个旧二进制被悄悄沿用。
GOOSE_BIN := $(ROOT)/bin/goose
GOOSE := GOOSE_DRIVER=postgres GOOSE_DBSTRING="$(GOOSE_DBSTRING)" \
	GOOSE_MIGRATION_DIR=$(MIGRATIONS) \
	$(GOOSE_BIN)

# 库存服务自己的迁移目录（微服务拆分阶段 1b，docs/电商系统-微服务拆分方案.md「数据库与迁移」）。
#
# 版本表是 goose_db_version_inventory，不是 goose 默认的 goose_db_version：两个目录可以指向
# 同一个库（单体，库存表早由 core 迁移建好，这边是空操作）也可以指向两个库（拆分），
# 共用一张版本表的话，core 的 00075 与库存的 00001 会被当成同一条时间线上的两个版本。
#
# 连接串默认与 GOOSE_DBSTRING 相同（单体）；拆分部署指向库存库：
#   make migrate-inventory INVENTORY_GOOSE_DBSTRING=postgres://keel:...@inventory-db:5432/keel_inventory?sslmode=disable
# 单体库上要跑它，必须排在 make migrate 之后（00001 里的 IF NOT EXISTS 靠 core 先建好那几张表）。
MIGRATIONS_INVENTORY := $(ROOT)/db/migrations-inventory
INVENTORY_GOOSE_DBSTRING ?= $(GOOSE_DBSTRING)
GOOSE_INVENTORY := GOOSE_DRIVER=postgres GOOSE_DBSTRING="$(INVENTORY_GOOSE_DBSTRING)" \
	GOOSE_MIGRATION_DIR=$(MIGRATIONS_INVENTORY) \
	$(GOOSE_BIN) -table goose_db_version_inventory

.PHONY: help generate generate-go generate-ts generate-sql tools-versions version search-metrics \
	contract-check schema-check admin-install admin-type-check admin-test admin-build flutter-get flutter-generate flutter-analyze flutter-test flutter-e2e-web flutter-build flutter-build-mp flutter-ios-install flutter-android-install \
	sdk-smoke goose-bin migrate migrate-down migrate-status migrate-inventory migrate-inventory-status test-db \
	test-engine category-eval dtmrs-deps build \
	init doctor prod-up prod-down prod-logs prod-config release-up release-config

# ── 生产部署 ─────────────────────────────────────────────────────────
# 演示栈是裸 `docker compose up`（不带 -f），生产栈是下面这组 target。
# 两者项目名与数据卷都分开，同一台机器上可以并存 —— 演示栈的 COMPOSE_PROJECT_NAME
# 是 keeldemo，这里固定 -p keel。
#
# 为什么要 doctor 挡在 prod-up 前面：compose 的报错指向性很差（连不上 / 401 / 全站 404），
# 而上面那几个失败原因对应的症状几乎一模一样。让人先看到「哪一项没配」再动手。

PROD_ENV        := --env-file $(ROOT)/.env
PROD_COMPOSE    := docker compose -p keel $(PROD_ENV) -f $(ROOT)/compose.yaml -f $(ROOT)/compose.prod.yaml
# 走预构建镜像的部署用这个，额外需要 .env 里的 KEEL_IMAGE_TAG。
RELEASE_COMPOSE := docker compose -p keel $(PROD_ENV) -f $(ROOT)/compose.yaml -f $(ROOT)/compose.prod.yaml -f $(ROOT)/compose.release.yaml

init:
	@bash $(ROOT)/scripts/init-env.sh

doctor:
	@bash $(ROOT)/scripts/doctor.sh

prod-up:
	@$(MAKE) --no-print-directory doctor
	$(PROD_COMPOSE) up -d
	@echo "起了。首个平台管理员的 bootstrap token 在 app 日志里：make prod-logs"

prod-down:
	$(PROD_COMPOSE) down

prod-logs:
	$(PROD_COMPOSE) logs -f app

prod-config:
	$(PROD_COMPOSE) config

# 走预构建镜像。快一个数量级：本地构建要拉 module 编 Rust，几分钟；这个是拉四个镜像，
# 实测 4.9 秒起完整套。代价是代码冻结在那个 tag 上。
release-up:
	@$(MAKE) --no-print-directory doctor
	$(RELEASE_COMPOSE) up -d
	@echo "起了。bootstrap token 在 app 日志里：make prod-logs"

release-config:
	$(RELEASE_COMPOSE) config

help:
	@echo "make init           生成 .env（不覆盖已有的），三个密钥项自动填随机值"
	@echo "make doctor         上线前自查：docker、.env 必填项、compose 解析、端口占用、存量数据坑"
	@echo "make prod-up        按生产配置起栈（先自动跑 doctor）"
	@echo "make prod-down      停掉生产栈（数据卷保留）"
	@echo "make prod-logs      跟 app 日志，首个管理员的 bootstrap token 在里面"
	@echo "make prod-config    打印生产栈合成后的完整 compose 配置"
	@echo "make release-up     同 prod-up，但用 .env 里 KEEL_IMAGE_TAG 指定的预构建镜像（快一个数量级）"
	@echo "make release-config  打印镜像版合成后的配置"
	@echo "make generate       生成 Go + TS 两侧契约产物"
	@echo "make generate-go    只生成 Go 侧（GO_OUT / GO_PACKAGE / GO_MODE 可覆盖）"
	@echo "make generate-ts    只生成 TS 侧（TS_OUT 可覆盖）"
	@echo "make generate-sql   跑 sqlc，重生成 internal/repository/internal/db（SQLC_CONFIG 可覆盖）"
	@echo "make contract-check 校验 3.1 可空语义没有被生成器悄悄改掉"
	@echo "make schema-check   用 tsc --strict 检查整个 web/src（含契约产物与 SDK）"
	@echo "make admin-install  装商家后台（web/admin）的依赖（npm ci，版本由 lock 锁定）"
	@echo "make admin-type-check 用 vue-tsc --strict 检查 web/admin/src 下全部 .ts 与 .vue"
	@echo "make admin-test     跑商家后台的单元测试（围栏几何与坐标系换算、券金额换算、订单与售后的按钮与请求体、铃铛的跳转、运费模板校验、经营概览的环比与图表几何、营销活动的表单换算、AI 员工提案的种类翻译与自动执行策略上限换算）"
	@echo "make admin-build    构建商家后台静态产物（compose 起栈时会自己构建，日常不用跑）"
	@echo "make sdk-smoke      用 TS SDK 对跑着的服务真打一次 GET /products"
	@echo "make tools-versions 打印钉住的工具版本"
	@echo "make goose-bin      把钉在 tools/go.mod 的 goose 编到 bin/goose（migrate 会先调它）"
	@echo "make migrate        把 db/migrations 迁到最新（GOOSE_DBSTRING 可覆盖）"
	@echo "make migrate-down   回滚一个版本"
	@echo "make migrate-status 打印各版本的应用状态"
	@echo "make migrate-inventory 把 db/migrations-inventory 迁到最新（INVENTORY_GOOSE_DBSTRING 可覆盖，默认同 GOOSE_DBSTRING）"
	@echo "make migrate-inventory-status 打印库存库各版本的应用状态"
	@echo "make search-metrics 按店铺 × 策略统计搜索效果（PERIOD 默认 7 days，要管理员连接）"
	@echo "make test-db      跑需要数据库的测试（强制不吃缓存，含替身那一组）"
	@echo "make test-engine    对真的跑着的 infero 跑三条判据（要 KEEL_EMBED_ENDPOINT + 数据库；GPU 版，CPU 版目前太慢）"
	@echo "make category-eval  类目推荐的离线评测（Top-1 / Top-3 与阈值表，要跑着的 infero + KEEL_EMBED_ENDPOINT）"
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

# ---------------------------------------------------------------------------
# 商家后台（web/admin，Vue 3 + Vite + Element Plus）
# ---------------------------------------------------------------------------
#
# **为什么它不在 web/src 下、不进 schema-check 的范围**，完整论证写在
# scripts/check_admin_types.py 的文件头与 web/admin/tsconfig.json 的注释里。
# 一句话：web/src 那道闸门的价值是「零 node_modules」，而 `tsc` 读不了 `.vue`；
# 把 Vue 应用塞进去会让几十个文件静默掉出所有闸门，而闸门照样报绿。
# **web/src 的覆盖范围一个字节没动** —— 这里是新增一道，不是把旧的那道改松。
#
# 版本钉死在 web/admin/package.json，且用 `npm ci` 按入库的 package-lock.json
# 安装，理由与本文件头那条 @latest 禁令一样：CI 与本地装到不同版本是最难查
# 的一类问题。后台的 typescript 也钉在 $(TSC) 的同一个版本上（5.9.2），
# 免得两道 TS 闸门用两个编译器。
admin-install:
	cd $(ROOT)/web/admin && npm ci

# web/admin/src 下全部 .ts 与 .vue 在 --strict 下能不能编译，
# 以及编译范围有没有真的盖住它们（vue-tsc 对「范围里没有这个文件」同样静默）。
# 还会断言类型真的来自入库的契约产物 web/src/api/schema.d.ts，而不是一份副本。
#
# 要先 make admin-install。没装依赖时它**失败**而不是跳过 ——
# 跳过会让「后台的类型检查跑过了」这句话在没装依赖的机器上是假的。
admin-type-check:
	python3 $(ROOT)/scripts/check_admin_types.py

# 后台的单元测试：电子围栏的几何（坐标序、闭合、GCJ-02 / BD-09 → WGS-84），
# 以及券管理里「元 ↔ 分」「折 ↔ 千分比」的换算（src/api/money.ts：只做字符串解析，
# 不做一次浮点乘法 —— Number("0.29") * 100 是 28.999999999999996），
# 还有订单与售后页的界面规则（src/api/orderRules.ts：哪个状态亮哪个按钮、
# 审核请求体里运费带不带、日期范围怎么变成半开区间）。
# 以及批量导入页的规则（src/api/importRules.ts：只预选服务端判定可信的类目、
# 确认时把推荐的类目显式带回——服务端确认那一步不调引擎、不自动采用推荐）。
#
# 守的是「偏了不会报错」那一类错：经纬度写反、坐标系没换，服务端都会收下一个
# **合法**的多边形，只是位置偏了几百米，买家被判进错的门店。
#
# `node --test` 直接跑 .ts（Node 24 的类型剥离），不引入测试框架：被测文件
# （src/api/geo.ts）刻意只有 import type，没有运行时依赖，所以这一步**不需要**
# node_modules。别往 geo.ts 里加运行时 import，否则这里会以
# ERR_MODULE_NOT_FOUND 失败。
admin-test:
	cd $(ROOT)/web/admin && node --test src/api/geo.test.ts src/api/money.test.ts src/api/orderRules.test.ts src/api/notifications.test.ts src/api/importRules.test.ts src/api/freightRules.test.ts src/api/reports.test.ts src/api/promotionRules.test.ts src/api/shopSettings.test.ts src/api/localDeliveryRules.test.ts src/api/agentProposalRules.test.ts src/api/agentPolicyRules.test.ts src/api/paymentReturnRules.test.ts src/api/mapTiles.test.ts src/api/channelRules.test.ts

# 后台布局检查：每个页面（含标签页、「新建」弹窗）按手机 390px 与电脑 1440px 各打开一次，查横向撑破、控件出屏、
# 点击目标过小、按钮文字截断、控制台报错，并截图。只读。报告在 tmp/responsive/report.md。
# ADMIN_URL 默认演示站（免登录）；本地预览见 scripts/admin-responsive-check.cjs 文件头的 LOCAL_DIST。
ADMIN_URL ?= https://eshop.zzss.fun/admin
admin-responsive-check:
	node $(ROOT)/scripts/admin-responsive-check.cjs $(ADMIN_URL) $(ROOT)/tmp/responsive

# 构建静态产物到 web/admin/dist。日常不用跑：compose 起栈时在
# docker/Dockerfile.admin 的 node 阶段里构建，产物交给 nginx。
admin-build:
	cd $(ROOT)/web/admin && npm run build

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

# 打印钉住的版本。
#
# 它原先只打构建期工具，因为主模块的运行时依赖是 0 —— 那时「工具版本」
# 就是这个仓库全部的第三方版本。M2 Task 1.5 之后不是了：口令哈希需要
# golang.org/x/crypto（argon2id），那是主模块第一个**运行时**依赖。
#
# 所以这里跟着长一段。两段的版本纪律其实不同，写清楚免得有人以为是同一回事：
#
#   · 构建期工具钉在 tools/go.mod，理由是 Makefile 顶部那段（不许 @latest）；
#   · 运行时依赖钉在主模块的 go.mod + **go.sum**，后者是密码学校验 ——
#     版本对不上不是「装到了别的版本」，是 go build 直接拒绝。
#
# 顺带一句 x/crypto 的账：它**不是**一个新拖进来的模块。gin、validator、
# oapi-codegen/runtime、quic-go 早就各自依赖它，它一直在 go.mod 的 indirect 段里。
# 这次只是从 indirect 升成 direct —— 依赖图的模块数一个没多。
tools-versions:
	@echo "== 构建期工具（tools/go.mod） =="
	@go -C $(TOOLS) list -m -f "{{.Path}} {{.Version}}" \
		github.com/oapi-codegen/oapi-codegen/v2 \
		github.com/pressly/goose/v3 \
		github.com/sqlc-dev/sqlc
	@echo "$(OPENAPI_TS)"
	@echo "== 运行时依赖（go.mod 的 direct，由 go.sum 做完整性校验） =="
	@go list -m -f "{{if not .Indirect}}{{.Path}} {{.Version}}{{end}}" all | grep -v '^$$' | tail -n +2

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
#
# goose-bin 把它编到仓库里的 bin/（已被 .gitignore 忽略），不是 PATH 上。
# 输出路径必须是绝对的，理由同 MIGRATIONS：`go -C $(TOOLS)` 下相对路径
# 会从 tools/ 解析。二进制已是最新时这一步约 0.15 秒（go 比对 build ID，不重链）。
#
# 测试（internal/testdb）调的仍是 `make migrate`，不直接 exec bin/goose：
# 调用方式（环境变量、迁移目录、二进制在哪）只在这个文件里写一份。
goose-bin:
	@mkdir -p $(dir $(GOOSE_BIN))
	@go -C $(TOOLS) build -o $(GOOSE_BIN) github.com/pressly/goose/v3/cmd/goose

migrate: goose-bin
	$(GOOSE) up

migrate-down: goose-bin
	$(GOOSE) down

migrate-status: goose-bin
	$(GOOSE) status

# 库存库只往前迁：00001 的 Down 是空操作（单体库里那些表归 core 迁移所有），所以不提供 migrate-inventory-down。
migrate-inventory: goose-bin
	$(GOOSE_INVENTORY) up

migrate-inventory-status: goose-bin
	$(GOOSE_INVENTORY) status

# 搜索效果的离线统计（语义检索层 §9.2）：无结果率、CTR@10、搜索→加购率、
# 搜索→下单转化率、首次点击名次倒数，按店铺 × 策略 × 实际跑过的阶段分组。
# 口径与用法写在 scripts/search_metrics.sql 的文件头。
#
# 连接走 libpq 的环境变量，**要管理员角色**：search_logs 有 RLS，keel_app 在一条
# 没有租户上下文的连接上一行都读不到（它会安静地打印一张空表，而不是报错）。
# 演示栈里可以：docker compose exec -T postgres psql -U keel -d keel \
#   -v period='7 days' -f - < scripts/search_metrics.sql
PERIOD ?= 7 days
search-metrics:
	psql -X -v ON_ERROR_STOP=1 -v period='$(PERIOD)' -f $(ROOT)/scripts/search_metrics.sql

# 跑碰数据库的测试。-count=1 不是可选项：这些测试真正依赖的输入是数据库状态，
# 而那在 Go 的视野之外。源码和环境变量没变时 `go test` 会直接回放上次的成功结果，
# 于是把 RLS 策略删掉、把谓词改成 USING (true)，测试照样 `ok (cached)`。
# 一个「本地跑两遍就永远绿」的测试，恰恰只在它该报警的时候失灵。
TEST_PKGS ?= ./...

# 包级并行（go test 的默认 -p，即 CPU 数）。
#
# 以前这里是 -p 1：各个包的 TestMain 都对**同一个库**跑 goose 迁移，并行的话
# 五个包会同时建 goose_db_version、同时跑 00001_init.sql，互相撞成
# `relation "goose_db_version" does not exist` / `duplicate key value violates
# unique constraint "pg_type_typname_nsp_index"`。那个竞态的方向和一般抖动相反：
# **库是暖的就必绿，库是冷的就必红**，所以本地反复跑永远看不到它，而 CI 每一次
# 都是全新空库，每一次都会踩（实测冷库 3/3 全红，加上 -p 1 后 3/3 全绿）。
#
# 现在每个碰库的包在自己的 TestMain 里经 internal/testdb 建一个只属于它的库
# （keel_test_<包名>），在上面迁移、跑测试、结束时删掉 —— 包与包之间没有共享的
# schema，那个竞态的前提没了。剩下两处跨库共享的东西都在 testdb 里处理了：
# keel_app 是集群级角色，两个库同时迁移会在 00003 上撞（实测
# `tuple concurrently updated`），所以迁移那一步在集群范围内串行；
# 两个包写了同一个库名时，后到的那个带着说明失败，而不是悄悄共用。
#
# 连接方式没变：PGHOST / PGPORT 指向集群，PGDATABASE（默认 keel）现在只是
# 建库删库用的维护库，测试数据不再写进去。
#
# test-db 先编好 goose（goose-bin）再起 go test：各包的 TestMain 都会经
# make migrate 用到它，先编好就不会有几个包同时去链接同一个输出文件。
#
# -timeout：go test 默认 10 分钟，而且是**每个包**各自计时。并行化之后最慢的包
# 本机实测在 30 秒以内（见提交说明），负载重的时候观察到过三倍的抖动；
# 给 5 分钟，真卡住时 5 分钟内会带着各 goroutine 的栈失败，而不是陪着等满
# CI job 的 timeout-minutes、最后只留下一句「超时」。
TEST_TIMEOUT ?= 5m

test-db: $(DTMRS_LIB) $(DTMRS_BIN) goose-bin
	go test -count=1 -timeout=$(TEST_TIMEOUT) $(TEST_PKGS)
	@echo "==> 替身那一组（-tags keel_fake_embedder）"
	go test -count=1 -timeout=$(TEST_TIMEOUT) -tags keel_fake_embedder ./internal/inference/...

# 类目推荐的离线评测（商品批量导入的预检用它推荐类目）。
#
# 与 test-engine 同一个理由不在 test-db 里：要一块 GPU 和一个另外起着的 infero。
# 评测集与算分逻辑（internal/understanding/testdata/category_eval/、
# category_evalset_test.go）不带标签，test-db 每次都跑；这里跑的是对真引擎的那一半，
# 打印 Top-1 / Top-3、按余弦与按分差的阈值表、推错的样本。换模型之后必须重跑，
# 并按表重定 understanding.DefaultCategoryGate —— 余弦的尺度是模型相关的，失效时不报错。
# 不碰数据库。
category-eval:
	@test -n "$$KEEL_EMBED_ENDPOINT" || { echo "要设 KEEL_EMBED_ENDPOINT（例如 http://127.0.0.1:18081）指向一个跑着的 infero"; exit 1; }
	go test -count=1 -tags keel_category_eval -run TestCategoryEval -v ./internal/understanding/

# 推理引擎的替身（internal/inference/fake）带编译标签，默认构建里不存在——
# 那是有意的（生产路径够不着它，理由写在那个包的 doc.go 里）。代价是
# 「替身跑不出真实语义相关性」那条测试默认也不会跑，于是它会慢慢烂掉。
# 所以上面那行把它接回 test-db：一条不在闸门里的测试，等于没有。

# 对**真的跑着的**推理引擎（infero）跑一次 keel-integration.md 的三条判据。
#
# 它不在 test-db 里，因为它要一块 NVIDIA GPU、1.2 GB 权重和一个另外起着的进程。
# 但它必须存在：internal/inference 别的所有测试用的都是假引擎（httptest），
# 它们能证明客户端的判断力，证明不了「这套东西真的能算出语义相近」——
# 而那正是 M3 的验收标准（搜「连衣裙」能返回相关商品）。
#
# 起引擎（要 GPU；infero 的 CPU 后端 --features cpu 数值一致但单线程、单条约 5 秒，
# 跑这条会慢且可能超时，见 docs/电商系统-总体架构.md §1「那个缺口」）：
#     ./scripts/infero-up.sh
# 然后（**两样都要**：真引擎 + 真数据库，理由见下）：
#     KEEL_EMBED_ENDPOINT=http://127.0.0.1:18081 make test-engine
#
# ## 为什么是两个包
#
# 三条判据落在两个包里，因为判据一（归一化要从 PostgreSQL 读回来验）需要数据库，
# 而 internal/inference **不能**依赖 internal/repository —— 后者依赖前者
# （要 inference.Dim），反过来 import 是一个环。
#
#   internal/inference  判据二（语义 margin）、判据三（引擎挂了要报错）+ 协议层拒绝
#   internal/repository 判据一（向量写进库再读回来重算 L2 范数）
#
# 代价说明白：这条目标因此同时要 KEEL_EMBED_ENDPOINT 和一个迁好的数据库
# （PGHOST/PGPORT，和 test-db 同一套）。少一样都会 Fatal，不会 Skip。
#
# 不再需要 -p 1：internal/repository 的测试经 internal/testdb 用自己的库
# （keel_test_repository），理由见 test-db 上面那段。
#
# 刻意不 Skip：没配 KEEL_EMBED_ENDPOINT 时那些测试 Fatal 而不是 Skip。
# 一条会自己跳过的测试，在它该报警的时候是静默的。
test-engine: goose-bin
	go test -count=1 -v -timeout=$(TEST_TIMEOUT) -tags keel_real_engine ./internal/inference/ ./internal/repository/

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
# 版本钉在 scripts/fetch-dtmrs.sh 里（v0.12.0），examples/dtmrs-embedded 调的
# 也是同一份脚本 —— 两份脚本就是两个版本，而它们错开时的症状是
# 「例子绿、服务红」，报错停在 C ABI 的某个符号上，不指向真因。
DTMRS_DIR := $(ROOT)/third_party/dtmrs
DTMRS_LIB := $(DTMRS_DIR)/lib/libdtmrs.so
# 独立部署形态的协调器二进制（同一个脚本、同一个 tag 编出来）：远程客户端与两库测试真起一个它
# （internal/dtm/dtmserver）。
DTMRS_BIN := $(DTMRS_DIR)/bin/dtmrs

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

$(DTMRS_BIN):
	@echo "==> 没找到 $(DTMRS_BIN)（0.12 起测试要真起一个独立部署的协调器），重新跑一遍 dtmrs-deps"
	@$(MAKE) dtmrs-deps

# ---------------------------------------------------------------------------
# 版本号
# ---------------------------------------------------------------------------
#
# 三个值经 -ldflags -X 注入 internal/buildinfo。为什么不靠 Go 自带的 VCS 烧录
# （go 1.18 起 `go build` 会自动写 vcs.revision）：**镜像里拿不到**，
# docker/Dockerfile 的构建上下文把 .git 排除在外（.dockerignore 第 3 行），
# 而发布出去的恰恰是镜像。详见 internal/buildinfo/buildinfo.go 的包注释。
#
# VERSION 可以从外面覆盖（`make build VERSION=v0.1.0`）。默认值取 git describe：
# 打过 tag 就是 tag 名，没打过是 `<最近的tag>-<距离>-g<sha>`，一个 tag 都没有时
# 回落到 `dev-<sha>`。**不写死成某个版本号** —— 一个没被注入的构建应当说自己是
# 开发构建，而不是冒充某一版。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
# UTC + RFC3339。本地时区会让两台机器上同一次提交编出两个不同的「构建时间」。
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BUILDINFO := github.com/keel/keel/internal/buildinfo
LDFLAGS   := -X $(BUILDINFO).Version=$(VERSION)              -X $(BUILDINFO).Commit=$(COMMIT)              -X $(BUILDINFO).Date=$(DATE)

# `go build ./...` 不落产物（除了缓存），所以这里同时编一份带版本号的二进制到
# build/ —— 那才是 -ldflags 看得见效果的地方。两条都留着：前者是编译期检查
# （所有包都要能编过，包括没有 main 的），后者是产物。
build: $(DTMRS_LIB)
	go build ./...
	go build -trimpath -ldflags '$(LDFLAGS)' -o build/keel ./cmd/keel

# 打印将要注入的版本，给 CI 和「这次到底编出的是什么」用。
version:
	@echo "VERSION=$(VERSION)"
	@echo "COMMIT=$(COMMIT)"
	@echo "DATE=$(DATE)"

# Flutter 买家端（flutter_app/）。用官方 stable：PATH 上默认的是 flutter_ohos。
FLUTTER ?= $(HOME)/development/flutter/bin/flutter
FLUTTER_APP := $(ROOT)/flutter_app
# flutter 工具链内部还会调 PATH 上的 dart（analysis server 等）。PATH 上默认是 flutter_ohos 的 dart，
# 版本对不上会崩（实测 flutter analyze 报 analysis server exited with code 64），所以把这个 SDK 的 bin 放最前。
# FLUTTER_GIT_URL 指向 ohos 的镜像时，官方 SDK 会一直警告「上游不一致」，这里也清掉。
FLUTTER_ENV := PATH=$(dir $(FLUTTER)):$$PATH FLUTTER_GIT_URL=

flutter-get:
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) $(FLUTTER) pub get

flutter-analyze:
	# 用 dart analyze 而不是 flutter analyze：后者在这台机器上启动 analysis server 时崩（exit 64，
	# 实测），dart analyze 读同一份 analysis_options.yaml（含 flutter_lints），检查范围相同。
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) $(dir $(FLUTTER))dart analyze --fatal-infos

flutter-test:
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) $(FLUTTER) test

flutter-e2e-web:
	FLUTTER=$(FLUTTER) bash $(FLUTTER_APP)/tool/e2e_web.sh

# Android SDK / JDK 17：找法是 ANDROID_HOME → ~/Library/Android/sdk → Homebrew，按顺序取第一个存在的。
FLUTTER_ANDROID_HOME ?= $(or $(ANDROID_HOME),$(firstword $(wildcard $(HOME)/Library/Android/sdk/platforms /opt/homebrew/share/android-commandlinetools/platforms)))
FLUTTER_JAVA_HOME ?= $(or $(JAVA_HOME),$(shell /usr/libexec/java_home -v 17 2>/dev/null))

# 编译检查：Web / Android / iOS（不签名）。服务地址用 KEEL_API_BASE 注入（原生必须注入）。
flutter-build:
	@test -n "$(KEEL_API_BASE)" || (echo "要设 KEEL_API_BASE" && exit 1)
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) $(FLUTTER) build web
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) ANDROID_HOME=$(FLUTTER_ANDROID_HOME:/platforms=) JAVA_HOME=$(FLUTTER_JAVA_HOME) $(FLUTTER) build apk --dart-define=KEEL_API_BASE=$(KEEL_API_BASE)
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) $(FLUTTER) build ios --no-codesign --dart-define=KEEL_API_BASE=$(KEEL_API_BASE)

# 装到连着的 iPhone（发版前真机验 / 想玩一下时）。签名团队用 KEEL_IOS_TEAM 传，不写进工程。
flutter-ios-install:
	@test -n "$(KEEL_API_BASE)" || (echo "要设 KEEL_API_BASE" && exit 1)
	@test -n "$(KEEL_IOS_TEAM)" || (echo "要设 KEEL_IOS_TEAM（security find-identity -v -p codesigning）" && exit 1)
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) $(FLUTTER) build ios --config-only --release --dart-define=KEEL_API_BASE=$(KEEL_API_BASE)
	cd $(FLUTTER_APP)/ios && xcodebuild -workspace Runner.xcworkspace -scheme Runner -configuration Release -sdk iphoneos \
	  -derivedDataPath ../build/ios-derived DEVELOPMENT_TEAM=$(KEEL_IOS_TEAM) CODE_SIGN_STYLE=Automatic -allowProvisioningUpdates -quiet build
	DEV=$$(xcrun devicectl list devices | awk '/available/ && /iPhone/ {print $$3; exit}'); \
	  xcrun devicectl device install app --device $$DEV $(FLUTTER_APP)/build/ios-derived/Build/Products/Release-iphoneos/Runner.app && \
	  xcrun devicectl device process launch --device $$DEV dev.keel.keelBuyer

# 装到连着的安卓机（USB 或 adb 无线连上的）：release、只打 arm64，装完拉起。
# 连着多台时用 ANDROID_SERIAL=<adb devices 里的序列号> 指定。
FLUTTER_ADB ?= $(FLUTTER_ANDROID_HOME:/platforms=)/platform-tools/adb
flutter-android-install:
	@test -n "$(KEEL_API_BASE)" || (echo "要设 KEEL_API_BASE" && exit 1)
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) ANDROID_HOME=$(FLUTTER_ANDROID_HOME:/platforms=) JAVA_HOME=$(FLUTTER_JAVA_HOME) \
	  $(FLUTTER) build apk --release --target-platform android-arm64 --dart-define=KEEL_API_BASE=$(KEEL_API_BASE)
	$(FLUTTER_ADB) install -r $(FLUTTER_APP)/build/app/outputs/flutter-apk/app-release.apk
	$(FLUTTER_ADB) shell am force-stop dev.keel.keel_buyer
	$(FLUTTER_ADB) shell monkey -p dev.keel.keel_buyer -c android.intent.category.LAUNCHER 1 >/dev/null

# 微信小程序（mp-flutter）。产物 flutter_app/build/weapp，用微信开发者工具打开。
# --no-licenses：第三方许可证清单不打进包（许可证页能开、不列第三方包），省体积。常用字合一字体默认打进 pkg-cjk。
flutter-build-mp:
	@test -n "$(KEEL_API_BASE)" || (echo "要设 KEEL_API_BASE" && exit 1)
	cd $(FLUTTER_APP) && $(FLUTTER_ENV) dart run flutter_miniprogram --flutter $(FLUTTER) --no-licenses --dart-define=KEEL_API_BASE=$(KEEL_API_BASE)

flutter-generate:
	python3 $(ROOT)/scripts/gen_dart_schema.py
