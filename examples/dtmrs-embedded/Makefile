# dtmrs 嵌入式协调器示例
#
# 先 `make deps` 取回并构建 libdtmrs，之后才能 build/run。

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

.PHONY: help deps build run-saga run-topology race clean verify

help:
	@echo "make deps          取回 dtmrs 并构建 C ABI 动态库（需要 Rust 1.88+，调 ../../scripts/fetch-dtmrs.sh）"
	@echo "make build         编译两个示例程序到 bin/"
	@echo "make run-saga      SAGA 提交 / 逆序补偿 / 拉取式分支 / 提交期校验"
	@echo "make run-topology  同一段编排跑 local 与 remote 两种形态"
	@echo "make run-tcc       TCC：Try 全成功 -> Confirm；Try 失败 -> 逆序 Cancel"
	@echo "make run-barrier   子事务屏障三种异常（需要 PostgreSQL，见 README）"
	@echo "make race          -race 跑一遍（验证 cgo.Handle 的 checkptr 处理）"
	@echo "make verify        以上全部，外加崩溃恢复"
	@echo "make clean         清掉构建产物与 sqlite 文件"

lib/libdtmrs.so:
	@$(MAKE) deps

# 调仓库根目录那一份，而不是自己留一份。
#
# M2 把 dtmrs 接进了主模块，于是主模块和这个例子都要同一个 .so。两份脚本意味着
# 两个 REF，它们会在某次升级里错开，而症状是「例子绿、服务红」，报错停在 C ABI
# 的某个符号上，不指向真因。版本钉子只有 scripts/fetch-dtmrs.sh 那一处。
deps:
	../../scripts/fetch-dtmrs.sh "$(CURDIR)"

build: lib/libdtmrs.so
	go build -o bin/saga ./cmd/saga
	go build -o bin/topology ./cmd/topology
	go build -o bin/tcc ./cmd/tcc
	go build -o bin/barrier ./cmd/barrier

run-saga: lib/libdtmrs.so
	go run ./cmd/saga

run-topology: lib/libdtmrs.so
	@go run ./cmd/topology local
	@echo
	@go run ./cmd/topology remote

run-tcc: lib/libdtmrs.so
	@go run ./cmd/tcc
	@echo
	@go run ./cmd/tcc fail

run-barrier: lib/libdtmrs.so
	go run ./cmd/barrier

race: lib/libdtmrs.so
	go run -race ./cmd/saga >/dev/null && echo "-race 干净"

verify: lib/libdtmrs.so
	@echo "=== SAGA ==="            && go run ./cmd/saga
	@echo "=== 两种形态 ==="         && $(MAKE) -s run-topology
	@echo "=== 崩溃恢复 ==="         && (go run ./cmd/saga fail || true) >/dev/null \
	                               && RESUME=1 go run ./cmd/saga
	@echo "=== TCC ==="              && $(MAKE) -s run-tcc
	@echo "=== -race ==="           && $(MAKE) -s race
	@echo ""
	@echo "屏障验证需要 PostgreSQL，单独跑：make run-barrier"

clean:
	rm -rf bin lib include .dtmrs-src /tmp/dtmrs_saga_demo.db /tmp/dtmrs_topology_*.db
