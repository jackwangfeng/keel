# dtmrs 嵌入式协调器 · 可复现验证

这个例子回答一个问题：**Keel 最核心的架构主张能不能成立。**

架构文档说「一套代码，从单机长到分布式」——事务编排代码在进程内与跨服务两种
形态下完全一致，迁移只改分支地址那一行。这里把它跑出来。

顺带验证了 ADR #1 那个悬了很久的前提。原先写的是「放弃 dtmrs 嵌入式模式
（除非 cgo 打通）」，实测结论是：**cgo 不需要「打通」，它本来就是通的。**

---

## 跑起来

版本：**dtmrs v0.11.1**（仓库根目录的 `scripts/fetch-dtmrs.sh` 里钉死）。

```bash
make deps      # 取回 dtmrs 并构建 libdtmrs.so（需要 Rust 1.88+）
make verify    # 跑全部五项验证
```

单项：

```bash
make run-saga       # SAGA 提交 / 逆序补偿 / 拉取式分支 / 提交期校验
make run-topology   # 同一段编排，local 与 remote 两种形态
make run-tcc        # TCC：Try 全成功 -> Confirm；Try 失败 -> 逆序 Cancel
make race           # -race 跑一遍
make run-barrier    # 子事务屏障三种异常（需要 PostgreSQL）
```

屏障那一项要真数据库——它的意义就在于与业务 SQL 同事务提交：

```bash
docker run -d --name keel-dtmrs -e POSTGRES_PASSWORD=x -e POSTGRES_DB=dtm \
    -p 55433:5432 postgres:16
make run-barrier
```

> 不需要 PostgreSQL。默认落 sqlite，`DTMRS_DSN=postgres://...` 可切换。

---

## 它证明了什么

### 一、「只改一行」是真的

`cmd/topology` 把同一段编排跑两遍，唯一的差别是这两个字符串：

```go
stockAction, stockCompensate := "local://deduct_stock", "local://deduct_stock_undo"
if mode == "remote" {
    stockAction     = "http://127.0.0.1:18089/stock/deduct"
    stockCompensate = "http://127.0.0.1:18089/stock/undo"
}
```

输出：

```
mode=local   status=succeed
    进程内调用  create_order       branch=02 op=action
    进程内调用  deduct_stock       branch=01 op=action
mode=remote  status=succeed
    经由 HTTP   deduct_stock       branch=01 op=action
    进程内调用  create_order       branch=02 op=action
```

库存分支从进程内函数调用变成真的走 HTTP，订单分支保持进程内，两种形态都成功。
**业务逻辑、编排结构、补偿关系一字未改。**

### 二、SAGA 的四种行为

`cmd/saga` 覆盖：正向提交、失败后逆序补偿、拉取式异步分支、
以及未注册的 `local://` 名字在**提交期**就被拒绝（而不是执行到一半才炸）。

### 三、TCC 可用（0.11 新增）

0.8 的 C ABI 只有 `dtmrs_submit_saga`，这曾是「预售定金 / 多仓调拨」唯一的拦路石。
0.11 导出 22 个符号（0.8 是 11 个，**只增不减**），TCC / XA / 二阶段消息 /
workflow 全部可用。

`cmd/tcc` 两条路径都验过：全部 Try 成功 → Submit → 逐个 Confirm；
第二步 Try 失败 → Abort → **逆序** Cancel（02 先于 01）。

TCC 的一阶段由调用方自己跑，所以它是按 gid 的一串调用而不是一次提交。
两条纪律写在 `dtmrs/tc.go` 的注释里，都有代价：先 register 再跑 Try
（反过来 Try 冻结的资源 TC 不知道），以及 Submit 之后不能 Abort。

### 四、子事务屏障（Go 侧自己实现）

三种异常实测通过：**重复请求**、**空回滚**（补偿先到、正向从未执行）、
**悬挂**（补偿已到，迟到的正向不得执行）。另有 50 协程并发投递同一分支，
结果恰好一次 `Execute`、49 次 `Duplicated`。

### 五、崩溃后跨进程恢复

```bash
go run ./cmd/saga fail      # 提交后不 close 直接 exit(7)
RESUME=1 go run ./cmd/saga  # 重启，未完成的事务被驱动到终态
```

---

## 并发行为：每个阻塞的 cgo 调用占一个 OS 线程

这是当初列为「最高优先级剩余风险」的一项，已实测。结论比预想的清楚：

**线程数跟的是「同时卡在 cgo 里的调用数」，不是总工作量。**
2000 个 SAGA、handler 各阻塞 50ms，只改提交并发度：

| 提交并发 | 峰值 OS 线程 | 吞吐 |
|---|---|---|
| 2000（不限制） | 1213 | 246.8 saga/s |
| 256 | 290 | 247.2 saga/s |
| 32 | 70 | 243.7 saga/s |

**吞吐三者几乎相同。** 限制进入 cgo 的并发度不花任何代价，却省掉一千多个线程。

超出 Go 的线程上限（默认 10000）不是返回错误，是**进程直接死**：

```console
$ MAX_THREADS=100 ./stress push 2000 50        # 不限制提交并发
runtime: program exceeds 100-thread limit
fatal error: thread exhaustion

$ MAX_THREADS=100 ./stress push 2000 50 32     # 同样上限，提交并发限到 32
执行阶段峰值线程=64   吞吐=221.5 saga/s        # 安然跑完
```

所以规则很简单：**所有进入 dtmrs 的调用都要过一个有界的信号量。**
`SubmitSaga`、`WaitFinal` 都是阻塞 cgo 调用，`WaitFinal` 尤其——
它按设计就要等到终态，n 个并发等待就是 n 个线程。

> **我第一版测错了。** 最初以为线程增长来自「Go 的分支 handler 阻塞了 tokio worker」，
> 于是对比推模式与拉模式，结果两者线程数一样高——而拉模式的 handler 跑在普通
> goroutine 里、根本不占 M。查下去才发现增长来自我自己那 n 个并发的
> `SubmitSaga` 和 `WaitFinal`。
>
> 记在这里是因为：**这个误判很容易再犯一次**。看到「cgo + 线程暴涨」
> 第一反应是怪回调，但回调只是众多阻塞调用中的一种。

> **另一个观察**：Go 不回收已创建的 M，线程数只涨不落。峰值即终值。

## 两个会咬人的坑（都已在代码里处理）

### 1. `-race` 下的 checkptr 崩溃

把 `cgo.Handle` 直接塞进 `unsafe.Pointer` 会在 `-race` 下必然崩：

```go
// ❌ fatal error: checkptr: pointer arithmetic result points to invalid allocation
C.dtmrs_register(tc, name, handler, unsafe.Pointer(uintptr(h)))
```

必须把 handle 放进 C 分配的内存（见 `dtmrs/tc.go` 的 `Register`）：

```go
box := C.malloc(C.size_t(unsafe.Sizeof(C.uintptr_t(0))))
*(*C.uintptr_t)(box) = C.uintptr_t(h)
```

**这个坑 100% 会在开了 `-race` 的 CI 上命中**，而本机不开 race 跑起来一切正常。

### 2. Go panic 不能穿过 C 栈

分支 handler 里的 panic 会直接搞死进程。每个 handler 都该自己 `defer recover()`，
并且——**返回 `Unknown` 而不是 `Failure`**：

| 返回 | 协调器的反应 |
|---|---|
| `Failure` | 立刻触发全局补偿 |
| `Unknown` | 重试 |

panic 意味着「我不知道业务做没做」。返回 `Failure` 是在断言「肯定没做」，
而那可能是错的——业务 SQL 也许已经提交了，panic 发生在之后。

---

## 已知边界（实测，不是推断）

**子事务屏障不在 C ABI 里，也不可能在。**

dtmrs 的 `decide()` 签名是 `decide(&mut self, tx: &mut Transaction)`——
它接收**调用方的事务**，因为屏障记录必须与业务变更在同一个本地事务内提交，
这正是屏障成立的全部意义。Go 侧事务握在 pgx 手里，没有办法递过 C 边界。

所以这不是 dtmrs 的缺口，是**结构上的必然**：任何非 Rust 宿主都得自己实现
那三十行。算法见 `barrier/barrier.go`，与 `dtmrs-barrier/src/lib.rs` 逐行对应。

**引入 cgo 意味着失去 `CGO_ENABLED=0` 静态编译。** 静态链接 `libdtmrs.a` 可行，
单文件二进制约 15 MB，但 dtmrs 上游不提供预编译产物、CI 只跑 ubuntu-latest。
Keel 要么要求贡献者装 Rust，要么自建多平台预编译流水线。
**这是嵌入式形态最大的真实成本，别低估。**

---

## 这个例子**没有**证明的事

诚实地说清楚边界，比多列几条战果有用：

- **未验证 macOS / arm64 构建**。上游 CI 只跑 ubuntu-latest。
- Go panic 跨 C 栈只做了理论分析，没有实际压测。

**这个例子证明的是「demo 能跑通」，不是「能上生产」。**

已补完的：并发线程行为、屏障三种异常、TCC 两条路径。
仍未做的：macOS / arm64 构建（上游 CI 只跑 ubuntu-latest，本机也没有那两个环境），
以及 Go panic 跨 C 栈的实际压测（目前只有理论分析与 defer recover 的写法约定）。

---

## 目录

```
dtmrs/          C ABI 的 Go 绑定（TC 类型、分支注册、拉取式任务、TCC）
barrier/        子事务屏障的 Go 实现（C ABI 不提供，见上）
cmd/saga/       SAGA 四种行为
cmd/topology/   「只改一行」的实证
cmd/tcc/        TCC 两条路径
cmd/barrier/    屏障三种异常（需 PostgreSQL）
```

`lib/`、`include/`、`bin/` 都是构建产物，不入库——
`.so` 是平台相关的，而头文件必须与 `.so` 同版本，分开管理迟早对不上。

取回与构建由仓库根目录的 `scripts/fetch-dtmrs.sh` 完成（`make deps` 调它），
版本**钉死在一个 tag** 上。上游改了 C ABI 而这里悄悄跟着变，是最难查的一类问题。

脚本在根目录而不在这里，是因为 M2 起主模块也嵌入 dtmrs，两边必须用同一个
`.so`。两份脚本就是两个 REF，它们会在某次升级里错开，而症状是
「例子绿、服务红」，报错停在 C ABI 的某个符号上，不指向真因。
