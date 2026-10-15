# keel 进度（2026-10-15）

新会话先读这份，再看 CLAUDE.md 的 token 纪律。**这份文件控制在 8KB 以内**：
只写现状、待办、关键决策、常用操作。做完的事一条一行，细节写进 commit 或 docs，
2026-10-15 之前的完整流水在 [docs/归档/PROGRESS-至2026-10-15.md](docs/归档/PROGRESS-至2026-10-15.md)
（部署踩坑、各期实现细节、备份文件名都在那里，用 grep 查，别整份读）。

## 现状

- **版本**：v0.9.0（tag，2026-10-09）。迁移最高号以 `ls db/migrations | tail -1` 为准。
- **定位**：AI 员工（提案 → 审批 / 自动策略 → 执行 → 复盘）是差异化；普通功能停堆（2026-10-02 拍板）。
  用户重心转向销售，功能优先「能演示、能量化」。
- **已有的大块**：交易全链路、营销 / 券 / 运费、搜索（向量 + 相关度预判）、AI 经营 M9–M11（MCP 接入、
  五种提案、事件 webhook、成绩单、自动授权）、渠道层一至三期（Shopify 商品 / 库存 / 订单）、
  AI 调渠道库存分配、微服务三档部署（单体 / 同库分 schema / 物理拆分）、多商家形态、生产部署自助化
  （`make init doctor prod-up`）、请求跟踪号、访问日志运行时开关。
- **客户端**：Flutter 买家端（`flutter_app/`，H5 + 小程序 + 原生）；后台 Vue 3（`web/admin`）。
  uni-app x 已下线（tag `uniapp-final`）。小程序真机输入阻塞未解（mp-flutter 侧），先别提审。

## 待办（按顺序，2026-10-15 用户认可）

1. ~~清掉本机残留的 keelchaos 压测栈~~ → 自动权限拦下，**等用户手动执行**（见「待用户处理」）。
2. ✅ PROGRESS 瘦身（本文件；旧版归档）。
3. **AI 调渠道库存分配设计稿等用户审**：`docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md`，
   重点是复盘口径（少缺货、毛利），这些数字要进对外素材。
4. **按客户门店生成演示店**：客户商品清单 → 一条命令建店 + 模拟订单 → AI 提案 / 复盘有数可讲。
5. **Playwright 关键流程测试进 CI**（后台 + H5 约十条）；`make test-engine` 在 CI 仍无执行者（`ci.yml` 第 40 行）。

后备（AI 方向）：渠道异常单 AI 处理建议、对账 AI 解释差异（库存对账还没比 `quota_qty`）。
不做：拼团、循环满减、拍照搜同款、大规模秒杀（方案在 `docs/大规模秒杀-方案备忘.md`）。
小尾巴：下一版删 `promotion_skus.stock_qty / sold_qty`；dtmrs barrier 表未清理。

## 待用户处理 / 拍板

- 清理 keelchaos 残留（dtmrs 容器在重启循环）：
  `docker compose -p keelchaos down -v`（不行就 `docker ps -a | grep keelchaos` 逐个 `rm -f`，再删 `keelchaos_*` 卷）。
- 真实支付 / 微信登录 / 短信：需资质，暂不做。
- 演示站限流按真实 IP 生效：要改中转机 nginx + Caddy `trusted_proxies`（归档里「A. 等用户拍板」第 2 条）。

## 关键决策

- **计价顺序**：门店价 → 营销活动（按行分摊）→ 优惠券（按活动后金额判门槛）→ 运费 → 包邮券抵运费。
- **迁移号**：并行前先分配号段，起点在 main 最高号之后留余量；goose 不补低号。
- **通知**：状态变化同事务写 outbox；新增改订单 / 退款 / 库存的调用要在
  `internal/service/notification_policy.go` 登记。
- **AI 写操作默认走提案审批**；每个 AI 能力要有可量化结果。
- **对外文案不夸大**：美团 / 饿了么只说接口对齐、未对接。
- 不在会话之间传凭据。

## 常用操作

- 全量测试：自起 `keel-postgres:16`（`--shm-size=1g`），
  `env -u PGUSER -u PGPASSWORD -u PGDATABASE PGHOST=127.0.0.1 PGPORT=<port> make test-db`，完了 `docker rm -f -v`。
  不要与 `./scripts/check-all.sh` 同时跑。
- **演示栈**（https://eshop.zzss.fun，C 档）：只用 `./scripts/demo-up.sh`（`--build/--down/--config/--env`），
  改完跑 `./scripts/verify-demo.sh`（35 项基线，全绿才算完）。**别手写 compose 命令**（会丢叠加文件）。
- 演示站一键重置：`~/.local/share/keel-eshop/bin/reset-demo.sh`；前端发布：`.../bin/publish-frontend.sh [h5|admin|all]`。
- 多商家形态：`make multi-up / multi-verify / multi-down`；后台登录 `make multi-login STAFF=<id>`。
- 外部验证走跳板：`ssh -p 44922 jeff@183.58.3.218 'curl ...'`（本机 curl 走代理）。
- 部署前备份 core 与 inventory 两个库；**`pg_dump -Fc` 不含 ACL**，恢复后按迁移原文补授权，
  别用 `GRANT ... ON ALL TABLES`（会破坏 C 档拆分）。授权基线 353。
- `DROP SCHEMA / TRUNCATE / DROP DATABASE` 只在副本库做。

## 踩过的坑（只留会再踩的）

- 裸 `docker compose up` 会自动读项目根 `.env` —— 生产配置叫 `.env.prod`，别改回来。
- compose 合并 `ports` 是追加，覆盖要写 `!override`。
- 并行 agent 同时跑 check-all 与 test-db 会互相踩；并行上限 3 路，机械活派 Sonnet。
- 主机负载高时搜索日志写入超 200ms 被放弃 → 响应缺 trace_id，不是 bug。
- CI runner 是本机（lenserver），Go 在 `/usr/local/go`（1.26.8）。
