# keel 进度（2026-10-16）

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

## 待办

2026-10-15 夜那一轮（用户认可的顺序）：
1. ~~清 keelchaos 残留~~ → 自动权限拦下，**等用户手动**（见下）。
2. ✅ PROGRESS 瘦身（`cdc4eec`）。
3. **AI 调渠道库存分配设计稿等用户审**：`docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md`。
4. ✅ 客户演示店 `make prospect-demo`（`7daf36e`，`docs/指南/客户演示店.md`）：示例清单 14 天约 2 分钟，
   AI 员工巡店出 10 条补货提案 + 日报。
5. ✅ 后台关键流程浏览器测试 10 条进 CI（`4fc84ec`，`make admin-e2e`）；顺带修了改价只能填整元（`23e2ff8`）。
   未做：H5（Flutter Web 已有 25 条无头用例 `make flutter-e2e-web`）还没进 CI；`make test-engine` 在 CI 仍无执行者。

2026-10-16 用户给的下一批（AI 方向，都走 提案 → 审批 → 执行 → 复盘；设计稿
`docs/superpowers/specs/2026-10-16-ai-ops-onboarding-restock-coupon-design.md`，**用户还没审**）：
1. ✅ 上新整理（`c9a2e81`）：`catalog_issues` + `propose_product_copy.category_id` + 手册 `上新整理.md`。
   演示：乱清单 24 处问题 → 12 条提案 → 批准后剩 2 处（AI 有意交给人的价格异常与缺类目）。
2. ✅ 补货准确率复盘（`c9a2e81`）：提案带服务端测算快照；复盘加准确率 / 少断天数 / 多卖件数金额；成绩单汇总。
   数字要执行后 7 天才有，演示站（每小时模拟下单 + 每天巡店）跑一周后就能讲。
3. 券的精准发放（带对照组）：`customer_segments` + `propose_coupon.target` + 迁移 00350 `coupon_target_members`
   + 手册 `精准发券.md`。见下一条提交。
顺带修：平台账号审批提案 500 → 403（`c9a2e81`）。

infero（推理引擎 18081）2026-10-16 凌晨没在跑：演示站新品搜不到，需要的话 `scripts/infero-up.sh`。

后备（AI 方向）：渠道异常单 AI 处理建议、对账 AI 解释差异（库存对账还没比 `quota_qty`）。
不做：拼团、循环满减、拍照搜同款、大规模秒杀（方案在 `docs/大规模秒杀-方案备忘.md`）。
小尾巴：下一版删 `promotion_skus.stock_qty / sold_qty`；dtmrs barrier 表未清理。

**演示站 2026-10-16 已部署到 `2d91bc0`**（库 00350；部署前备份 `~/.local/share/keel-eshop/backup-pre-00350-{core,inv}-202610160341.dump`，
迁移先在备份副本上试跑过）。基线 35/35。模拟器加了 150 个结构化买家（`simulate.py` 的 MORE，原文件备份 `.bak-segments`），
一周后演示站上会有沉睡 / 只买一次人群；补货复盘的数字执行满 7 天后出现。
定时任务：每周一 09:00 `cron-agent-coupon.sh`（精准发券手册，日志 `logs/agent-coupon.log`；改前 crontab 备份 `crontab.bak-20261016`）。
**AI 提的发券提案要有人在后台批准**，批准后 7 天才有对照组复盘。

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
