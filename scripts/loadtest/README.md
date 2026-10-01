# 性能压测工具与数据

一轮压测的方法、数字与结论在 [性能压测-2026-10](../../docs/性能压测-2026-10.md)。这里只讲怎么复现。

| 文件 | 干什么 |
|---|---|
| `seed/main.go` | 造数据：10 万商品（每件 1–3 个 SKU）、40 家带围栏的门店（杭州城区）、库存行、5000 买家、5 万历史订单；写出 fixture |
| `../../cmd/keel-loadtest/` | 压测器（只用标准库）：闭环并发、按场景生成请求，输出 RPS、p50/p95/p99、按状态码与 problem type 的错误分布，可选采样 `docker stats` 与 `pg_stat_activity` |
| `vectors/main.go` | 给种子商品补文本向量（接语义检索压搜索时用）：按「形容词 + 叶子类目名」去重约 1500 种，调引擎算完共用，**近似**（品牌、型号、副标题不进向量），见报告第十一节 |
| `run-all.sh` | 按文档里的场景表跑一轮（可只跑某几个阶段；`SEARCH_C` 改搜索并发档，`SAMPLE_EXTRA` 多采样容器） |
| `backlog.sh` | 看后台积压：jobs 队列、过期未关的待支付单、两个协调器里没走完的全局事务、连接数 |

## 1. 起一套一次性的私有栈

拆分形态（core + 库存两进程两库），项目名与卷名要和任何在用的栈分开：

```bash
export COMPOSE_PROJECT_NAME=keellt KEEL_HTTP_PORT=38180 KEEL_CONSOLE_PORT=38181
export KEEL_INTERNAL_SECRET=$(openssl rand -hex 32) KEEL_AUTH_SECRET=$(openssl rand -hex 32)
export KEEL_SEARCH_RATE_PER_SEC=0        # 关掉 /search 的按 IP 限流，否则压的是限流器
DC=(docker compose -f compose.yaml -f compose.split.yaml)
"${DC[@]}" up -d --build
```

语义检索不要接（不设 `KEEL_EMBED_ENDPOINT`），搜索只压关键词路径。要压带语义检索的搜索：再叠 `compose.infero-cpu.yaml`，造数后跑
`go run ./scripts/loadtest/vectors -core-dsn ... -embed http://<inference 容器 IP>:8081` 补向量（报告第十一节）。

## 2. 造数据

种子以管理员账号直连两个库（容器 IP 在宿主机上可达）：

```bash
ip() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"; }
go run ./scripts/loadtest/seed \
  -core-dsn "postgres://keel:keel@$(ip keellt-postgres-1):5432/keel?sslmode=disable" \
  -inv-dsn  "postgres://keel:keel@$(ip keellt-postgres-inventory-1):5432/keel_inventory?sslmode=disable" \
  -out /tmp/lt/fixture.json          # 约 75 秒；规模参数见 -h
```

- 单体形态：`-inv-dsn` 与 `-core-dsn` 相同。
- 买家口令统一是 `keel-demo-2026`。没接短信服务的栈上验证码登录是 501、契约里没有别的注册入口，
  所以买家行直接写库，压测器再按契约 `POST /auth/login` 换令牌（缓存在 `-tokens` 文件里）。
- 库存行写进库存库，core 的有货标记（`product_store_stock`）按同一份数量算好写进去；
  每家店每件商品的第一个 SKU（fixture 的 `hot_sku`）在每家店都有 1000 万件，留给热点下单。
- 造过一次再跑会拒绝（`-force` 强行再造一份）。

## 3. 后台令牌

```bash
"${DC[@]}" logs app | grep bootstrap_token          # 一次性引导 token
curl --noproxy '*' -s -X POST http://127.0.0.1:38180/api/v1/admin/auth/bootstrap \
  -H 'Content-Type: application/json' -d '{"token":"<上面那串>","email":"lt@example.com"}'
export KEEL_LT_ADMIN_TOKEN=<响应里的 token>
```

## 4. 压

```bash
go build -o /tmp/lt/keel-loadtest ./cmd/keel-loadtest
/tmp/lt/keel-loadtest -list                                  # 场景清单
/tmp/lt/keel-loadtest -fixture /tmp/lt/fixture.json -scenario list-default -c 8,16,32 -d 45s \
  -sample keellt-app-1,keellt-inventory-1,keellt-postgres-1,keellt-postgres-inventory-1 \
  -pg keellt-postgres-1:keel,keellt-postgres-inventory-1:keel_inventory

# 或者整轮（read search cart order hot flash admin mix，可只给其中几个）
PROJECT=keellt FIXTURE=/tmp/lt/fixture.json OUT=/tmp/lt/results scripts/loadtest/run-all.sh
```

- `-c 8,16,32` 是逐级加压，每级 `-d`，级间停 `-pause`；每级先预热 `-warmup` 再统计。
- 压测器不读 `HTTP_PROXY`（本机常设代理，压测流量不能绕出去）。
- `5xx%` 只算 5xx 与网络错误；409 / 422 这类业务拒绝单独列在 problem 分布里（热点、秒杀会有大量 409）。
- `-json` 把每一级的结果追加成一行 JSON，便于对比。

## 5. 收尾

```bash
"${DC[@]}" down -v && docker volume ls | grep keellt     # 应为空
```
