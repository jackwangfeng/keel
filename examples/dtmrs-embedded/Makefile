# dtmrs 嵌入式协调器示例
#
# 先 `make deps` 取回并构建 libdtmrs，之后才能 build/run。

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

.PHONY: help deps build run-saga run-topology race clean verify

help:
	@echo "make deps          取回 dtmrs 并构建 C ABI 动态库（需要 Rust 1.82+）"
	@echo "make build         编译两个示例程序到 bin/"
	@echo "make run-saga      SAGA 提交 / 逆序补偿 / 拉取式分支 / 提交期校验"
	@echo "make run-topology  同一段编排跑 local 与 remote 两种形态"
	@echo "make race          -race 跑一遍（验证 cgo.Handle 的 checkptr 处理）"
	@echo "make verify        以上全部，外加崩溃恢复"
	@echo "make clean         清掉构建产物与 sqlite 文件"

lib/libdtmrs.so:
	@$(MAKE) deps

deps:
	./scripts/fetch-dtmrs.sh

build: lib/libdtmrs.so
	go build -o bin/saga ./cmd/saga
	go build -o bin/topology ./cmd/topology

run-saga: lib/libdtmrs.so
	go run ./cmd/saga

run-topology: lib/libdtmrs.so
	@go run ./cmd/topology local
	@echo
	@go run ./cmd/topology remote

race: lib/libdtmrs.so
	go run -race ./cmd/saga >/dev/null && echo "-race 干净"

verify: lib/libdtmrs.so
	@echo "=== SAGA ==="            && go run ./cmd/saga
	@echo "=== 两种形态 ==="         && $(MAKE) -s run-topology
	@echo "=== 崩溃恢复 ==="         && (go run ./cmd/saga fail || true) >/dev/null \
	                               && RESUME=1 go run ./cmd/saga
	@echo "=== -race ==="           && $(MAKE) -s race

clean:
	rm -rf bin lib include .dtmrs-src /tmp/dtmrs_saga_demo.db /tmp/dtmrs_topology_*.db
