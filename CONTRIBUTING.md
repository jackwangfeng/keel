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
| PostgreSQL | 16+ **且同时装了 pgvector 0.8+ 与 PostGIS 3+** | 两条扩展各有一条迁移点名要它：`00016` 的第一句是 `CREATE EXTENSION IF NOT EXISTS vector`，`00020` 的第一句是 `CREATE EXTENSION IF NOT EXISTS postgis`。缺哪一个都是整条迁移链断在那里，而错误停在 goose 上、不指向真因。**没有任何官方镜像同时带这两样**（pgvector 上游的镜像没有 PostGIS，PostGIS 上游的没有 pgvector），所以仓库自建：`docker/postgres/Dockerfile`，`docker build -t keel-postgres:16 docker/postgres`（本机实测约 60 秒，之后走层缓存）。底座是官方 `postgres:16-bookworm`，两个扩展都从 PGDG 装。实测 PostgreSQL 16.15 / vector 0.8.6 / postgis 3.6.4 / glibc 2.36；PG、vector、glibc 三项与换镜像之前逐项一致 —— **glibc 那一项最要紧**，它决定旧数据卷挂上来之后文本索引还能不能信（见 `docker/postgres/Dockerfile` 文件头）。0.8 是 `hnsw.iterative_scan` 的下限 |
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
> 版本钉在 `scripts/fetch-dtmrs.sh` 里（v0.11.1），**只有那一处** ——
> `examples/dtmrs-embedded` 的 `make deps` 调的也是它。
>
> 如果你不想装 Rust，也可以用独立 TC 进程的部署形态开发，
> 见[总体架构](./docs/电商系统-总体架构.md)的「部署演进」一节。

## 本地数据库

镜像要**自己建一次**：00016 要 pgvector、00020 要 PostGIS，而没有一个官方镜像
同时带这两样（论证在 `compose.yaml` 的 postgres 服务上方）。建过之后是本地标签，
`docker compose` 用的也是它。

```bash
docker build -t keel-postgres:16 docker/postgres   # 本机实测约 60 秒，之后走层缓存
docker run -d --name keel-pg --shm-size=1g -e POSTGRES_PASSWORD=keel -e POSTGRES_USER=keel \
    -e POSTGRES_DB=keel -p 5432:5432 keel-postgres:16
make migrate          # 迁到最新；make migrate-status 看状态，make migrate-down 回滚一格
make test-db          # 跑碰数据库的测试
```

5432 被别的容器占了就换端口，两个目标都认 `PG*` 变量：

```bash
docker run -d --name keel-pg ... -p 5433:5432 keel-postgres:16
PGPORT=5433 make migrate
PGPORT=5433 make test-db
```

`--shm-size=1g` 只影响一条测试的快慢：`internal/repository` 的 HNSW 测试要并行建
一个 3.4 万条向量的索引，pgvector 把整张图放在共享内存里，而 Docker 默认的
`/dev/shm` 只有 64 MB。按旧命令起的库照样能跑，那条测试会退回串行建索引，
慢二十秒左右，日志里有一句说明。

`make test-db` **不往你的 `keel` 库里写东西**。每个碰库的测试包建一个自己的库
（`keel_test_db`、`keel_test_handler`……），在上面从空库迁移、跑完删掉；`PGDATABASE`
（默认 `keel`）只用来建库、删库。所以各包可以并行跑，也不会冲掉你库里的数据。
实现和理由见 `internal/testdb`。测试被 `-timeout` 杀掉时库会留下来，下一次运行
会先删掉再建。

> 已经有一个旧的 `keel-pg`（跑着 `pgvector/pgvector:pg16`）的话，**数据卷可以留着**，
> 只换容器：删掉旧容器，用同一个卷起 `keel-postgres:16`，再 `make migrate`。
> 两个镜像的底座都是 Debian bookworm、glibc 2.36，排序规则对得上；PostGIS 由
> `00020` 那句 `CREATE EXTENSION IF NOT EXISTS postgis` 在迁移时补建 ——
> 官方 postgres 镜像的入口脚本本来就不建任何扩展，这句话正是为挂着旧卷升级上来
> 的库写的。
>
> **唯一的例外**：如果你的卷**曾经**被 `keel-postgres:16` 的第一版（底座是
> bullseye、glibc 2.31，M4 合并后短暂存在过）打开过，PostgreSQL 会报
> `collation version mismatch`。那时在库里跑一次
> `REINDEX DATABASE keel; ALTER DATABASE keel REFRESH COLLATION VERSION;`
> —— 前一句重建在错的排序规则下建出来的文本索引，后一句只是清掉警告，
> **单跑后一句不修任何东西**。

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
代价是生产库上一次不一致的 schema。`make migrate` 先把它编到仓库里的
`bin/goose`（已忽略，不在 PATH 上），已是最新时这一步约 0.15 秒；
测试调的也是 `make migrate`，不另写一份调用方式。

## 迁移怎么写

迁移在线上是对着一个正在接流量的库跑的。本地库几百行、什么写法都是毫秒级，所以下面这几条
在本地**永远测不出来**，只能靠写的时候守住。`scripts/check_migrations.py`（`check-all.sh` 的一步）
机器检查其中第 2、3 条，只管编号 > 00151 的迁移 —— 更早的已经上线，改已应用的迁移不会重跑，
只会让库与文件对不上。

1. **先扩后收。** 一次部署里只做「加」：加列（可空、无默认值或常量默认值）、加表、加索引、
   加触发器维护新列；等代码全部切到新结构、旧代码不再在线上跑了，下一次迁移再「收」（删旧列、
   删旧索引、加 NOT NULL）。滚动发布期间新旧两版代码同时在跑，一步到位的迁移总有一版会炸。
   `ADD COLUMN ... GENERATED ALWAYS AS (...) STORED` 与带易变默认值的 `ADD COLUMN` 会**重写整张表**、
   全程持 ACCESS EXCLUSIVE 锁，大表上等于停机 —— 用普通可空列 + 触发器 + 分批回填代替
   （例子：`00170` / `00171` 的 `orders.receiver_phone`）。
2. **大表建索引用 `CREATE INDEX CONCURRENTLY`。** 普通 `CREATE INDEX` 持 SHARE 锁，建多久就挡多久的写。
   CONCURRENTLY 不挡写，代价是：不能在事务块里跑、失败会留下一条 INVALID 索引
   （`IF NOT EXISTS` 会把它当成已存在跳过，重跑前先 `DROP INDEX CONCURRENTLY`）。
   「大表」的清单在 `scripts/check_migrations.py` 的 `BIG_TABLES`，一张表开始随商家数 × 时间增长就加进去。
3. **CONCURRENTLY 要配 `-- +goose NO TRANSACTION`**（写在文件第一行）。goose 默认把整份迁移包在
   一个事务里 —— `00057` / `00064` 当年就是以此为由没用 CONCURRENTLY，那个理由不成立。
   NO TRANSACTION 的迁移中途失败不会回滚前面已执行的语句，所以每一句都要能重跑
   （`IF NOT EXISTS` / `IF EXISTS`、回填的 WHERE 判「还没填」）。
4. **回填不进 DDL 事务，分批提交。** 一条 `UPDATE` 改全表，就是整张表的行锁攥在一个事务里、
   一次性产生整表的死元组（`00085` 在同一个事务里 ADD COLUMN 后全表 UPDATE，是反例）。
   在 NO TRANSACTION 的迁移里用 DO 块按主键区间每批几千行、每批 `COMMIT`（PostgreSQL 11 起，
   在事务外执行的 DO 块里可以 COMMIT）；要绕开 `touch_updated_at` 这类触发器时在 DO 块里把
   `session_replication_role` 设成 `replica`、结束前复原（迁移角色是超级用户）。
   例子：`00171`。
5. **要拿锁的 DDL 先 `SET LOCAL lock_timeout`。** `ALTER TABLE` 要 ACCESS EXCLUSIVE：它排在一条长查询
   后面等锁的时候，后面进来的**所有**读写都排在它后面 —— 一次加列变成整站卡死。
   设个几秒的 `lock_timeout`，拿不到就失败重来。只对包事务的迁移有效（`SET LOCAL`）；
   NO TRANSACTION 的迁移里 goose 逐条经 `*sql.DB` 执行，会话级 `SET` 落在哪条连接上没有保证。
6. **迁移号按分配的号段取。** 多路并行开发前先分号段（见仓库根目录的 CLAUDE.md），
   不要取「下一个可用编号」。`db/migrations-inventory/` 是库存库自己的目录，改库存表时两边各一份、
   逐字一致（`00173` 是例子）。

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

### 派生数据入库：文本向量与 bigram 关键词串

商品的两份派生数据（`product_text_vectors.embedding` 与 `products.search_text`）
由 `internal/service` 的派生数据入库任务维护，它**要一个真的推理引擎**。

引擎是 infero，**要一块 NVIDIA GPU**，而且它跑在 compose 之外的宿主机进程里
（显存账写在 `scripts/infero-up.sh` 的文件头）：

```bash
./scripts/infero-up.sh                                       # 先起引擎
docker compose -f compose.yaml -f compose.infero.yaml up -d   # 再起栈
```

叠加层会给 `app` 配上 `KEEL_EMBED_ENDPOINT`，进程里的**增量**任务随之启动：
30 秒一轮，商品改了就重算（触发点是 `products.updated_at`，判定只看
`product_understanding.input_hashes` 的那一格，两段判据的论证写在
`db/migrations/00016_semantic_layer.sql` 的文件头第四节与 `internal/service/index.go`）。

**没配 `KEEL_EMBED_ENDPOINT` 时它不启动**，启动日志里有一条 WARN 说明后果
（新品与改过的商品搜不到）。这是刻意的：默认那条 `docker compose up` 里没有引擎。
没有 GPU 的机器默认就是这条路 —— 栈起得来、`/search` 走纯关键词降级链，
只是没有语义召回。infero 2026-09-26 起有了 CPU 后端（`--features cpu`），`cb0ccbd`
（2026-09-29）提速后数值与 GPU 一致、延迟也进了 Keel 的预算（本机 20 核空闲实测：
单条 117 ms 中位数、64 条一批 2.68 s，三条判据全过），**没有 GPU 时现在也可以选
CPU 版跑语义检索**——需要专门编译并留够核数，实测表见
[总体架构](./docs/电商系统-总体架构.md) §1「那个缺口」，想自己验可以照那里的复现命令跑 `make test-engine`。

**全量**走一条单独的命令，补两类增量看不见的东西：存量（00016 刚落地时全库
`search_text` 都是 NULL，而没有任何 `updated_at` 因此前进），以及换模型 / 改拼接模板：

```bash
KEEL_EMBED_ENDPOINT=http://127.0.0.1:8001 go run ./cmd/keel-index            # 全部活跃商家
KEEL_EMBED_ENDPOINT=http://127.0.0.1:8001 go run ./cmd/keel-index -merchant 3
KEEL_EMBED_ENDPOINT=http://127.0.0.1:8001 go run ./cmd/keel-index -force      # 换模型之后，会真的花钱
```

不带 `-force` 的全量同样走判定，所以在已经索引过的库上跑它几乎不花钱。

## 提交前自查

```bash
./scripts/check-all.sh
```

**它需要 Node**（后面几步走 `npx`），而最后一步还需要 `make admin-install` 装过
商家后台的依赖。没有它们的环境里会失败，那是诚实的失败：
契约产物与后台确实没被验证过。

十三步，依次是：

1. 文档链接是否有效（含中文文件名的百分号编码与锚点）
2. README 承诺的文件是否真实存在，以及快速开始里的端口是否真的在 compose 里映射
3. OpenAPI 契约结构是否有效且无悬空引用
4. 架构文档声称的 AI 能力条目数是否与清单一致
5. **每张业务表是否都带 `merchant_id`**（多租户隔离，漏一次就是跨租户泄露）
6. `db/queries/*.sql` 里有没有应用层的租户过滤（那是 RLS 的活；应用层再加一份，
   「RLS 到底有没有生效」就永远测不出来了）；以及新迁移（> 00151）给大表建索引是不是
   `CONCURRENTLY` + `-- +goose NO TRANSACTION`（`check_migrations`，见上文「迁移怎么写」）
7. **契约产物漂移比对** —— 生成到临时目录再和入库产物比，对工作区只读
8. **SQL 产物漂移比对** —— `make generate-sql` 生成到临时目录，与入库的 sqlc 产物比，红了跑 `make generate-sql` 并一起提交
9. **Flutter 契约漂移比对与页面结构** —— `flutter_app/lib/api/schema.g.dart` 是否与契约同步
   （`check_dart_contract`，重生成到临时目录再 diff，红了跑 `make flutter-generate`），
   `lib/pages`、`lib/widgets` 下有没有直接 `import` 契约生成的类型（`check_flutter_pages`，
   不许，页面只能读视图模型），外加生成器自身的单元测试
10. `make schema-check` —— `web/src` 在 `--strict` 下编译得过，且编译范围真的覆盖到每个源文件
11. `make app-type-check` —— `app/src` 下全部 `.uts` 在 `--strict` 下编译得过（**冻结客户端**；
    `check_uts_contract` 另外核对 `app/src/api/schema.uts` 与契约同步），只做存量维护，跟不跟得上契约仍然会红，但新功能不再往这里加）
12. `make admin-type-check` —— 商家后台 `web/admin/src` 下全部 `.ts` 与 `.vue` 在 `--strict`
    下编译得过，编译范围真的覆盖到它们，且类型真的来自入库的契约产物。
    **它要先 `make admin-install`**（`vue-tsc` 才认 `.vue`，npx 拉不到一个能用的组合）；
    没装依赖时它失败而不是跳过——跳过会让「后台的类型检查跑过了」这句话变成假话

13. `make admin-test` —— 商家后台的单元测试（电子围栏的坐标序与 GCJ-02 / BD-09 → WGS-84 换算）。
    `node --test` 直接跑 .ts，不需要 node_modules

第 9、10、11 三条不能合成一条，理由写在 `scripts/check_admin_types.py` 的文件头：
第 9 条的全部价值是「零 node_modules」，把它换成 `vue-tsc` 等于让契约产物的闸门
取决于一棵 UI 框架依赖树。

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
