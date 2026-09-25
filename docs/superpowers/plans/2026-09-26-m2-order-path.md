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

### 四、构建形态要变（已完成，且原计划有两处说错了）

`CGO_ENABLED=1` + 构建阶段装 Rust 并编出 `libdtmrs.so` + 最终镜像换成
debian-slim 底座。已落地，镜像 26.7 MB → 118 MB。

两处原计划写错、由实测改正的：

- **Rust 的下限是 1.88，不是 1.82。** 1.82 是 dtmrs 自己的 `rust-version`，
  但卡住构建的是它 `Cargo.lock` 锁定的依赖树（`home@0.5.12 requires rustc 1.88`）。
  所以镜像里不能 `apt install cargo rustc`（Debian trixie 给的是 1.85.1），
  换成官方 `rust:1.90-slim-trixie`。
- **`/etc/passwd` 不是换底座自动解决的。** 文件确实有了，但里面没有 65532。
  现在显式建了 nonroot 用户。tzdata 与 `/tmp` 则确实随底座一起来了。

### 五、协调器的存储必须显式配置（原计划没有定，由 Task 3 定下）

`KEEL_DTM_DSN`，**没有默认值，空着拒绝启动**。

实测两条：用 `keel_app` 调 `Start()` 报 `permission denied for schema public`
（协调器要建表还要 `ALTER TABLE ADD COLUMN`）；换管理员角色能建出来，但那三张
表（`trans_global` / `trans_branch_op` / `auth_token`）落进业务库 `public` 之后
**`internal/db` 的四道闸门当场全红**，而且应用从此握着一份能绕过 RLS 的凭据。

不给默认值的理由：这份状态丢了，正向阶段已扣的库存与已核销的券再没人回补，
架构 §5 的「少卖」从可恢复变成永久漏账。一个「反正能跑起来」的默认值会让每个
忘配它的部署安静地拿到这个结局。

> **`barrier` 不是 dtmrs 建的**（原计划说是，错了）。把 C ABI 的 `dtmrs_start`
> 指向空库跑一遍，长出来的只有协调器自己的三张状态表。barrier 由
> `dtmrs-barrier` crate 的 `BranchBarrier::migrate` 建，而 `dtmrs-ffi` 没有这个
> 依赖——这不是缺口是必然：`decide()` 接收调用方的事务，结构上过不了 FFI。
> 现在由本项目的 `00008_barrier.sql` 建，`keel_app` 的 GRANT 面**只有 INSERT**。
>
> 那条 GRANT 面同时是一道**形状约束**：实测 `ON CONFLICT DO NOTHING` 跑得通
> （首次 `INSERT 0 1`、重复 `INSERT 0 0`，正是算法要的两个值），而写成
> `ON CONFLICT (gid,...) DO NOTHING` 报 `permission denied`——带冲突目标要额外的
> SELECT 权。屏障的 SQL 因此不许写冲突目标，权限会当场拒绝，而不是让一个错误的
> 幂等语义悄悄生效。

---

## 任务切分

| # | 任务 | 依赖 | 可并发 |
|---|---|---|---|
| 1 | 订单域 schema + RLS（orders / order_items / inventories / inventory_logs） | — | ✅ 已完成 |
| 3 | dtmrs 嵌入应用 + 构建形态变更 + CI + `barrier` 建表 | — | ✅ 已完成 |
| 1.5 | 买家身份：`users` / `user_identities` + `/auth/login` + bearer 中间件 | 1 | |
| 2 | 屏障从 `examples/` 产品化进 `internal/repository` | 3 | |
| 4 | `POST /orders/preview`（无副作用试算） | 1.5 | |
| 5 | `POST /orders`：SAGA 正向**两**分支（库存 / 建单） | 1.5,2,4 | 汇合点 |
| 5.5 | 券：`coupon_templates` / `user_coupons` / `coupon_scopes` + SAGA 第三分支 | 5 | 可推到 M2 收口 |
| 6 | 超时未支付的补偿定时任务 | 5 | |
| 7 | 支付回调 + 二阶段消息 | 5 | |

> **任务 1.5 是被一条刻意留红的线逼出来的。** Task 1 建 `orders` 时没有给
> `user_id` 加外键（`users` 还不存在，没有落点），也**刻意没有**把它登记进
> `fk_missing_ok`——登记等于把提醒关掉。于是 `users` 一被建出来，
> `TestForeignKeysAreNotSilentlyMissing` 当场红，那份迁移不补复合外键就过不去。

## 每个任务的硬性要求（沿用 M1）

1. **变异验证**：对每一处关键断言，删掉它声称守护的那段逻辑（只改语义、
   不破坏可编译性），确认它真的红，然后恢复。逐条列出。
2. 冷库 `make test-db` 全绿、`./scripts/check-all.sh` 全绿、端到端全绿。
3. 新表必须在 `db/tenancy.json` 里有类别；不在清单里的按 `tenant` 类查。
4. 发现任务书自相矛盾或与仓库既有决定冲突，**先说出来再动手**。

## 券为什么从任务 5 里拆出来

架构 §5 的正向阶段画的是三分支：库存、券、建单。而券的三张表
（`coupon_templates` / `user_coupons` / `coupon_scopes`）一张都还没建——
和 `users` 当初的情况一样，它是任务 5 的隐藏前置。

判据是 M2 的产出标志：**能下单能支付**。一笔没有用券的订单是完整的订单，
所以券不在这条判据里。而 SAGA 本身要验的东西（正向、补偿、屏障幂等、
崩溃重放）两个分支就已经全都走到了——第三个分支是重复同一种结构，不是
多验一件事。

拆出来的代价要说清楚：任务 5 落地时 `docs/电商系统-总体架构.md` §5 的流程图
与实现不一致（图上有券，代码里没有）。**这不许靠「以后会补」糊过去**——
任务 5 必须在那张图旁边注明当前实现到哪一步，理由与本节一致。
README 的「还没在盒子里的」那一节是同一套做法。

## 任务 5 的一条硬约束（来自任务 2）

`WithSagaBranch` 不解析 gid，租户从 ctx 取（与 `WithTenant` 同规矩）。于是
「ctx 里的租户」与「gid 里的租户」一致性，由唯一的产生者
`dtm.TenantContextFromGID` 按构造保证，repository 那一层**没有复核**。

让 repository import `internal/dtm` 会把 cgo 拖进数据访问层，还要让它认得 gid
的文法（多一份会漂移的真相），所以刻意不做。

**因此任务 5 只许经 `dtm.TenantContextFromGID` 造那个 ctx，不许手搓。**
手搓一个带着别家租户的 ctx，屏障与业务都会老老实实跑在那个错租户下。

## 已知会在 M2 路上撞到的

- `barrier` 的 GRANT 面现在是按 dtmrs 的 `migrate()` 形态猜的四权，
  接上之后应按实际调用面收窄（`TestAppRoleGrantSurface` 会把它钉住）。
- `parent-scoped` 与 `shared-reference` 两条清单分支今天在 CI 里是死代码，
  M2 建出 `inventories` 与两张 transitions 之后才会真正被执行到。
- SAGA 在正向阶段就真实扣减，**超卖为零、少卖存在**（架构 §5 已论证）。
  这是刻意接受的取舍，但它把「超时补偿任务跑不跑得起来」从运维问题
  变成了正确性问题——少卖会变成永久漏卖。任务 6 不是可选项。
