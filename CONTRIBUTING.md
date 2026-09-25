# 贡献指南

欢迎贡献。在动手之前，请先读完这一页——尤其是「两条硬规矩」。

## 两条硬规矩

### 一、handler 里不准出现 SQL 和事务

业务逻辑在 service，数据访问在 repository。handler 只做三件事：
参数校验、鉴权、响应封装。

这不是风格偏好。「单机形态平滑演进到微服务」这个架构主张能否成立，
完全取决于这条纪律——handler 里一旦出现 SQL，将来拆服务就得重写业务代码，
而整个项目的卖点就是「业务代码零改动」。

**handler 里出现 SQL 或事务的 PR 会被直接退回。**

### 二、OpenAPI 是唯一真相源

顺序永远是：先改契约 → 生成代码 → 实现。

反过来做（先写 handler 再补契约）会让前后端不一致悄悄溜进主干，
而契约驱动的全部价值就在于接口变更会让构建失败，而不是让线上出错。

同理，**数据库表结构的唯一真相源是 [`docs/电商系统-数据模型设计.md`](./docs/电商系统-数据模型设计.md)**。
其余文档不得出现 `CREATE TABLE`；需要讲结构时引用该文档的章节。
唯一例外是 dtmrs 的 `barrier` 表，它是第三方组件的产物。

## 开发环境

| 依赖 | 版本 | 说明 |
|---|---|---|
| Go | 1.26+ | 后端。下限由 `tools/` 里钉的 sqlc 与 goose 传染而来——两者都声明 `go 1.26` |
| Rust | **1.88+** | **必需**（M2 起）：编译 dtmrs 的 C ABI 动态库，主模块经 cgo 嵌入它。`make dtmrs-deps` 会取回源码并编出 `third_party/dtmrs/lib/libdtmrs.so`；`make build` 与 `make test-db` 缺了它会自动建一次。**下限不是 dtmrs 声明的 1.82** —— 那是它的 `rust-version`，而卡住构建的是它 `Cargo.lock` 锁定的依赖树：实测 1.85.1 直接报 `home@0.5.12 requires rustc 1.88`。Debian trixie 的 apt 版正好是 1.85.1，不够，用 rustup。已验证于 1.94.0（本机）与镜像里的 `rust:1.90-slim-trixie` |
| PostgreSQL | 16+ **且装了 pgvector 0.8+** | M3 起 `db/migrations/00016` 的第一条语句就是 `CREATE EXTENSION IF NOT EXISTS vector`，官方 `postgres:16` 里实测报 `extension "vector" is not available`，整条迁移链在那里断掉。compose 与 CI 都换成了 `pgvector/pgvector:pg16`（本机实测 vector 0.8.6 / PostgreSQL 16.15），本地起库请用同一个镜像。0.8 是 `hnsw.iterative_scan` 的下限 |
| Node | 22.18+ | `make generate-ts` 生成 TS 侧契约类型、`make schema-check` 编译 `web/src`、`make sdk-smoke` 直接跑 `.mts`（靠 Node 自带的类型剥离，不经构建步骤——这是下限的来源）。已验证于 v24.10.0 |
| Docker | 任意近期版本 | `docker compose up` 起全栈 |

> **为什么需要 Rust 工具链**：事务协调器 [dtmrs](https://github.com/jackwangfeng/dtmrs)
> 是 Rust 实现，主模块通过 cgo 以嵌入式模式运行它（`internal/dtm`）。
> 代价是确定的，别想绕：`CGO_ENABLED=0` 静态编译没了，最终镜像也不再是
> `scratch`（动态链接需要 glibc 与动态链接器，M2 换成了 `debian:trixie-slim`）。
>
> **M1 时这一条写的是「将来才需要」，M2 把它变成了现在。** 没装 Rust 时的症状是
> 链接器的一句 `cannot find -ldtmrs`；`make build` / `make test-db` 会先替你
> 跑一次 `make dtmrs-deps`（本机实测约 1 分钟），而那一步缺 cargo 会打印
> 一句指名道姓的中文提示。
>
> 版本钉在 `scripts/fetch-dtmrs.sh` 里（v0.11.0），**只有那一处** ——
> `examples/dtmrs-embedded` 的 `make deps` 调的也是它。
>
> 如果你不想装 Rust，也可以用独立 TC 进程的部署形态开发，
> 见[总体架构](./docs/电商系统-总体架构.md)的「部署演进」一节。

## 本地数据库

```bash
# 镜像是 pgvector/pgvector:pg16，不是官方 postgres:16 —— 官方镜像里没有 vector
# 扩展，00016 的第一条语句就会失败，而错误停在 goose 上、不指向真因。
docker run -d --name keel-pg -e POSTGRES_PASSWORD=keel -e POSTGRES_USER=keel \
    -e POSTGRES_DB=keel -p 5432:5432 pgvector/pgvector:pg16
make migrate          # 迁到最新；make migrate-status 看状态，make migrate-down 回滚一格
make test-db          # 跑碰数据库的测试
```

5432 被别的容器占了就换端口，两个目标都认 `PG*` 变量：

```bash
docker run -d --name keel-pg ... -p 5433:5432 pgvector/pgvector:pg16
PGPORT=5433 make migrate
PGPORT=5433 make test-db
```

### 两个角色，别用错

| 角色 | 谁用 | 连接串 |
|---|---|---|
| `keel`（超级用户） | **只有迁移**。建表、建角色、授权要属主权限 | `db.AdminDSN()`，env `KEEL_ADMIN_USER` / `KEEL_ADMIN_PASSWORD` |
| `keel_app`（`NOSUPERUSER NOBYPASSRLS`） | 应用与**所有测试** | `db.DSN()`，env `PGUSER` / `PGPASSWORD` |

由 `db/migrations/00003_app_role.sql` 建出来。**口令只在角色首次创建时设置**：
有 `KEEL_APP_PASSWORD` 就用它，没有就用开发默认值 `keel_app`。
角色已存在时迁移一个字都不碰口令——角色是集群级对象而 `GRANT` 是单库的，
同集群每多一个库就要再跑一遍这份迁移，若写成无条件设置，
给第二个库跑 `goose up` 时忘了带环境变量就会把生产口令静默重置回版本库里的默认值。

代价是**轮换口令要走单独的运维操作**（`ALTER ROLE keel_app PASSWORD ...`），
迁移不负责这件事。首次 provisioning 请务必带上 `KEEL_APP_PASSWORD`。

**为什么必须分开**：超级用户和带 `BYPASSRLS` 的角色无条件绕过行级安全，
`FORCE` 也拦不住。用 `keel` 连上来，`00002` 里的租户隔离就是一张废纸——
既读得到别家租户的数据，也不会在忘记设 `app.merchant_id` 时报错，
而且**所有「跨租户读不到数据」的测试都会假绿**。
所以 `internal/db.Connect()` 在建连接时就会查 `rolsuper` / `rolbypassrls`，
发现能绕过就拒绝返回连接。拿连接请走它，别自己 `pgx.Connect`。

### 表属主是 `keel`，不是 `keel_app`

`keel_app` 只是被授权者。这意味着它走普通 RLS 路径，`ENABLE` 就足以约束它；
`00002` 里的 `FORCE` 是给「属主自己连上来」那种部署形态兜底的。
**如果以后有人 `ALTER TABLE ... OWNER TO keel_app`，安全边界就只剩 `FORCE` 一道**，
那时它从冗余保险变成唯一防线——别当多余的删掉。

新增的表由 `ALTER DEFAULT PRIVILEGES` 自动授权给 `keel_app`，不用逐张补 `GRANT`。

### 跑数据库测试请走 `make test-db`

它把 `-count=1` 钉死了。这些测试真正依赖的输入是**数据库状态**，
而那在 Go 的视野之外：源码和环境变量没变时 `go test` 会直接回放上次的成功结果。
实测过——把 RLS 策略整个 `DROP` 掉，裸 `go test` 照样报 `ok (cached)`。
一个「本地跑两遍就永远绿」的测试，恰恰只在它该报警的时候失灵。

goose 不装全局二进制，它和 sqlc、oapi-codegen 一样钉在 `tools/go.mod`，
只经 `make migrate` 调用——本地与 CI 装到不同版本的迁移工具，
代价是生产库上一次不一致的 schema。

## 起全栈：`docker compose up`

```bash
docker compose up -d --build
./scripts/smoke.sh          # 退出码 0 表示链路通
```

四个服务依次跑：`postgres` → `migrate`（goose，跑完退出）→ `seed`（psql 加载种子，
跑完退出）→ `app`。每一段都等上一段**成功**，所以 `up` 失败时看最后一个没起来的
服务的日志就够了，不必猜是谁先坏的。

**默认是单商家形态**：种子只播一家店（`db/seed/single.sql` 里的 `demo`），
应用配 `KEEL_DEFAULT_MERCHANT=demo`，Host 完全不参与解析——
`curl localhost:8080/api/v1/products` 直接有商品。这就是 README 承诺给小商家的那个形态。

**别把测试夹具 `db/seed/dev.sql` 加载进这个形态**：它播 6 家商家（其中 4 家活跃），
而 `tenant.Preflight` 在「配了默认商家 + 库里多家活跃商家」时会拒绝启动——
实测报的是 `启动自检未通过，拒绝启动: 配置了默认商家 "demo"……但库里有 4 家活跃商家`。
要多商家形态请走叠加层，它换的是种子与租户来源两件事：

```bash
docker compose -f compose.yaml -f compose.multi.yaml up -d --build
KEEL_SMOKE_HOST=shop-a.example.com ./scripts/smoke.sh
```

叠加层用的是**自己的数据卷**（`keel_pgdata_multi`），两种形态可以来回切，
不需要 `docker compose down -v`。共用一个卷的话，`single → multi` 这个方向会静默出错：
两份种子都是增量式的，dev.sql 叠在 `demo` 上会变成 7 家商家，而多商家模式的 Preflight
不检查「有几家」，于是应用照常起、冒烟照常绿，只是库里多了一家本不该存在的店。

**判据是 `./scripts/smoke.sh` 的退出码，不是 `docker compose up -d` 的**：
后者只等到容器启动，应用随后因为启动自检没过而退出，它照样返回 0。

宿主机的 8080 被占就换端口，compose 与 smoke 读的是同一个变量：

```bash
KEEL_HTTP_PORT=18080 docker compose up -d
KEEL_HTTP_PORT=18080 ./scripts/smoke.sh
```

数据库刻意**不**往宿主机映射端口：开发机上 5432 被别的容器占着是常事，
为一个谁都不必用到的端口让 `docker compose up` 当场失败，代价和收益不成比例。
要连进去看：`docker compose exec postgres psql -U keel keel`。

## 提交前自查

```bash
./scripts/check-all.sh
```

**它需要 Node**（最后两步走 `npx`）。没有 Node 的环境里会失败，那是诚实的失败：
契约产物确实没被验证过。

八步，依次是：

1. 文档链接是否有效（含中文文件名的百分号编码与锚点）
2. README 承诺的文件是否真实存在，以及快速开始里的端口是否真的在 compose 里映射
3. OpenAPI 契约结构是否有效且无悬空引用
4. 架构文档声称的 AI 能力条目数是否与清单一致
5. **每张业务表是否都带 `merchant_id`**（多租户隔离，漏一次就是跨租户泄露）
6. `db/queries/*.sql` 里有没有应用层的租户过滤（那是 RLS 的活；应用层再加一份，
   「RLS 到底有没有生效」就永远测不出来了）
7. **契约产物漂移比对** —— 生成到临时目录再和入库产物比，对工作区只读
8. `make schema-check` —— `web/src` 在 `--strict` 下编译得过，且编译范围真的覆盖到每个源文件

改动文档或契约的 PR 必须先让它通过。

> 注意它**不**碰数据库。RLS 策略、跨租户复合外键这些只有真库能验的东西在
> `make test-db` 里（需要一个 PostgreSQL 16）。两条命令合起来才是完整的自查。

## 提交信息

用中文，遵循 Conventional Commits 的类型前缀：

```
feat: 订单项带上优惠分摊
fix: 退款数量超过剩余时拒绝
docs: 说清 saga 补偿的顺序
```

标题行说**做了什么**，正文说**为什么**——为什么这一条尤其重要：
这个仓库里很多改动的价值不在代码本身，而在它替换掉了哪一条容易出错的
人工纪律。那种理由写不进一行标题，但半年后回来看 `git log` 时，
它才是你真正需要的那句话。

> 早先这里写的是「标题行用英文」，而实际提交已经是中文居多。
> 统一成中文，判据是：设计文档、评审记录、代码注释全是中文，
> 标题行单独用英文会让「为什么」在标题和正文之间断一次。

## 代码之外

- 设计文档用中文，代码与 Issue 用英文（见[总体架构](./docs/电商系统-总体架构.md)的 ADR #9）
- 数据库变更走迁移工具，禁止手改
- 核心逻辑必须有测试：库存扣减、优惠计算、订单状态机
