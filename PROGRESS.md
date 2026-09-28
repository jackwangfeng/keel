# keel 进度（2026-09-27 晚更新）

新会话先读这份，再看 CLAUDE.md 的 token 纪律。

## 当前状态

- main：`861c51c`（已 push，2026-09-28；含上传缩略图 ?w=；含商品列表有货在前（00087 product_store_stock，每分钟全量刷新）；含商品列表 in_stock 与无货卡片、选点围栏配色；含单字搜索 00086、0 元单自动入账、门店坐标必填 + 地图选点 + 须在围栏内、无货 SKU 排后、围栏编辑器两态、大区停用生效）；演示站 API `861c51c`（库 00087，部署前备份 backup-pre-00087-*.dump）、后台 `33b23b5`、H5 `7200942`（华南大区当前是停用状态，gaoerfu 因此不在买家端）（gaoerfu 坐标已设为围栏中心，默认门店仍未定位；部署前备份 backup-pre-storeloc-*.dump；库 00086，部署前备份 `backup-pre-00086-202609271948.dump`；公网搜「杯」「咖」「茶」关键词路均命中），23 个种子商品有图（db/seed/images，CC0）。收尾分支已合并（`8a826db`），worktree 与分支已删。
- 演示栈（2026-09-27 晚起 **C 档**：core + inventory 两个进程、两个库，40c23da）：环境与 compose 文件清单统一在 `~/.local/share/keel-eshop/demo-env.sh`（`source` 后用 `"${DC[@]}"`），叠加层 `compose.demo-split.yaml` 把卷名换回 keeldemo_*（原地迁移，迁移前备份 `backup-pre-split-202609271854.dump`）；内网密钥 `~/.config/keel/demo-internal-secret`；keeldemo.service 已改为 source demo-env.sh（含登录锁定豁免，重启不再丢）；reset-demo.sh 已适配 C 档（两个库一起重建 + split-data）。**库存在库存库 keeldemo-postgres-inventory-1 / keel_inventory 里**，补库存要改那边。外部验证通过。
- 已发布：**v0.2.0**（2026-09-27，tag 指向 d530fd7，库落在 00065）；ghcr 四个镜像 keel / keel-migrate / keel-postgres / keel-console 均可匿名拉取。iPhone 15 真机验证通过；Android 真机、小程序真机、tab 角标、长标题两行省略未验证。

## Flutter 买家端（flutter_app/，2026-09-28 起主要维护的客户端）

- **对外展示（eshop.zzss.fun 的 `/`）2026-09-28 起是 Flutter Web**：`~/.local/share/keel-eshop/bin/publish-frontend.sh h5`（本机构建，SDK `~/development/flutter` 3.47.5；PATH 上的是 flutter_ohos）。uni-app x 版留作 `h5-uniapp`（回滚用），上一份产物备份在 `/srv/keel-eshop/h5-uniapp`。
  **国内打得开的关键**：Flutter Web 默认从 gstatic 取 CanvasKit 与补字字体，屏蔽后整页空白（实测）。构建加 `--no-web-resources-cdn`，`flutter_bootstrap.js` 注入 `fontFallbackBaseUrl: "/gfonts/"`，引擎补字清单的 725 个字体镜像在 `~/.local/share/keel-eshop/gfonts`（24M），发布时拷进产物。升级 Flutter 后按 `bin/cache/flutter_web_sdk/lib/_engine/engine/font_fallback_data.dart` 补镜像。屏蔽 google/gstatic 后从公网打开首页验证通过（请求全在本站）。首屏要下 main.dart.js 约 3M + canvaskit.wasm 约 7M（Caddy gzip）。

- **状态**：对齐 uni-app x 全部页面（第一阶段购物主链路 + 第二阶段券 / 消息 / 资料 / 服务地址 / 售后），main `fce951c` 起。uni-app x（`app/`）冻结，只修 bug。
- **验收**：`make flutter-analyze flutter-test`（107 条单测）· `KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-e2e-web`（Web 无头 25 条，带前置状态的 5 条读 `KEEL_E2E_*`，没设跳过）· `make flutter-build`（web / apk / ios）· `make flutter-build-mp`（mp-flutter main `4273715`）· `make flutter-ios-install KEEL_IOS_TEAM=2Q89DQSSH6`。
- **小程序**：开发者工具与 mp-flutter 会话共用，用前打招呼；`flutter_app/tool/mp_walk.js` 冒烟（复用 mp-flutter 的 drive.js）。报给 mp-flutter 的：安全区 / 胶囊（viewPadding 恒 0）、冷启动首页滚动位置、webgl2 日志 —— 在修。凭证上传（chooseMedia → uploadFile）、定位（getLocation wgs84）、image_picker / geolocator 的 Web 注册在小程序里还没实测。
- **样式**：2026-09-28 逐页（首页、搜索、详情、购物车、结算、订单列表 / 详情、我的、券两页、消息、地址列表 / 编辑、资料、服务地址、售后列表 / 详情 / 申请、登录）与 uni-app x 的 H5 同尺寸截图对照并对齐（`app/dist/build/h5` 与 `flutter_app/build/web` 各起一个带 /api 反代的静态服务，playwright 用本机 Chrome 截图，e2e 买家的令牌直接写进两边的 localStorage）。Web 上字体不同（Roboto vs 苹方），真机上都是系统字体。
- **小程序**：安全区（mp-flutter main 4273715）、冷启动在顶部、启动无 error、顶栏右侧按钮让出胶囊（`MpWechat.menuButtonRect`）已在开发者工具验过；凭证上传与定位也在开发者工具实测过（mock chooseMedia 返回真文件、wx.uploadFile 真传，售后单带上凭证；getLocation 成功按坐标解析）。实测抓到的 `Random.secure()` 不可用（K4）与 cullRect / Null check 控制台报错（K5）mp-flutter 已在 bc76bf5 修好并复验通过；App 侧的随机数兜底保留（mp-flutter 取不到种子时仍不提供 crypto）。列表图用 `NetImage` 按显示尺寸解码（cacheWidth）。
- **小程序性能（2026-09-28 iPhone 15 / 安卓真机调试实测）**：列表图向服务端要缩略图（`?w=` 四档）；小程序解码像素比封顶 2x（480 档，`12e8962`）；Web / 小程序上缩略图不传 cacheWidth（Web 引擎带 cacheWidth 每张解两遍，`79f47c0`）；mp-flutter perf-native 分支改原生解码后 decode-slow 归零，稳态滑动 iOS 43–60 / 安卓 45–62 fps。剩下首屏 3 次长帧（iOS 约 700–820ms、安卓约 300–360ms）全是回退字体分片到达 → 注册 → fontChange 全量重排版，mp-flutter 在做常用字合一字体（首帧前注册）+ 合并 fontChange，并查安卓 font-fetch 正好 1s。测量版做法：pubspec 临时切 perf 分支（不提交）、`dart run mp_flutter … --perf-hud --dart-chunk-kb 900`（真机调试强制 ES5，分片要小）、真机调试控制台右键 Save as 导出日志（开发者工具日志与端口都拿不到真机 console）。待 mp-flutter 合回 main 后升级并加 `--no-licenses`。
- **踩过的坑**：`go('/login')` 再 `go` 回来会在转场中重建外壳（Duplicate GlobalKey）→ 登录页一律 push / pop；主题按钮最小宽度无穷大放进 Row 布局失败 → 页面测试必须用 `keelTheme()`；Web e2e 同一输入框第二次 `enterText` 收不到；`timeout(onTimeout: () => null)` 遇到运行时不可空的 Future 会因协变当场抛错（坐标被吞）；本机真实位置会被围栏解析到别的店 → e2e 用 `KEEL_LOCATE=off`；Gradle 不读 http_proxy → android 仓库走阿里云镜像。

## 里程碑

| 阶段 | 状态 |
|---|---|
| M1–M4 | ✅ v0.1.0 |
| 交易链路补齐（购物车、地址、/me、取消、发货、确认收货与自动确认、售后退款与寄回物流、买家上传、后台订单页与售后页、平台幂等、员工重签登录 token） | ✅ |
| M5 搜索质量可量化 | 大半：业务重排（库存因子）、search_logs、/search/events 回传、`make search-metrics` ✅；**cross-encoder 精排**（等 infero `/v1/rerank`）、**离线评测集** 待做 |
| M6 能开店 | 站内通知（00053）✅、运费模板与包邮券（00055–00056）✅；真实支付 / 微信登录 / 短信 **需资质，用户明确暂不做** |
| M7 能做生意 | 批量导入与 AI 类目推荐（00054）✅、经营报表看板（00057）✅、营销活动（满减满折 / 限时特价 / 秒杀 / 新人礼，00058）✅；拼团、循环满减未做 |
| **M9–M11 AI 经营（AI 员工）** | 规划与 M9 设计已定稿（2026-09-28）：`docs/AI经营-规划.md`、`docs/AI经营-M9设计.md`。外部 harness（Claude Code 等）经 MCP 接入，写操作默认走提案审批；迁移号段 M9 = 00090–00099。任务 0（README 改诚实）✅、任务 1（AI 员工与接入密钥：00090、/admin/agents、/agent/whoami、kagt_ 密钥）✅。任务 2（/api/v1/mcp，10 个读工具，00093 审计，限流）✅。任务 3（restock_plan：日均分母去掉断货天，库存服务新增 StockoutDays 本地 + HTTP）✅。**下一步：任务 4（提案，00091）→ 5（简报，00092）** |
| M8 拍照搜同款 | 未开始；顺延到 AI 经营之后 |
| 远期 | 对话导购、同款归并、generate 类能力、Text-to-SQL、MCP、销量预测——用户认为偏水，排到远期 |

## 对外展示环境 https://eshop.zzss.fun（2026-09-27 上线，纯演示环境）

用户定的原则：全站公开，**无 Basic 认证**；`/admin/` 全功能、**免登录**（访客直接以 demo 商家管理员身份进入）；**不做**限流、WAF、只读账号；数据被搞乱就一键重置。

- **一键重置（约 10 秒）**：`~/.local/share/keel-eshop/bin/reset-demo.sh`——删库重建 + 迁移 + 单商家种子 + 清事务协调器与上传文件 + 重建演示管理员会话 + e2e 操作员 + 可反复领的包邮券。不重建镜像。
- 路径分流（本机 Caddy 网关）：`/` 买家端 H5 · `/admin/` 商家后台 · `/api/*`、`/healthz`、`/version` → 演示栈 API · `/robots.txt` 全站 Disallow · 所有响应带 `X-Robots-Tag: noindex, nofollow, noarchive`，后台页另有 robots meta。
- 链路：DNS A 记录 eshop → 180.184.132.129（火山 RecordID 50548154）→ 中转机 nginx（`*.zzss.fun` 泛证书）→ frp **remotePort 10024** → 本机 frpc → **本机 Caddy 127.0.0.1:18180** → 演示栈 API 127.0.0.1:18099。
- **后台免登录的做法**（只在部署层，没改产品代码）：
  - 演示员工 `demo-admin@keel.invalid`（demo 商家，role 1 管理员）。
  - `/srv/keel-eshop/admin/index.html` 的 `<head>` 注入了 `/srv/keel-eshop/demo/inject.html` 的脚本：sessionStorage 里没有有效会话时，同步取 `/admin/demo-session.json` 写进 `keel.admin.session`。**重新构建后台后要重新注入**（见下）。
  - `keel-eshop-demo-session.timer` 每分钟跑 `~/.local/share/keel-eshop/bin/demo-session.sh`：确保演示管理员在岗且是管理员（被访客停用 / 降级会自愈）；会话失效（过期或被「退出」作废）就重签，写 `/srv/keel-eshop/demo/demo-session.json`。
- 本机 systemd（均已 enable，开机自启）：
  - `keel-eshop-gateway.service`：Caddy，**以专用系统用户 keel-eshop 运行**，`ProtectHome=true`、`ProtectSystem=strict`、只读 `/srv/keel-eshop`、无任何 capability。配置 `/srv/keel-eshop/Caddyfile`，静态产物 `/srv/keel-eshop/{h5,admin}`，属主 jeffwang、属组 keel-eshop。
  - `keeldemo.service`：开机 `docker compose -f compose.yaml -f compose.infero.yaml up -d --no-build`（演示栈容器本身没有 restart 策略）。
  - `keel-eshop-demo-session.timer` / `.service`：见上。
- **隔离（用户要求：跑服务的身份读不到 ~/.env 与 ~/.ssh）**：
  - 网关：专用用户 + ProtectHome，已验证 `sudo -u keel-eshop` 读不到 `~/.env`、进不了 `~/.ssh`，网关进程命名空间里 `/home` 为空；路径穿越请求只回落到首页。
  - 演示栈容器：只挂具名卷（pgdata / dtmdata / uploads），不挂任何宿主机目录，应用在容器内以 65532 运行；上传目录是独立具名卷 `keeldemo_uploads`。
  - `~/.env` 权限由 664 收紧为 600；家目录本来就是 750。
  - 仍以 jeffwang 身份运行的只有启动器（`keeldemo.service` 调 docker compose）和维护脚本（`demo-session.sh`、`reset-demo.sh`），它们不对外。
- frpc：`/etc/frp/frpc.toml` 末尾追加 `[[proxies]] name="eshop" localPort=18180 remotePort=10024`（改前备份 `/etc/frp/frpc.toml.bak.*`）。
- 中转机 vhost `/etc/nginx/conf.d/eshop.zzss.fun.conf` 是**手写、无 auth** 的，**不要再跑 `publish_site.sh eshop`**（会强制加回 htpasswd）。备份在中转机 `/root/eshop.zzss.fun.conf.bak.*`。
- **更新前端**（前端改动不会自动上线）：
  - H5：`make app-build-h5 && rm -rf /srv/keel-eshop/h5 && cp -r app/dist/build/h5 /srv/keel-eshop/h5 && chmod -R g+rX /srv/keel-eshop/h5`
  - **一条命令**：`~/.local/share/keel-eshop/bin/publish-frontend.sh [h5|admin|all]`（构建到 .new 再换目录，后台自动重新注入并断言注入成功）。下面是它展开后的手工步骤：
  - 后台：`cd web/admin && npx vite build --base /admin/ --outDir /srv/keel-eshop/admin --emptyOutDir`，**然后重新注入**：`python3 -c "p='/srv/keel-eshop/admin/index.html';s=open(p).read();i=open('/srv/keel-eshop/demo/inject.html').read();open(p,'w').write(s if 'keel.admin.session' in s else s.replace('<head>','<head>\n'+i,1))"`，再 `chmod -R g+rX /srv/keel-eshop/admin`
  - API 随演示栈部署更新，不用动网关。
- 验证不要用本机 curl（本机走 127.0.0.1:8890 代理）：`ssh -p 44922 jeff@183.58.3.218 'curl -s -o /dev/null -w "%{http_code}" https://eshop.zzss.fun/'`。
- 踩过的坑：Caddy 站点地址写成 `http://127.0.0.1:18180` 会按 Host 匹配，中转机转发来的 `Host: eshop.zzss.fun` 匹配不上 → 空 200。改成 `http://:18180` + `bind 127.0.0.1`。
- 数据：2026-09-27 已用重置脚本重建，全部是假数据。**你原来的平台管理员账号随旧库清掉了**；需要平台级权限时，从 `docker logs keeldemo-app-1 | grep bootstrap_token` 取引导 token。
- 已知可接受的风险：演示买家 13800000000 / keel-demo-2026 所有访客共用；搞乱了就跑重置脚本。

## 演示栈上为 e2e 准备的数据

- 包邮券模板「包邮券（e2e 可反复领）」（重置后重建，当前 id 3）：全场、无门槛、运费全免、每人限领 100。promotion 用例每轮领一张，约 100 轮后要调高上限或新建。
- 员工 `e2e-operator@keel.invalid`（demo 商家操作员，**id 会随重置变化，按邮箱找**）：只给本机代做脚本用，平时没有有效 token。
- 库存：2026-09-26 把 available_qty < 20 的补到 100（inventory_logs biz_type 5，biz_id demo-restock-2026-09-26）。e2e 会持续消耗，定期再补。

## 关键决策

- **计价顺序**：门店最终价 → 营销活动优惠（按行分摊）→ 优惠券（按活动后金额判门槛）→ 运费（按优惠后应付商品金额判满额包邮）→ 包邮券抵运费。
- **派多路 agent 前先分配迁移号段写进 prompt，起点放在 main 当前最高号之后并留余量**（CLAUDE.md 约定）；禁止 agent 自取下一个可用号。
- **迁移编号必须单调递增**：演示库已迁到高号时，goose 不许补低号（不开 allow-missing）。并行 agent 预分的号段若低于 main 上最高号，合并时统一改号，并同步注释、tenancy.json、数据模型文档、CHANGELOG；**按文件名读迁移的测试**（如 `00041_freight_templates.sql`）整词替换抓不到，要单独改。
- **通知**：与状态变化同事务写 outbox；新增任何改订单 / 退款 / 库存的语句或调用都要在 `internal/service/notification_policy.go` 的 `notificationCallSites` 登记（`TestEveryStateTransitionNotifiesOrSaysWhyNot`）。外发渠道（微信 / 短信 / 邮件）只有接口，默认 skipped。
- **不在会话之间传凭据**（见 memory `no-credentials-between-sessions`）。客户端要后台前置状态时，它发单号过来，由本机脚本代做。
- 路线图已重排（README 与 `docs/电商系统-总体架构.md` 第十三节）。

## 常用操作

- 全量测试：自起 `keel-postgres:16`（`--shm-size=1g`，唯一容器名），`env -u PGUSER -u PGPASSWORD -u PGDATABASE PGHOST=127.0.0.1 PGPORT=<port> make test-db`，结束 `docker rm -f -v`。不要与 `./scripts/check-all.sh` 同时跑。
- 部署演示栈：
  `export COMPOSE_PROJECT_NAME=keeldemo KEEL_HTTP_PORT=18099 KEEL_CONSOLE_PORT=18100 KEEL_AUTH_SECRET="$(cat ~/.config/keel/demo-auth-secret)" KEEL_VERSION=$(git rev-parse --short HEAD) KEEL_COMMIT=$(git rev-parse HEAD) KEEL_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) && docker compose -f compose.yaml -f compose.infero.yaml up -d --build`
- 迁移试跑：`pg_dump` 演示库 → 恢复到临时容器 → `GOOSE_DBSTRING=... make migrate`。
- 后台代做（给客户端 e2e）：`~/.local/share/keel-eshop/bin/demo-admin.sh ship|reject|approve|receipt <no>` 或 `api METHOD PATH [JSON]`（现签 e2e 操作员的一次性 token，用完作废）。
- 客户端会话：Remote Control「App客户端编译配置」/「订单生命周期批处理」，地址 `bridge:session_01Bon3JCdkr4yGYp8w81KQqz`（2026-09-27 晚更新，旧的 01NKFeh… 已失效；单向，它回不了消息）。**日常 e2e 一律无头 H5 全量跑**（用户 2026-09-27 定）：真机要解锁、要人在场、慢且不可复现；真机验证只留到发版前做一次，Android / iOS / 小程序平时只验证编译。

## 踩过的坑

- goose 不补低号迁移（见上）。
- 并行 agent 同时跑 check-all 与 test-db 会互相踩（npm ci 重建 node_modules 与 go list 冲突）。
- 主机负载高时（多路测试 + 演示栈同机），搜索日志写入可能超 200ms 被放弃 → 响应缺 trace_id。
- 5 路 agent 齐发曾同时撞 429 额度上限（营销、报表中断）——现在并行上限 3 路；机械活（合并冲突、改号、文档）派 Sonnet。
- 这一批号段 00041–00052 低于演示库已有的 00053，合并时全部改号：导入 00050→00054、运费 00041/42→00055/56、报表 00048→00057、营销 00044→00058。
- 数据模型文档末尾「待确认事项」编号多路并行时会撞号（运费与营销都用了 31–36），合并后要顺延（营销已改 37–42）。
- CI runner（lenserver）用本机 Go：`/usr/local/go` 已升到 1.26.8（旧版备份 `/usr/local/go-1.25.6.bak`）；`scripts/ci_go_toolchain.sh` 用 `GOTOOLCHAIN=local` 测真实版本。

## 待办（按「重要 × 紧急」排，2026-09-27 重排）

排序口径：先看重要（拖着会不会越来越贵、会不会伤到数据 / 安全 / 别人的工作），再看紧急。纯文案、纯功能扩展往后放。

### 本轮已做完（2026-09-27）
- 合并收尾分支 + 全量 test-db / check-all 通过 + 在「00058 + 种子」的复刻库上试跑 00059–00061 + 部署演示栈 + 通知客户端。
- **买家端时间早 8 小时**（`2c9247a`）：`shortTime` / `day` 截 UTC 字符串 → 改为解析时刻、按设备本地时区显示。库（timestamptz）、服务端（契约 UTC）、后台（Date 解析）、报表（店铺时区切天）都是对的，只有买家端错。私有栈 + 公网 H5 / 后台均验证。
- 限流按真实客户端 IP：`KEEL_TRUSTED_PROXIES`（`9ea398a`），默认一个都不信。
- 数据模型待确认事项 22/23/24/26 标已解决、9 标部分解决；残留注释改掉；部署指南补「多实例部署」。
- 发布流水线补推 `keel-postgres` / `keel-migrate` / `keel-console` 三个镜像（`fb318fd`）。
- `products.total_stock` 删掉（00062）；库存相对调整 `POST …/inventory/adjustments`（00063 `inventory_logs.reason`，后台改库存对话框默认「加减」），子 agent 做、合并时 00070 改号 00063。
- 前端发布脚本 `publish-frontend.sh`（部署层，不进仓库）。
- CAS 改库存（PUT）也写 `inventory_logs`（`fd2bc6d`）：CAS 成功即知旧值 = expected。**首次推送后 ghcr 新包默认私有，要到 GitHub Packages 设置里改成 public**。

### 深度审查（2026-09-27）已修
- 00064 补索引（skus.product_id 缺失，2 万商品时列表 / 检索 55 秒/页）；SAGA 窗口里关单凭空回补库存；退款回补锁序；
  登录连续失败锁定；00062 改空操作（滚动发布兼容，下一版再删列）、00063 CHECK NOT VALID；券错误文案分 / UTC；
  买家端：自动续期 + 统一跳登录、列表原地加购与规格浮层、详情页购物车入口、下单直达付款、沙箱底栏结算、地址丢街道、
  券截止日、折扣率精度；后台日期区间默认 23:59:59。
### 深度审查留下的（未修，需拍板或较大）
- ~~0 元订单付不了~~ **已修（自动入账）**：收尾分支在 MarkOrderPlaced 同一事务里 settleFreeOrder（10→20、paid_cents=0、不落 payments、核销券、发支付成功通知）；下单直接返回 20，再发起支付 409，售后回无可退。回归测试 TestZeroPayableOrderSettlesOnPlacement。
- ~~单字搜索无结果~~ **已修**：索引侧 search_text 在二元组后追加单字（search.IndexTerms），查询侧不变；search_text 指纹独立版本 bigram-v2，00086 把 product_understanding.updated_at 拨回纪元触发全库重判（光升版本号触发不到已有商品），只重写 search_text、不重算向量；已在演示库副本上试跑。演示站部署新 API 后生效（种子已同步）。
- 后台订单 / 售后日期筛选按浏览器时区切天，报表按店铺时区（改契约传日期 + 店铺时区）。
- 订单号 / 退款单号前缀日期是 UTC（order.go:778 / refund.go:1015），纯展示。
- 后台订单按手机号筛选 OR 用不上索引（admin_orders.sql ~38，20 万单 60ms×2）；has_open_refund 子计划、按门店数售后总数。
- 促销 SKU 移除与并发预占时被静默跳过（promotions.sql:260）；ReservePromotionSku 不复核活动状态 / 时间窗。
- 测试缺口：买家 B 操作 A 的订单没有回归测试；已支付订单二次入账没有测试；e2e 结算 / 券 / 活动下单几乎不断言金额；
  售后 4 条在缺 staff token 时静默跳过。
- 列表卡片价格不体现限时特价（卡片 ¥69–89，浮层里实际 ¥49.9）；确认页「技术信息」对买家可见。
- 下一版：新迁移 DROP products.total_stock（旧版本全下线之后）。

### 多实例实测（2026-09-27，3 实例 + nginx 轮询 + 1 个 Postgres）
- 修了：refresh_token 并发轮换（原来并发 5 次全成功）；登录锁定计数进库（00065，原来阈值 ×3、重启解锁）；连接池可配
  （`KEEL_DB_MAX_CONNS` + `DTMRS_DB_POOL`，默认 3 实例就打满 max_connections=100，读请求 7% 500）。
- 实测成立：定时任务并发扫（无重复处置）、支付回调 / 退款审核重复打、无业务缓存、协调器用 Postgres 存储可多实例。
- 仍不准：/search 限流每实例一份（×N）；上传必须共享存储（S3 driver 未实现）；迁移无锁（只能跑一次性任务）。
- 压测（同一台 20 核机器上全部同跑，是下限）：列表 6300 req/s（PG ~10 核，读瓶颈在 PG）；下单 650 单/s p95 178ms（PG ~5.5 核）。
- 拆库结论：到指标再拆（见最终报告里的指标线）。
### 商品卡片
- product-card 组件（首页 / 分类 / 搜索共用）；卡片上没有划线价（列表接口没有特价字段，要加契约）；
  搜索结果里看不到活动标签（挂耳咖啡有限时特价，搜索行没标签）—— 待查 SearchHit.promotion_tags 是否填了。

### 微服务拆分（进行中，施工图 docs/电商系统-微服务拆分方案.md）
- 粒度：只拆 inventory（门店库存 + 活动配额），券 / 订单 / 支付留 core；三档部署（单体 / 同库独立 schema+账号 / 物理拆分）。
- 阶段 0（KEEL_ROLE、库存库连接池、内网签名调用、带载荷与 HTTP 的分支）✅ 6890c74
- 阶段 1a（库存读侧 + 后台库存写改走 inventory.Service）✅ 8609298
- 阶段 1b ✅ ae5abae（下单库存分支、关单/退款回补走 outbox、活动配额搬家、删外键、库存迁移目录、两库集成测试；合并后全量 test-db + check-all 通过）
- 阶段 2 ✅ d9c2470（已 push）：B 档（库存迁移按 current_schema 建、`KEEL_INVENTORY_ROLE`，`split-migrate.sh prepare-b`）、
  C 档 `compose.split.yaml`、`scripts/split-migrate.sh copy|verify|cutover|rollback`（可重复执行）、对账任务（每小时，只读只报）、
  `KEEL_INTERNAL_SECRET_PREVIOUS` 轮换、**00085 orders.placed_at**（C 档杀库存服务时发现：SAGA 没走完的 10 单能付款 → 现在 409）。
  实测：B 权限两个方向都拒、B/C smoke 全过、C 杀 inventory 下单恢复后推完（退避封顶 300 秒）。全量 test-db + check-all 通过。
  **B 档 e2e 栈**（2026-09-27 晚）：COMPOSE_PROJECT_NAME=keelb，`-f compose.yaml -f compose.infero.yaml -f compose.split-b.yaml`，局域网 http://192.168.0.110:18781（后台 18782），auth 密钥 `~/.config/keel/b-auth-secret`；**已 down -v**（e2e 跑完：45 过 / 0 失败 / 9 跳过）。代做：`KEEL_ADMIN_API=http://192.168.0.110:18781/api/v1 KEEL_ADMIN_PG=keelb-postgres-1 demo-admin.sh …`。
  e2e 已切到专用买家 13800000001（客户端写好了 e2e.env）。
  **C 档客户端 H5 全量 e2e：40 过 / 0 失败 / 14 跳过**（演示站已是 C 档；首轮 3 条 promotion 失败 = 种子缺活动配额行，9293b56 修）。B 档 e2e ✅ 45/0/9。
- 阶段 3 ✅（README 中英「部署形态：单体与微服务」，命令原样跑过）。
- 待办：秒杀单 SKU 热点吞吐基线；下一版删 promotion_skus.stock_qty / sold_qty。

### A. 等用户拍板 / 批准（被权限拦下的）
1. ~~e2e 专用买家~~ **已建（2026-09-27）**：13800000001，杭州默认地址（运费 8 元已验），口令在 `~/.config/keel/demo-e2e-buyer-password`（0600），`reset-demo.sh` 调 `~/.local/share/keel-eshop/bin/e2e-buyer.sql` 重建。待用户在客户端机器上设 `KEEL_E2E_PHONE/KEEL_E2E_PASSWORD`。
2. **演示站让限流按真实 IP 生效**：代码已支持，但链路是 中转机 nginx → frp → 本机 Caddy → API。要 ① 中转机 nginx 设 `X-Forwarded-For $proxy_add_x_forwarded_for`；② Caddy `reverse_proxy` 里 `trusted_proxies 127.0.0.1`（frpc 从本机回环进来）让它透传而不是覆写 XFF；③ 演示栈 `KEEL_TRUSTED_PROXIES` 设成 Caddy 到容器的来源（docker 网桥，`172.16.0.0/12`）。改网关配置属于共享基础设施，没动。
3. ~~是否发 v0.2.0~~ **已发（2026-09-27，tag d530fd7）**。

### B. 重要不紧急——越拖越贵，优先于新功能
4. 下次部署演示栈：API（00062–00063）→ `publish-frontend.sh all` → 外部验证。

### C. 有时限，但时点由发版决定
5. **真机验证**（见下节）。

### D. 可以放——功能扩展
6. M7：拼团（资金状态机，需产品口径）、循环满减、活动效果报表、业务重排接入 w_promo。
7. M5：离线评测集；cross-encoder 精排（等 infero `/v1/rerank`）。
8. 小项：报表同比、店铺信息扩充、通知偏好、客服电话在买家端展示。
9. 演示站定时自动重置（用户目前要求手动一键）。

### 例行运维（到点就做，不排序）
- 包邮券模板每人限领 100，约 100 轮 e2e 后调高；演示库库存定期补到 100。

## 发版前必须做的真机验证

- **结算页在 iOS 原生上的渲染**：营销活动那轮客户端在半锁屏的 iPhone 上跑 promotion 用例 6 条全挂（Connection closed），aftersale / smoke 单独跑却能过，分不清是设备还是结算页新内容的问题，记为「留待发版前真机验证」，没有算通过。
- ~~原生端商品图能否实际加载~~：**iPhone 15 + 小米 2206122SC 真机验证通过（2026-09-27，4d52227）**，相对 302 能跟随。购物车行 / 订单行的图随后补上（ce2d6cd，SKU 无图时退回商品主图，订单行下单时快照）。仍未验证：长标题两行省略（缺数据）、小程序真机。
- tab 未读角标（原生 API，H5 测不到）、相册选图上传凭证（选择器无法自动化）。
- **时间显示**：`view.uts` 的 `new Date(iso)` 在 Android / iOS 原生上能否解析 ISO（带毫秒与 Z）——只验证过编译与 H5。

## 待用户拍板

- 是否发下一版（v0.2.0 之后微服务拆分等又积累在 Unreleased）。
- 真实支付 / 微信登录 / 短信的资质。
