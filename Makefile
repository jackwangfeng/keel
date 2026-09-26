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
# 客户端（app/，uni-app x）那一份契约产物。**UTS 不是 TypeScript** ——
# 它的类型系统要落到 Kotlin/Swift 上，openapi-typescript 的产物里那些
# 映射类型、条件类型、索引签名都没有对应物（实测结论在 app/README.md）。
# 所以 UTS 侧另生成一份，而不是共用 TS_OUT；生成器是自己的 Python 脚本，
# 不引入新的 npm 生成器版本要钉。
UTS_OUT    ?= $(ROOT)/app/src/api/schema.uts

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

.PHONY: help generate generate-go generate-ts generate-sql generate-uts tools-versions version search-metrics \
	contract-check schema-check app-type-check admin-install admin-type-check admin-test admin-build app-install app-build-h5 app-build-android app-build-mp-weixin app-apk app-apk-e2e app-e2e app-e2e-h5 app-adb-wifi app-ios app-ios-e2e app-e2e-ios \
	sdk-smoke goose-bin migrate migrate-down migrate-status test-db \
	test-engine dtmrs-deps build

help:
	@echo "make generate       生成 Go + TS 两侧契约产物"
	@echo "make generate-go    只生成 Go 侧（GO_OUT / GO_PACKAGE / GO_MODE 可覆盖）"
	@echo "make generate-ts    只生成 TS 侧（TS_OUT 可覆盖）"
	@echo "make generate-uts   只生成客户端 UTS 侧（UTS_OUT 可覆盖）"
	@echo "make generate-sql   跑 sqlc，重生成 internal/repository/internal/db（SQLC_CONFIG 可覆盖）"
	@echo "make contract-check 校验 3.1 可空语义没有被生成器悄悄改掉"
	@echo "make schema-check   用 tsc --strict 检查整个 web/src（含契约产物与 SDK）"
	@echo "make app-type-check 用 tsc --strict 检查 app/src 下全部 .uts"
	@echo "make admin-install  装商家后台（web/admin）的依赖（npm ci，版本由 lock 锁定）"
	@echo "make admin-type-check 用 vue-tsc --strict 检查 web/admin/src 下全部 .ts 与 .vue"
	@echo "make admin-test     跑商家后台的单元测试（围栏几何与坐标系换算、券金额换算、订单与售后的按钮与请求体、铃铛的跳转、营销活动的表单换算）"
	@echo "make admin-build    构建商家后台静态产物（compose 起栈时会自己构建，日常不用跑）"
	@echo "make app-install    装客户端依赖（含 npm 跳过 uts 原生 binding 的绕法）"
	@echo "make app-build-h5   用 DCloud 编译器真编一遍 H5（要先 app-install）"
	@echo "make app-build-mp-weixin  编微信小程序到 app/dist/build/mp-weixin（读 KEEL_API_BASE）"
	@echo "make app-apk        本地打 Android apk（KEEL_API_BASE=http://host:port/api/v1 指定默认服务地址）"
	@echo "make app-apk-e2e    打带自动化运行时的测试包（同样读 KEEL_API_BASE）"
	@echo "make app-e2e        在 Android 真机上跑 app/e2e 下的自动化用例（USB 或无线）"
	@echo "make app-adb-wifi   把 USB 连着的 Android 手机切到无线调试，之后可拔线"
	@echo "make app-ios        本地打 iOS 真机包（KEEL_IOS_TEAM 指定签名团队，KEEL_API_BASE 同上）"
	@echo "make app-ios-e2e    打带自动化运行时的 iOS 测试包"
	@echo "make app-e2e-ios    在 USB 连着的 iPhone 上跑 app/e2e 下的自动化用例"
	@echo "make sdk-smoke      用 TS SDK 对跑着的服务真打一次 GET /products"
	@echo "make tools-versions 打印钉住的工具版本"
	@echo "make goose-bin      把钉在 tools/go.mod 的 goose 编到 bin/goose（migrate 会先调它）"
	@echo "make migrate        把 db/migrations 迁到最新（GOOSE_DBSTRING 可覆盖）"
	@echo "make migrate-down   回滚一个版本"
	@echo "make migrate-status 打印各版本的应用状态"
	@echo "make search-metrics 按店铺 × 策略统计搜索效果（PERIOD 默认 7 days，要管理员连接）"
	@echo "make test-db      跑需要数据库的测试（强制不吃缓存，含替身那一组）"
	@echo "make test-engine    对真的跑着的 infero 跑三条判据（要 GPU + KEEL_EMBED_ENDPOINT + 数据库）"
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

# 客户端的契约产物。生成器是 Python + PyYAML，刻意不是又一个 npm 生成器：
# openapi-typescript 的产物 UTS 吃不下（见 app/README.md 的实测记录），
# 而为了一份 600 行的类型再钉一个 npm 生成器版本不划算。
generate-uts:
	python3 $(ROOT)/scripts/gen_uts_schema.py -o $(UTS_OUT)

# app/src 下全部 .uts 在 --strict 下能不能编译。
#
# 范围由脚本自己核对（tsc 对「范围里没有这个文件」是静默的，
# 和 check_ts_scope.py 挡的是同一个坑）。零 node_modules，只 npx 拉 tsc。
app-type-check:
	python3 $(ROOT)/scripts/check_app_types.py $(TSC)

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
#
# 守的是「偏了不会报错」那一类错：经纬度写反、坐标系没换，服务端都会收下一个
# **合法**的多边形，只是位置偏了几百米，买家被判进错的门店。
#
# `node --test` 直接跑 .ts（Node 24 的类型剥离），不引入测试框架：被测文件
# （src/api/geo.ts）刻意只有 import type，没有运行时依赖，所以这一步**不需要**
# node_modules。别往 geo.ts 里加运行时 import，否则这里会以
# ERR_MODULE_NOT_FOUND 失败。
admin-test:
	cd $(ROOT)/web/admin && node --test src/api/geo.test.ts src/api/money.test.ts src/api/orderRules.test.ts src/api/promotionRules.test.ts src/api/notifications.test.ts

# 构建静态产物到 web/admin/dist。日常不用跑：compose 起栈时在
# docker/Dockerfile.admin 的 node 阶段里构建，产物交给 nginx。
admin-build:
	cd $(ROOT)/web/admin && npm run build

# 装客户端依赖。**不要直接 npm ci** —— 见脚本里那段：npm 11 会把
# @dcloudio/uts-linux-x64-gnu 当成 libc 不匹配跳过，而少了它 uni 的编译器
# 在加载配置时就死，报的是「Cannot find module」，和真因（npm 的 libc 判定）无关。
app-install:
	bash $(ROOT)/app/scripts/install-deps.sh

# 用 DCloud 自己的编译器真编一遍（含 .uvue 模板）。要先 make app-install。
# 它比 app-type-check 盖得多（模板表达式、pages.json、样式），也重得多。
app-build-h5:
	python3 $(ROOT)/scripts/check_app_build.py h5

app-build-android:
	python3 $(ROOT)/scripts/check_app_build.py app-android

# 微信小程序。产物用微信开发者工具打开 app/dist/build/mp-weixin。小程序没有「页面 origin」，
# 和原生 App 一样要绝对地址：KEEL_API_BASE 编进去（见 app/vite.config.js）。
app-build-mp-weixin:
	python3 $(ROOT)/scripts/check_app_build.py mp-weixin

# 本地打 apk：离线 SDK + Gradle，不经 HBuilderX、不上传。前置条件（JDK 17、Android SDK）
# 与流程写在脚本头里。KEEL_API_BASE 是原生 App 的默认服务地址，不设就要在 App 里手填。
app-apk:
	bash $(ROOT)/app/scripts/build-apk.sh

# 真机自动化测试（uni-automator）。app-apk-e2e 打测试包，app-e2e 装到手机上跑用例。
# 两步分开：改用例不必重新打包。怎么接上官方自动化、为什么不走 HBuilderX，见 app/e2e/README.md。
app-apk-e2e:
	bash $(ROOT)/app/scripts/build-apk.sh --e2e

app-e2e:
	cd $(ROOT)/app && npm run test:e2e

# 同一套用例在本机 Chrome 无头里跑（带自动化运行时编 H5 → 同源反代 → playwright）。
# 真机都锁屏时的兜底，一轮不到一分钟；只覆盖 JS / H5 那一层。要 KEEL_API_BASE。
app-e2e-h5:
	bash $(ROOT)/app/scripts/e2e-h5.sh

# 插着线跑一次，之后拔线也能 make app-e2e。手机重启后要重跑。
app-adb-wifi:
	bash $(ROOT)/app/scripts/adb-wifi.sh

# iOS：页面逻辑编译成 JS 跑在 JavaScriptCore，界面原生渲染；本地用离线 SDK + XcodeGen + xcodebuild
# 出真机包（没有模拟器版本，理由见 app/scripts/build-ios.sh 头）。
app-ios:
	bash $(ROOT)/app/scripts/build-ios.sh

app-ios-e2e:
	bash $(ROOT)/app/scripts/build-ios.sh --e2e

app-e2e-ios:
	cd $(ROOT)/app && npm run test:e2e:ios

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

test-db: $(DTMRS_LIB) goose-bin
	go test -count=1 -timeout=$(TEST_TIMEOUT) $(TEST_PKGS)
	@echo "==> 替身那一组（-tags keel_fake_embedder）"
	go test -count=1 -timeout=$(TEST_TIMEOUT) -tags keel_fake_embedder ./internal/inference/...

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
# 起引擎（要 GPU；infero 只有 CUDA / Metal 后端，没有 CPU 后端）：
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
# 版本钉在 scripts/fetch-dtmrs.sh 里（v0.11.1），examples/dtmrs-embedded 调的
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
