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

# 经 tools 子模块跑工具：版本由 tools/go.mod 锁定
GORUN := cd $(TOOLS) && go run

# TS 侧生成器没有 Go 那样的模块锁，只能在这里钉死版本号
OPENAPI_TS := openapi-typescript@7.13.0

# 输出位置由后续任务按实际骨架覆盖，例如：
#   make generate GO_OUT=internal/api/openapi.gen.go TS_OUT=web/src/api/schema.d.ts
GO_OUT     ?= $(ROOT)/build/codegen/openapi.gen.go
GO_PACKAGE ?= api
GO_MODE    ?= types
TS_OUT     ?= $(ROOT)/build/codegen/schema.d.ts

.PHONY: help generate generate-go generate-ts tools-versions contract-check

help:
	@echo "make generate       生成 Go + TS 两侧契约产物"
	@echo "make generate-go    只生成 Go 侧（GO_OUT / GO_PACKAGE / GO_MODE 可覆盖）"
	@echo "make generate-ts    只生成 TS 侧（TS_OUT 可覆盖）"
	@echo "make contract-check 校验 3.1 可空语义没有被生成器悄悄改掉"
	@echo "make tools-versions 打印钉住的工具版本"

generate: generate-go generate-ts

generate-go:
	@mkdir -p $(dir $(GO_OUT))
	$(GORUN) github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen \
		-package $(GO_PACKAGE) -generate $(GO_MODE) "$(CONTRACT)" > $(GO_OUT)
	@echo "generated $(GO_OUT)"

generate-ts:
	@mkdir -p $(dir $(TS_OUT))
	npx --yes $(OPENAPI_TS) "$(CONTRACT)" -o $(TS_OUT)

tools-versions:
	@cd $(TOOLS) && go list -m -f "{{.Path}} {{.Version}}" \
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
