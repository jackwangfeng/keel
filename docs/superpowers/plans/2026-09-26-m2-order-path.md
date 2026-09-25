# M2 实施计划：下单主链路 + dtmrs SAGA

目标产出标志（架构 §13）：**能下单能支付（沙箱）**。

M1 的结论是骨架站得住、租户隔离四个方向都没挖穿。M2 是第一次往这个骨架里
写**真实的写路径**，所以本计划的重心不在「实现功能」，而在**M1 那些只在读
路径上验证过的保护，在写路径上还成不成立**。

---

## 开工前已经定下的四件事

这四条都是 M1 收口时用实验定下来的，不要重新讨论，但发现它们错了要说。

### 一、gid 必须携带租户

从 FFI 接口面核实：

- `SubmitSaga(gid, stepsJSON)`，steps 只有 `{"action","compensate"}` 两个地址，**不带业务载荷**
- `BranchFunc func(gid, branchID, op string) int` —— 分支只拿到这三个字符串
- 分支跑在任何 HTTP 请求之外；崩溃重启后由 TC 重放，进程对原请求毫无记忆

推论：分支需要的一切必须能从 gid 推出来。而分支要写库就得
`SET LOCAL app.merchant_id`，它不知道自己是哪个租户；想查表反查，
那张表在 RLS 之下，而 RLS 正需要租户——鸡生蛋。

**所以 gid 的形状是 `order-{merchant_id}-{order_no}`**，架构 §5 的
`order-{order_no}` 要跟着改。不为此开 RLS 豁免表。

### 二、屏障不能靠导出 `pgx.Tx` 来解决

子事务屏障必须和业务写在同一个事务里。而 `repository.WithTenant` 自己
`Begin`/`Commit`，交给业务层的是只有几个方法的 `repository.Tx` 接口，
**没有任何口子把 `pgx.Tx` 交出去**——那是 M1 用编译器堵住的缺口
（`db.New(pool)` 在 repository 包外是编译错误）。

导出一个 `BeginTx` 是最省事的解法，也会把那个缺口原样打开：那条路径上没有
`set_config('app.merchant_id')`，症状是线上偶发 42501。

**方向：把屏障做进 repository 层**，例如
`WithSagaBranch(ctx, merchantID, gid, branchID, op, fn func(Tx) error)`——
它开事务、设租户、调 `barrier.Decide(tx)`，再按判定决定跑不跑 `fn`。
业务分支永远看不到 `pgx.Tx`。实现者可以提出更好的形状，但**不许以
「先导出 BeginTx，以后再收」的方式绕过**。

### 三、`UPDATE 0` 有两种成因，不能混为一谈

扣减库存时 `rows_affected = 0` 可能是：

1. 库存不足（`available_qty >= n` 不满足）→ 应触发补偿
2. **这个 SKU 不属于本租户**（RLS 策略把行挡掉了）→ 是 bug 或攻击，不是业务分支

数据模型 §4 已经写下这条。它需要的是代码，不是文档：扣减语句要能区分
「行存在但数量不够」与「行根本不可见」。

### 四、构建形态要变

`CGO_ENABLED=1` + 构建阶段装 Rust 并编出 `libdtmrs.so` + 最终镜像不能再是
`scratch`（动态链接需要 glibc 与动态链接器）。`docker/Dockerfile:32-40`
已经把这件事和它的时机写清楚了。

换底座之后，M1 记下的 tzdata / `/tmp` / `/etc/passwd` 三个缺失会一并解决。

---

## 任务切分

| # | 任务 | 依赖 | 可并发 |
|---|---|---|---|
| 1 | 订单域 schema + RLS（orders / order_items / inventories / inventory_logs / barrier） | — | 与 3 并发 |
| 2 | 屏障从 `examples/` 产品化进 `internal/`，按上面第二条的形状 | 1 | |
| 3 | dtmrs 嵌入应用 + 构建形态变更 + CI 跟着改 | — | 与 1 并发 |
| 4 | `POST /orders/preview`（无副作用试算） | 1 | 与 2、3 并发 |
| 5 | `POST /orders`：SAGA 正向三分支（库存 / 券 / 建单） | 1,2,3,4 | 汇合点 |
| 6 | 超时未支付的补偿定时任务 | 5 | |
| 7 | 支付回调 + 二阶段消息 | 5 | |

## 每个任务的硬性要求（沿用 M1）

1. **变异验证**：对每一处关键断言，删掉它声称守护的那段逻辑（只改语义、
   不破坏可编译性），确认它真的红，然后恢复。逐条列出。
2. 冷库 `make test-db` 全绿、`./scripts/check-all.sh` 全绿、端到端全绿。
3. 新表必须在 `db/tenancy.json` 里有类别；不在清单里的按 `tenant` 类查。
4. 发现任务书自相矛盾或与仓库既有决定冲突，**先说出来再动手**。

## 已知会在 M2 路上撞到的

- `barrier` 的 GRANT 面现在是按 dtmrs 的 `migrate()` 形态猜的四权，
  接上之后应按实际调用面收窄（`TestAppRoleGrantSurface` 会把它钉住）。
- `parent-scoped` 与 `shared-reference` 两条清单分支今天在 CI 里是死代码，
  M2 建出 `inventories` 与两张 transitions 之后才会真正被执行到。
- SAGA 在正向阶段就真实扣减，**超卖为零、少卖存在**（架构 §5 已论证）。
  这是刻意接受的取舍，但它把「超时补偿任务跑不跑得起来」从运维问题
  变成了正确性问题——少卖会变成永久漏卖。任务 6 不是可选项。
