# dtmrs 嵌入式协调器 · 可复现验证

这个例子回答一个问题：**Keel 最核心的架构主张能不能成立。**

架构文档说「一套代码，从单机长到分布式」——事务编排代码在进程内与跨服务两种
形态下完全一致，迁移只改分支地址那一行。这里把它跑出来。

顺带验证了 ADR #1 那个悬了很久的前提。原先写的是「放弃 dtmrs 嵌入式模式
（除非 cgo 打通）」，实测结论是：**cgo 不需要「打通」，它本来就是通的。**

---

## 跑起来

版本：**dtmrs v0.11.0**（`scripts/fetch-dtmrs.sh` 里钉死）。

```bash
make deps      # 取回 dtmrs 并构建 libdtmrs.so（需要 Rust 1.82+）
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

- **未验证高并发下的 OS 线程行为**。tokio runtime 与 Go runtime 共存、
  `block_on` 占 OS 线程，单机 demo 跑通不代表压力下不出事。**这是最高优先级的剩余风险。**
- **未验证 macOS / arm64 构建**。上游 CI 只跑 ubuntu-latest。
- Go panic 跨 C 栈只做了理论分析，没有实际压测。

**这个例子证明的是「demo 能跑通」，不是「能上生产」。** 两者之间还差 5–7 人日。

---

## 目录

```
dtmrs/          C ABI 的 Go 绑定（TC 类型、分支注册、拉取式任务、TCC）
barrier/        子事务屏障的 Go 实现（C ABI 不提供，见上）
cmd/saga/       SAGA 四种行为
cmd/topology/   「只改一行」的实证
cmd/tcc/        TCC 两条路径
cmd/barrier/    屏障三种异常（需 PostgreSQL）
scripts/        取回并构建 libdtmrs
```

`lib/`、`include/`、`bin/` 都是构建产物，不入库——
`.so` 是平台相关的，而头文件必须与 `.so` 同版本，分开管理迟早对不上。

`scripts/fetch-dtmrs.sh` 把上游版本**钉死在一个 commit** 上。
上游改了 C ABI 而这里悄悄跟着变，是最难查的一类问题。
