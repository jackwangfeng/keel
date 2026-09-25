# M1 工程骨架 · 设计文档

- 日期：2026-09-25
- 状态：待评审
- 前置：`b7f281a`（设计债闭环、多租户、dtmrs 0.11 验证均已合入 main）

---

## 一、目标

把项目从「设计完备但一行代码没有」推进到 **「一条最细的链路真的跑通，
且契约在前后端两侧都能生成代码」**。

路线图给 M1 的产出标志是「契约冻结，前后端可并行」。**空骨架证明不了这句话。**
所以 M1 交付一条端到端打通的读链路，而不是一堆配置文件。

```
OpenAPI 契约
   ├─ codegen → Go server stubs ──┐
   └─ codegen → TS client SDK ────┼→ 一次真调用
                                   │
   Gin handler → service → repository(sqlc)
        → WithTenant(SET LOCAL) → RLS → PostgreSQL
```

选 `GET /products` 这一条，因为它同时踩到最多的东西：契约双向生成、分层纪律、
sqlc、租户上下文、RLS、迁移、种子数据。**不写任何业务逻辑。**

### 为什么 M1 要包含客户端

README 承诺「所有客户端由同一份 OpenAPI 契约生成代码」，而 M1 的产出标志正是
「前后端可并行」。只验证 Go 一侧，等于只证明了一半。

本契约是 OpenAPI **3.1**，用了 4 处 3.1-only 写法（1 处 `type: [integer,'null']`、
3 处 `examples` 数组）。很多主流生成器只完整支持 3.0。**这 4 处能不能被吃下去，
是 M1 必须回答的问题**，不是 M2 才发现的问题。

客户端范围严格限定为**生成的 SDK + 一次真调用**，不建 UI 工程。
UI（React 后台、uni-app 商城）留到后续里程碑。

---

## 二、必须前置的风险验证

**在搭任何骨架之前，先拿真契约喂两个生成器。**

那 4 处 3.1-only 写法能否被吃下去，**未知，不猜**。两条退路都很便宜：
把那 4 处降级成 3.0 写法，或换生成器。**现在花半小时，好过在骨架上盖了三天才发现。**

这是计划的第一个任务，其余任务都依赖它的结论。

---

## 三、技术选型

| 用途 | 选择 | 理由 |
|---|---|---|
| 迁移 | **goose**（纯 SQL 文件） | schema 里有 RLS 策略、`FORCE RLS`、plpgsql 函数、复合外键、部分索引。atlas 的声明式 diff 在这些上面容易打架；goose 是「你写什么它跑什么」 |
| 查询 | **sqlc** | 架构文档已定 |
| Go codegen | 待第一个任务验证后确定 | 需支持 Gin + 尽量支持 3.1 |
| TS codegen | 待第一个任务验证后确定 | 需正确处理 `type: [x,'null']` |
| Web 框架 | **Gin** | 架构文档已定 |
| 连接池 | **pgx/v5 + pgxpool** | RLS 与 sqlc 的验证都基于它 |

> Go / TS 生成器刻意留到第一个任务之后再定。先测再选，不是先选再祈祷。

---

## 四、目录结构

```
keel/
  cmd/keel/                 服务入口
  internal/
    api/                    ← codegen 产物，不手改
    handler/                实现生成的接口：校验 / 鉴权 / 响应封装
    service/                业务规则、事务编排入口
    tenant/                 租户上下文：只导出 FromContext
    repository/
      internal/db/          ← sqlc 产物，不手改
      tenant.go             WithTenant —— 唯一出口
  db/
    migrations/             goose 的 SQL 文件
    queries/                sqlc 的输入
  clients/ts/               生成的 SDK + 调用脚本
  docker/
```

### 关键：用编译器堵住那个缺口

RLS 验证那轮记了一个缺口：sqlc 的 `DBTX` 同时被 `*pgxpool.Pool` 满足，
所以 `db.New(pool)` **能编译过**，而那条路没有事务也就没有 `SET LOCAL`。
当时只能靠 RLS 在运行期拦。

Go 的 `internal` 规则按路径深度限制导入：`internal/repository/internal/db`
**只有 `internal/repository/...` 能 import**。handler 与 service 在物理上拿不到
那个包，唯一入口是：

```go
func WithTenant(ctx context.Context, fn func(*db.Queries) error) error
```

租户**不从参数传**，从 ctx 取（`tenant.FromContext`），取不到直接返回错误。
这样连「传错租户」这个动作都不存在——调用方没有那个参数可传。

于是三层防御：

| 层 | 拦什么 | 何时发现 |
|---|---|---|
| 编译期 | 拿不到没设租户的 Queries | 写代码时 |
| 运行期 | ctx 里没有租户 | 第一次请求 |
| 数据库 | RLS 策略 | 兜底 |

前面那轮验证只有后两层。这一层是 M1 新加的。

平台级操作（建商家、跨租户运维）走单独的 `BYPASSRLS` 连接池与单独构造函数，
物理隔离，且该路径必须审计。

---

## 四点五、公开接口的租户从哪来（设计时发现的缺口）

`GET /products` 在契约里是 `security: []` —— **公开接口，没有会话**。
那么租户从哪里确定？

查下来契约里**根本没有租户定位机制**：`servers` 只有两个固定 URL，
全文唯一提到 `/s/{code}` 的地方是 `Merchant.domain` 字段的描述文字，不是真实路由。
数据模型有 `merchants.code` 和 `shop_settings.domain` 正是为此准备的，但没接上。

**决定：租户定位是部署配置，不是契约的一部分。**

| 部署形态 | 租户怎么定 |
|---|---|
| 单商家（`docker compose up`，租户数为 1） | 配置项 `KEEL_DEFAULT_MERCHANT`，请求不必携带任何东西 |
| 多商家 | 从 `Host` 头解析：子域名匹配 `merchants.code`，或整个 host 匹配 `shop_settings.domain` |

> **为什么不把租户放进路径或请求头**
>
> 放路径（`/s/{code}/products`）要改每一条公开路由，且把部署形态烙进了 URL——
> 单商家部署也得带一段没意义的前缀。
>
> 放请求头（`X-Merchant-Code`）更糟：**那等于让客户端自己声明它是哪家店**。
> 公开接口没有鉴权，谁都能改这个头。租户必须由服务端从不可伪造的东西推出来，
> 而 `Host` 是被 TLS 证书和 DNS 约束的。
>
> 代价：本地开发多商家时要配 hosts 或用 `shop-a.localhost`。
> 单商家开发完全不受影响——而那是绝大多数人的路径。

`servers` 块相应补一条带变量的条目，把多商家形态写进契约文档（不改任何路径）：

```yaml
servers:
  - url: http://localhost:8080/api/v1
    description: 本地（单商家，租户由 KEEL_DEFAULT_MERCHANT 指定）
  - url: https://{shop}.example.com/api/v1
    description: 多商家，租户由子域名确定
    variables:
      shop:
        default: demo
```

M1 的验收第 3 条（跨租户读取被拒）因此走的是 **Host 解析这条真实路径**，
不是测试里手动塞一个租户 ID。

---

## 五、M1 不做的事

- **不接 dtmrs。** 读接口用不上事务协调器；接进来只会给骨架加一个「必须装 Rust
  工具链」的门槛，而它已在 `examples/dtmrs-embedded` 验证过。M2 做下单链路时再接，
  那时它会被包进一个带**有界信号量**的 `internal/txn` 包——这是并发验证测出来的规矩：
  所有进入 dtmrs 的调用都要过信号量，否则线程数随请求数线性涨，超限直接 `fatal`。
- **不建 UI 工程**（React 后台、uni-app 商城）。
- **不写业务逻辑。** `GET /products` 只做「查出来、返回」，不做搜索、不做排序策略。
- **不接推理引擎。** 语义检索是 M3。
- **不实现全部表的迁移。** 只迁移这条链路用得到的：`merchants` / `shop_settings` /
  `categories` / `products` / `skus`，加上 RLS 所需的 `current_merchant()` 函数。
  其余表在用到它们的里程碑再迁。

---

## 六、CI

四件事：`golangci-lint` → `go test ./...`（带 PostgreSQL service container）
→ `go build` → `./scripts/check-all.sh`。

**一个闭环**：设计债那轮写的 `check_promises.py` 判定 CI 徽章是否虚标，
用的是「`.github/workflows/` 存不存在」。M1 建了 CI，那个徽章就可以诚实地挂回去——
而且是脚本自动认可的，不是谁说了算。

必须进 CI 的测试，按价值排：

1. **跨租户读取被拒**。这是整套多租户设计唯一真正要命的地方，而且它
   **在单租户测试数据下完全看不出来**，必须专门测。
2. **漏设租户上下文则请求失败**。证明失败方向是关闭的。
3. **codegen 漂移检查**。重新生成一遍，有 diff 就失败。没有它，
   「OpenAPI 是唯一真相源」这条规矩名存实亡。

`examples/dtmrs-embedded` 单独一个 workflow，只在该目录变更时触发。
它需要 Rust 工具链与网络，不该拖慢主 CI；但也不能不跑——
它是那条核心架构主张唯一的可执行证据。

---

## 七、验收标准

| # | 标准 | 怎么验 |
|---|---|---|
| 1 | `docker compose up` 一条命令起全栈含种子数据 | `scripts/smoke.sh` 轮询 `/healthz` 直到 200，再断言 `GET /products` 返回非空 |
| 2 | 生成的 TS 客户端调通 `GET /products` | `tsc --noEmit` 通过，且调用脚本打印出种子商品标题 |
| 3 | 租户 A 读不到租户 B 的商品 | 集成测试：两个 Host 各自请求，断言**具体条数**且两边商品 id 集合无交集 |
| 4 | 漏设租户上下文则请求失败 | 集成测试 |
| 5 | 契约改了但没重新生成 → CI 失败 | **故意改一次契约验证** |
| 6 | 五个文档闸门 + `go test ./...` 全绿 | CI |

第 5 条要故意制造一次失败来验证。**没见它红过的闸门不算数**——
这一条在前面几轮里已经抓到过真问题。

---

## 八、风险

| 风险 | 应对 |
|---|---|
| 生成器不支持 3.1 的那 4 处写法 | 第一个任务就验；退路是降级那 4 处或换生成器，都很便宜 |
| `internal/repository/internal/db` 的路径限制理解有误 | 第一个用到它的任务里写一个「故意从 handler import」的编译失败测试 |
| goose 跑不了 RLS / plpgsql（分隔符问题） | 迁移任务里立即验证；goose 有 `-- +goose StatementBegin` 处理多语句 |
| PostgreSQL service container 与本地 docker compose 行为不一致 | CI 与本地用同一个 PG 大版本（16） |
| 种子数据只有一个租户，跨租户测试形同虚设 | 种子数据**必须**包含至少两个商家，且第 3 条测试断言具体条数 |
