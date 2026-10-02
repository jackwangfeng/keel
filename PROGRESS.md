# keel 进度（2026-09-28 晚更新，v0.6.0）

新会话先读这份，再看 CLAUDE.md 的 token 纪律。

## 当前状态

- **v0.6.0**（2026-09-28，加固版）：AI 经营演示站实跑验收 + 三轮破坏性测试的修复（多收款自动原路退回 00150、单价上限 00142、检索相关度下限 00140、简报更正 00141、售后期 00151 等），核心库落在 **00151**。发布说明见 CHANGELOG `[0.6.0]`。
- **渠道适配层第一期：骨架（2026-10-02，分支 `channel-phase1`，未合并、未部署）**：设计 `docs/superpowers/specs/2026-10-02-channel-adapter-design.md`（用户逐段确认：订单走「渠道单 + 关联 keel 订单」两层、一份库存不硬切、`orders.user_id` 放开可空、`KEEL_CHANNELS` 两级开关），计划 `docs/superpowers/plans/2026-10-02-channel-phase1-skeleton.md`。已做：迁移 00300（`channel_merchants`，开关 + 版本，库存库镜像）/ 00301（binding、门店 / SKU 映射、库存分配与价格规则、推送状态、回调留档）；`internal/channel`（按角色拆分的接口 + Caps + 注册表 + 规则计算，测试用假渠道 `channel/channeltest`）；库存服务对开了渠道的商家每次可售数变化发 `stock.changed`（闸门：开关关零查询，开着按商家缓存 30 秒）；core 重算入队、推送 worker（按 binding 分批、CAS 冲突以 keel 覆盖、凭据失效停推）；回调入口 `POST /webhooks/channels/{binding_id}`；后台 `/admin/channel-kinds`、`/admin/channel-bindings/*`（读全店范围、写管理员、凭据只写）；不变量测试（开关关 / 开但无 binding：零 stock.changed、零渠道任务、零行）。**没做**：压测对照（第一期开关关闭时热路径零新 SQL，挪到第三期改 orders 时做）、后台界面。**下一步**：第二期 Shopify（拉商品写 `channel_item_links`、推库存价格；Dev Dashboard 应用 + client credentials，token 可能 24h 过期要续）、第三期渠道订单（core 00320 起）、第四期美团模拟、第五期对账（00340 起）。**踩过的坑**：唯一键必须 merchant_id 打头、有 updated_at 就要挂触发器、新表 DDL 要进数据模型文档（迁移闸门）；库存服务的表的查询只能写在 `inventory_svc.sql`（拆分边界测试）、SQL 里不许写 merchant_id（只靠 RLS）；契约测试按 handler 文件清点 query 参数（读 query 的接口单独一个文件）；OpenAPI 枚举值 `none` 和 `StoreMatchType` 撞名会让生成器给全部常量加前缀（改用 off / inbound / outbound）；手建租户上下文的 worker 只能在登记过的文件里（`channel_worker.go`）。**Shopify 联调要用户做的**：spec §15（注册 Partner + 开发店、Dev Dashboard 建应用、凭据放 `~/.config/keel/shopify-dev`）。
- **演示站跑 v0.7.0（2026-10-02 部署）**：`/version` = v0.7.0 / e39f6e2；app、inventory、console 本机构建（`KEEL_VERSION` / `KEEL_COMMIT` / `KEEL_DATE` 三个都要传，只传版本号时 commit 和 date 为空），无新迁移（库 00240 / 库存库 00240）。注意 `up -d` 不带 `--no-deps` 会连三个 Postgres 和 dtmrs 一起重建（卷不变、数据无损，但有几秒中断），只换应用时加 `--no-deps`。
- **演示站**（https://eshop.zzss.fun，C 档：core + inventory 两个进程、两个库）：API `dde5deb`（2026-09-29 地图底图上线；前端 `f25ef57`（2026-09-30 11:30 发布，Flutter 3.41.9 构建：首页问候按时段细分、购物车 / 详情价格行窄屏换行、地址保存提示挪到按钮上方；应 Mac 会话请求）。**网关缓存头（2026-09-30）**：`/srv/keel-eshop/Caddyfile` 加了 Cache-Control——买家端全部 no-cache（Flutter 的 main.dart.js 等不带哈希）、`/gfonts/*` 30 天、后台 `/admin/assets/*` 一年 immutable、其余 no-cache；之前没有 Cache-Control，浏览器按 Last-Modified 自估缓存，发版后还显示旧版。备份 Caddyfile.bak-cache；改完 `sudo systemctl restart keel-eshop-gateway`（admin off，不能 reload）。坑：Caddy 的 header 不认 `!@matcher`，要写 `@x not path ...`）、库 00151；最近一次部署前备份 `~/.local/share/keel-eshop/backup-pre-00151-202609282256.dump`。环境与 compose 清单在 `~/.local/share/keel-eshop/demo-env.sh`（`source` 后用 `"${DC[@]}"`）；**库存在库存库 keeldemo-postgres-inventory-1 / keel_inventory**。部署：`"${DC[@]}" up -d --build inventory app console` + `bin/publish-frontend.sh all`，外部验证走 44922 跳板。
- **CI**（自建 runner lenserver = 本机）：端到端那一步 2026-09-27 起每次都红 —— 宿主机 18180 被演示站 Caddy 网关占了，已换到 28180 / 28181。**发版 workflow** v0.3.0–v0.5.0 的「建 release」都红（之前的会话先手动 `gh release create`，workflow 再建撞 already exists；镜像其实都推上去了），已改成已存在就 edit。**以后发版只打标签，不要手动建 release。**
- **跨商家隔离**：仓库里跨租户 / 权限 / 范围的测试 135 条 + 租户检查脚本全绿；2026-09-28 补了 AI 员工密钥只在本店有效、多收款退回 / 简报更正 / MCP 时区按店隔离四条。多商家真栈的人工抽查被安全机制拦下没做成（写越权请求清单 / 脚本会被拦）——以后这类验证一律写成仓库里的防守性回归测试。
- **infero CPU 后端已达标**（infero `cb0ccbd`，2026-09-29，「Fix the CPU backend's real-world performance: parallelism, decode cost, and serial attention」——GEMM 换成并行化的 `gemm` crate、常驻 F32 权重缓存、attention 按 (head, token) 并行；在 2026-09-26 首版 `0bcd714` 单线程基础上）：2026-09-29 本机（20 核，负载约 7，空闲窗口）实测——单条短查询 20 次中位数 117 ms / p90 124 ms（门槛 250 ms）、60 字查询 10 次中位数 359 ms / p90 384 ms（门槛 430 ms）、64 条一批中位数 2.68 s（门槛 5 s）——**三条判据全过**，此前「待空闲时段验收」的待办完成。向量与 GPU 版余弦 ≥ 0.9999995、model 名相同。负载 17–30 时会明显变慢（单条约 450–1000 ms、64 条约 5.3–10.2 s），**CPU 形态需要留核**（建议 4–8 核以上专用）。**Keel 文档已同步为「无 GPU 可选 CPU 推理」**：总体架构 §1「那个缺口」与 §6 形态 A、README（中英）、CONTRIBUTING、ai-capabilities、语义检索层设计、部署与配置指南（新增「没有 GPU」一节：编译 `--features cpu`、启动参数、`KEEL_EMBED_ENDPOINT`、核数与内存建议）、compose.infero.yaml 注释、scripts/infero-up.sh 提示、CI 注释均已改。**容器形态已落地**：`compose.infero-cpu.yaml`（从 infero `500ff62`（或更新）的 `Dockerfile.cpu` 本地构建，引擎在 compose 里、app 等 `/health/ready`；容器内模型目录名决定模型名，默认挂成 `Qwen3-Embedding-0.6B` 保持与 GPU 版同名，本机验证余弦 0.9999995）。CPU 版编译在 `~/work/infero/target-cpu`（与 GPU 版的 target 分开）。
- **地图底图 / 地图选点（2026-09-29，dde5deb + e45c13d，演示站已上）**：服务端代理天地图瓦片 `GET /geo/map` + `GET /geo/tiles/{layer}/{z}/{x}/{y}`（LRU 64 MB / 7 天、每 IP 60/s、全站总闸 10/s 瞬时 40）；App / H5 有 flutter_map 地图选点页，小程序仍用 wx.chooseLocation；后台门店坐标与围栏改走同源瓦片（不再直连 OSM）。配置 `KEEL_MAP_TILES=tianditu` + `KEEL_TIANDITU_KEY` + `KEEL_TIANDITU_REFERER`，演示站 key 在 `~/.config/keel/tianditu-key`（demo-env.sh 读入，Referer `https://eshop.zzss.fun/`）。**坑**：①天地图服务端 key 取不了瓦片（403「权限类型错误」），要浏览器端 key + 域名白名单 + 代理带 Referer；②不带 Referer 从服务端连打两百来次，浏览器端 key 被禁用过一把（403「Key已被禁用」）——测天地图只发个位数请求；③天地图两台 WAF 节点里 116.205.76.122 对合法请求固定回 418、116.205.76.86 正常，代理遇 418 换节点重连（随机拨解析出的地址，最多 3 次）。**Android 真机已验（2026-09-29，小米 12S Pro，Android 包连演示站）**：安装启动、定位、地图页天地图两层、中心图钉逆地理编码、拖动与确定回填均正常（后两步由用户在手机上确认）。**本机（Linux）能编安卓包了**：Gradle 走本机代理只对这一条命令生效——`JAVA_TOOL_OPTIONS="-Dhttp.proxyHost=127.0.0.1 -Dhttp.proxyPort=8890 -Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=8890 -Dhttp.nonProxyHosts=localhost|127.0.0.1" ANDROID_HOME=~/android-sdk JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64 flutter build apk --release --target-platform android-arm64 --dart-define=KEEL_API_BASE=https://eshop.zzss.fun/api/v1`（首次约 3 分钟；Flutter 迁移器会改 android/gradle.properties，编完 `git checkout` 还原）。手机无线调试：`adb mdns services` 能发现同网段的 192.168.0.52:5555，`adb connect` 后在手机上点允许。本机签名与 Mac 编的不同，覆盖安装会 INSTALL_FAILED_UPDATE_INCOMPATIBLE，要先卸载。**iOS 真机已验（2026-09-29，Mac 会话在 a5d7506 上 make flutter-ios-install 一次通过，ios/ 无改动；用户在 iPhone 上看过「没问题」）**。
- **架构改进第一、二档（2026-09-30，ccb0984，演示站已上：库 00180 / 库存库 00173，部署前备份 `backup-pre-00180-{core,inv}-202609301041.dump`）**：
  ①公开仓库 + 自托管 runner：外部贡献者 PR 全部要审批、fork PR 不在 lenserver 跑；②退款 / 多收款退回事务里另取池连接会自锁 → 走本事务，并加 `repository/txguard.go` 防线（测试里事务内再取池连接直接 panic，另抓出提案复盘一处）；③优雅停机（SIGTERM → 停接新请求 → 等在途 → 停后台 → 关协调器 → 关池，`KEEL_SHUTDOWN_GRACE` 默认 20s）、ReadHeaderTimeout、公网 `/readyz`；④库存 RPC 读超时 800ms + 熔断；⑤后台任务统一交 `internal/worker`（recover 重启、10 个扫描类 advisory lock 选主、`KEEL_BACKGROUND`）；⑥连接启动参数 statement_timeout 15s / idle_in_tx 30s，扣减与 SAGA 分支 lock_timeout 3s；⑦RLS 下索引失效：手机号冗余列（00170/00171）、类目区间比较、关键词召回走 SECURITY DEFINER 函数 `keyword_hit_products`（00172，窄权限角色 keel_search_definer 带 BYPASSRLS，**建它要超级用户，托管 PG 上可能建不出**）、后台订单单号 / 手机号拆独立查询；商品列表第一页 713ms → 0.08ms；⑧保留期清理（检索日志 90 天、工具调用与库存流水 180 天、幂等存档按到期；拆分形态库存流水暂无人清）；⑨有货标记改二阶段消息（跨 0 才发、接收方回源、全量刷新降到每小时），活动配额同步改二阶段消息（配额定义回 core，00180）；⑩迁移规矩写进 CONTRIBUTING + `scripts/check_migrations.py`。
  拆分部署新配置：库存进程 `KEEL_CORE_URL`、自己的 `KEEL_DTM_DSN`（invdtmdata 卷，演示站叠加层已改名 keeldemo_invdtmdata），core `KEEL_INTERNAL_ADDR=:8091`。演示站实测：SKU 24 在门店 1 扣到 0 再恢复，两条跨 0 消息两边屏障各一条、标记 f→t 正确。
  **补丁（74848d0）**：跨 0 消息管不到「从没设过库存」的 SKU，而标记表缺行按有货排 → 新门店（宝安中心区店，门店 7）所有商品都排进有货段、要等整点全量刷新才正常；现在建门店 / 建 SKU / 导入商品之后当场补齐标记（`seedStoreStockFlags` / `seedProductStockFlags`，有回归测试）。
  **遗留**：dtmrs barrier 表未清（要先向协调器拿已结束 gid）；回查屏障只适合「去查一下」类消息（keel_app 对 barrier 无 SELECT）；库存对账未比对 quota_qty；商品列表无货段最坏 O(全店)；第三档（uni-app x 下线、部署收进仓库、配置集中、可观测性、Tx 接口收窄）未做。
- **演示站底图 2026-09-30 起换成 OpenStreetMap**（`KEEL_MAP_TILES=osm` + `KEEL_TILE_PROXY=http://192.168.0.110:8890`，demo-env.sh 末行；备份 demo-env.sh.bak-osm）：①最初的总闸瞬时 40 张让放大一级就一半瓦片 503、小城市地图空白 → 放宽到瞬时 400 / 每秒 20、单 IP 瞬时 300；②天地图 418 重试加到 5 次后两台 WAF 节点都对本机 IP 回 418，整站地图灰掉 → 退回 3 次、演示站换 OSM；③国内服务器直连 tile.openstreetmap.org 不通（DNS 污染）→ 新增瓦片专用代理 KEEL_TILE_PROXY。实测全国视图连放 6 级到青海乡镇 42 张全 200。**商业化前要换回有审图号的服务商**（天地图谈白名单或商用图商）。另：demo 镜像重建时 fetch-dtmrs 从 GitHub 克隆会断，构建要带 `--build-arg HTTPS_PROXY=http://192.168.0.110:8890`（同 http_proxy / NO_PROXY=localhost,127.0.0.1,goproxy.cn）。
- **上传文件进对象存储（2026-10-02，fb2d65c + c28a391，演示站已切）**：S3 兼容 driver（`service/upload_s3.go`，driver 2，minio-go；OSS / COS / AWS / SeaweedFS 通用）；`UploadStorage` 写落主 driver、读删缩略图孤儿回收按每行 `uploads.driver` 路由（换存储不停服）；可选预签名直连（`KEEL_S3_PRESIGN_ENDPOINT`，演示站没开：SeaweedFS 不对外）；`keel-uploads migrate`（进 app 镜像）先核 sha256 / 大小再写再改指、可重跑。自建用 SeaweedFS 4.48（`compose.s3.yaml`，不用 MinIO：社区版已停发二进制与镜像）。演示站：切换前备份 `backup-pre-s3-{core,uploads}-202610021037.*`，切换后老图照旧可读，64 张迁移全部搬完（0 缺失 0 不一致），外网读回字节数一致、缩略图从桶里出。凭据 `~/.config/keel/demo-s3-secret`（0600）；demo-env.sh 的 DC 加了 `-f compose.s3.yaml`（备份 `.bak-s3`）。**待办**：观察几天后删 `keeldemo_uploads` 卷里的旧文件、撤 `KEEL_UPLOAD_ROOT`（部署指南「上传文件换到对象存储」第 4 步）。
- **协调器独立部署（2026-10-02，dccb49a + 1e2158d，演示站已切：库 00240 / 库存库 00240，切换前备份 `backup-pre-dtmrs-{core,inv}-202610020942.dump`）**：用户拍板「微服务直接上独立的 dtmrs」。C 档 core 与库存都不再嵌协调器，连独立部署的 dtmrs 0.12（`KEEL_DTM_SERVER` / `KEEL_DTM_TOKEN` / `KEEL_SELF_URL`，compose 里 `dtmrs` + `postgres-dtm`）；单体照旧嵌入（`KEEL_DTM_DSN`）。库存不再知道 core：跨 0 通知发主题 `stock.zero_crossing`（载荷 `{store_id, sku_ids}`，`allow_empty_topic`），core 启动后台订阅；配额同步定义 + `quota_rev` 随载荷（库存按版本接受），`KEEL_CORE_URL` 停用（配了拒绝启动）。两库测试改在真起的 dtmrs 上跑（`internal/dtm/dtmserver`），`fetch-dtmrs.sh` 顺带编服务端二进制。演示站：两个嵌入式 sqlite 协调器确认 0 未终结后切换；smoke 下单两次（seed 买家，各 1 件、已沙箱支付），重启 dtmrs 后照常下单、订阅还在。令牌在 `~/.config/keel/demo-dtm-token`（0600），demo-env.sh 读它；叠加层卷 `invdtmdata` 换成 `pgdtmdata → keeldemo_pgdtmdata`（备份 `.bak-dtmrs`）。旧的 `keeldemo_invdtmdata` 卷与两份 sqlite 已不用，留着备查。方案见 `docs/电商系统-微服务部署方案.md`（服务发现交给平台、serverless 方案未实施）。
- **检索相关度预判（2026-10-01，866be26 + 5d30987，演示站已上：库 00230，部署前备份 `backup-pre-00230-core-*.dump`；令牌在 `~/.config/keel/kev-token`，demo-env.sh 读它）**：演示站第一轮 5 秒判完 23 个热词 528 对；「连衣裙」不再混进开衫 / 大衣 / 牛仔裤，「保温杯」（店里没有）从 6 件咖啡器具冒充命中变成正确的「猜你想要」。infero 已做同请求批量前向（引擎侧 20 题 64 ms，每题边际 14→2.2 ms；公网实例 max-seqs 8→64 修了大请求崩溃），稳定性复测 160 个 8–64 题请求并发 8 全成功；跨请求攒批暂不需要（现在走离线）。离线评测（`cmd/keel-searcheval` sample → label → calibrate，Kev-4B 打标、与人工参照一致 47/50）证明余弦下限分不开相关度（0.40 精确率 0.13），而 Kev-4B 在线约 25 ms + 14 ms/题、挡不到搜索前面。所以后台预判：`service/search_judge.go`（选主，每小时）给热词（7 天 ≥3 次）的向量独有候选判相关度存 `search_relevance_judgments`，检索判过的按 P(是) ≥ 0.3 留、没判过照旧余弦。配置 `KEEL_SYSTEMONE_ENDPOINT` / `KEEL_SYSTEMONE_TOKEN`（只走环境变量）。Kev 公网实例 `http://kev.zzss.fun:10025`（bw 经 frp，临时、无高可用），keel 有独立 key。**已提给 infero**：同请求内各题并行（现约 14 ms/题、跨请求不合批），做到 20 题 <50 ms 再考虑在线判。评测数据在 `~/.local/share/keel-eshop/searcheval/`。
- **搜索召回改造（2026-10-01，8358a80，演示站已上：库 00220，部署前备份 `backup-pre-00220-core-202610011739.dump`）**：多词查询 AND 不够一页时，向量路可信命中（≥ VectorFloor）凑够一页就不跑 OR（keyword_match=and+vector）；OR 命中集合封顶 1000（`DefaultOrHitCap`）。复压见 `docs/性能压测-2026-10.md` 第十一节：**不带引擎** c=16/32/64 p95 51/99/170 ms 全部达标（主要靠 OR 封顶）；**接 CPU 版 infero 反而不达标**（p95 ≈320–340 ms，向量路几乎全部超时降级）——CPU infero 单条查询吞吐恒定约 8 条/s、不跨请求合批、不取消已超时的请求（压测停后还满载 11 分钟）。演示站用 GPU 版（宿主机 18081）不受影响。**待办**：①infero CPU 版合批 + 取消超时请求（infero 会话的活）②VectorFloor 0.40 对这个模型偏松（无关词 0.41–0.44 也过线，向量是近似造的，要离线评测集重标）③~~`compose.split.yaml` 卷名写死 `keel_*_split`，两套拆分栈会共用卷~~ 已修（2026-10-02）：split / split-b / multi 三个叠加层的卷名前缀改 `${COMPOSE_PROJECT_NAME}`，默认项目名下与原来一字不差，演示站解析结果逐字节相同、无需重建。
- **第二轮破坏性测试 + 界面乱点（2026-10-01，私有栈 keelchaos，修复 8b05a10，演示站已上：库 00190，部署前备份 `backup-pre-00190-{core,inv}-202610011143.dump`）**：服务端（kill / pause 库存与 core、SIGTERM、持锁、几百并发、近 3 万单）未发现 P0，有货标记 / 订单与库存净扣减 / 库存流水全部对上，秒杀不超卖、并发退款只成功一次；后台乱点 2380+ 次零未捕获异常；H5 乱点只有 P2（已由 Mac 修，H5 3308363 已发）。修掉：①57014 / 55P03 回 503 busy（Retry-After 3，「未生效」只在 `internal/outcome` 记录器确认本请求没落地任何写时才说；库存进程同样回 busy，rpc.ErrBusy 为确定失败、不计熔断）②活动上线后总是补一条配额同步消息（并发改配额交错，测试钩子可确定性复现）③熔断打开时下单 3 秒内 503 不落单 ④超时关单满预算连跑（单次唤醒 ≤20 轮）⑤选主连接 idle_session_timeout（ping 间隔 ×3）+ TCP keepalive，冻结的当选者被服务端断开后别的实例接手 ⑥围栏判定统一改平面几何 `fence::geometry`（00190 表达式 GIST；与地图直边一致，原 geography 南边线内缩约 3 米）⑦围栏 GeoJSON 读回 24 位逐位相等 ⑧有货标记缺行按无货、接上 `in_stock_only`、seed 失败进 inventory.release 队列重试 ⑨请求体类型错误点名字段（problem.WriteBindError）、后台整数输入框 precision 0、副标题 maxlength、客服电话格式 ⑩搜索 size 1–100 ⑪admin-responsive-check 支持 ADMIN_SESSION_JSON。**注意**：outcome 记录器只看数据库写；以后「先对外动作再写库」的路径要手动 `outcome.MarkDurable`。买家路径上 RLS 下空间索引仍用不上（&& 非 leakproof，按 merchant_id 取本店几十行再判，改前也一样）。小程序真机输入 bug（mp-flutter）未修，小程序先别提审。
- **后台画围栏显示其他门店（2026-09-30，09f9168，演示站已上）**：同商家别家店的围栏紫色虚线 + 浅填充、位置紫点 + 店名（≥12 级才显示店名），不接点击、可开关；数据来自已有的 `GET /admin/stores`（本来就带 fence 与坐标），服务端没改。分工：**商家后台归 lenserver**（用户 2026-09-30 定），Flutter 买家端归 Mac 会话。
- **GitHub SEO 与 Gitee 镜像（2026-09-30）**：GitHub 仓库简介改中英双语关键词、主页填演示站、加 20 个 topics；README 首屏关键词行；分享预览图 `docs/assets/social-preview.png`（**要在 Settings → Social preview 手动上传**）。Gitee 镜像 https://gitee.com/dahuangfeng96/keel （公开，关了 Issue / Wiki，简介写明 GitHub 为主仓）：`.github/workflows/mirror-gitee.yml` 在 push main 时同步 main 与 v* 标签、打标签时只同步标签，用仓库变量 GITEE_REPO + 密钥 GITEE_TOKEN（来自本机 gitee-cli 登录的令牌）。坑：Gitee 新建仓库默认私有，转公开要账号先开 2FA 或绑第三方账号（已绑）；转公开后网页匿名可看，但匿名 git clone 当时仍要求登录。
- **结算自动选收货地址（2026-09-30，服务端 d1be5f1 + 客户端 e438afc / 84227df，H5 e8c6f1e 已上演示站）**：`GET /addresses?store_id=` 给每条地址标 `in_service_area`（true / false / 缺省=判断不了；判据与下单围栏校验同一条，测试逐条对照）；结算页没明确选择时：默认地址不在围栏外就用它 → 否则围栏内离「送至」点最近的 → 都不在退回原行为；手动选择永不覆盖，自动选中且确定在范围内时提示「已按当前门店自动选择配送范围内的地址」。iPhone 真机已验（用户确认）。分工：服务端 lenserver、客户端 Mac 会话。顺带修：契约经纬度 / 围栏坐标 / 距离改 format: double（e8c6f1e；原来 Go 端 float32 写库掉约 1 米精度，演示站已存的围栏要重新保存一次才完全精确）。
- **后台手机浏览器可用（2026-09-29，4565a36，演示站已上）**：≤768px 菜单进抽屉、全站表单单列 / 弹窗不出屏 / 控件够手指点；订单、售后、多收款退回、AI 员工、经营概览、商品、门店与库存有手机卡片（电脑版不变）。`make admin-responsive-check` 按手机 390 + 电脑 1440 逐页（36 项：页面 + 标签页 + 「新建」弹窗）检查并截图，改前手机 0/36、改后 36/36，电脑始终 36/36；发布前查本地构建用 `LOCAL_DIST`（脚本拦截后台页面文件换成本地产物，接口仍走线上）。坑：卡片整张包在 router-link 里时，卡片内按钮要 `@click.stop.prevent`（只 stop 挡不住 <a> 的默认跳转）。
- ghcr 四个镜像 keel / keel-migrate / keel-postgres / keel-console 均可匿名拉取。真机：iPhone 15 验过；Android 真机、小程序真机未验。
- **uni-app x 买家端下线（2026-09-30，300b267）**：架构-设计与加固.md 待办里第三档「uni-app x 下线」项完成。删了整个 `app/` 目录（源码、e2e、native-android/ios 壳工程、脚本、构建产物）与专属脚本（`gen_uts_schema.py`/`check_uts_contract.py`/`check_app_types.py`/`check_app_build.py`）；Makefile 去掉全部 `generate-uts`/`app-*` 目标，`check-all.sh` 去掉对应两步；`contract_operations.py` 头注释改 Dart 独占；README（中英）、CONTRIBUTING（十三步改十二步）、docs 索引与指南、flutter_app/README、PR 模板、CI workflow 里提它的地方都改成「已下线，见 tag `uniapp-final`」或直接去掉。留档 tag `uniapp-final` 已推到远程，指向下线前的 HEAD `4105526`。`flutter_app/lib` 下代码未动（归 Mac 会话），只改了 README。验证：check-all.sh / go build+vet / test-db / flutter-analyze+flutter-test 全过。

## Flutter 买家端（flutter_app/，2026-09-28 起主要维护的客户端）

- **对外展示（eshop.zzss.fun 的 `/`）2026-09-28 起是 Flutter Web**：`~/.local/share/keel-eshop/bin/publish-frontend.sh h5`（本机构建，SDK `~/development/flutter`，**钉在 3.41.9**（`flutter_app/.flutter-version`，mp-flutter 只支持这一版；CI 会校验）；PATH 上的是 flutter_ohos）。uni-app x 版留作 `h5-uniapp`（回滚用），上一份产物备份在 `/srv/keel-eshop/h5-uniapp`。
  **国内打得开的关键**：Flutter Web 默认从 gstatic 取 CanvasKit 与补字字体，屏蔽后整页空白（实测）。构建加 `--no-web-resources-cdn`，`flutter_bootstrap.js` 注入 `fontFallbackBaseUrl: "/gfonts/"`，引擎补字清单的 725 个字体镜像在 `~/.local/share/keel-eshop/gfonts`（24M），发布时拷进产物。升级 Flutter 后按 `bin/cache/flutter_web_sdk/lib/_engine/engine/font_fallback_data.dart` 补镜像。屏蔽 google/gstatic 后从公网打开首页验证通过（请求全在本站）。首屏要下 main.dart.js 约 3M + canvaskit.wasm 约 7M（Caddy gzip）。

- **状态**：对齐 uni-app x 全部页面（第一阶段购物主链路 + 第二阶段券 / 消息 / 资料 / 服务地址 / 售后），main `fce951c` 起。uni-app x（`app/`）冻结，只修 bug。
- **验收**：`make flutter-analyze flutter-test`（107 条单测）· `KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-e2e-web`（Web 无头 25 条，带前置状态的 5 条读 `KEEL_E2E_*`，没设跳过；**按手机屏 390×844@3 跑**，`KEEL_E2E_VIEWPORT` 可改 —— flutter drive 默认 1600×1024 桌面宽度测不出窄屏问题）· `make flutter-build`（web / apk / ios）· `make flutter-build-mp`（mp-flutter `0.3.1`（pub.dev：flutter_miniprogram + mp_flutter_wechat；驱动随包发布，`dart run flutter_miniprogram e2e-driver` 打印目录，经 pub.flutter-io.cn 镜像；主包 v0.3.0 起由 mp_flutter 改名；安卓真机输入修复：构建期改写 \\p{} 正则、原生框移出屏幕单光标、iOS 光标透明；真粗体、着色器空闲预热、冷启动开关默认全开：dart 优先 preload、CanvasKit 提前编译、粗体首帧后下载、启动资源并入主包；iOS 光标默认常亮），主包约 600KB，带 `--no-licenses`）· `make flutter-ios-install KEEL_IOS_TEAM=2Q89DQSSH6`。
- **小程序**：开发者工具与 mp-flutter 会话共用，用前打招呼；`flutter_app/tool/mp_walk.js` 冒烟（复用 mp-flutter 的 drive.js）。报给 mp-flutter 的：安全区 / 胶囊（viewPadding 恒 0）、冷启动首页滚动位置、webgl2 日志 —— 在修。凭证上传（chooseMedia → uploadFile）、定位（getLocation wgs84）、image_picker / geolocator 的 Web 注册在小程序里还没实测。
- **样式**：2026-09-28 逐页（首页、搜索、详情、购物车、结算、订单列表 / 详情、我的、券两页、消息、地址列表 / 编辑、资料、服务地址、售后列表 / 详情 / 申请、登录）与 uni-app x 的 H5 同尺寸截图对照并对齐（`app/dist/build/h5` 与 `flutter_app/build/web` 各起一个带 /api 反代的静态服务，playwright 用本机 Chrome 截图，e2e 买家的令牌直接写进两边的 localStorage）。Web 上字体不同（Roboto vs 苹方），真机上都是系统字体。
- **小程序**：安全区（mp-flutter main 4273715）、冷启动在顶部、启动无 error、顶栏右侧按钮让出胶囊（`MpWechat.menuButtonRect`）已在开发者工具验过；凭证上传与定位也在开发者工具实测过（mock chooseMedia 返回真文件、wx.uploadFile 真传，售后单带上凭证；getLocation 成功按坐标解析）。实测抓到的 `Random.secure()` 不可用（K4）与 cullRect / Null check 控制台报错（K5）mp-flutter 已在 bc76bf5 修好并复验通过；App 侧的随机数兜底保留（mp-flutter 取不到种子时仍不提供 crypto）。列表图用 `NetImage` 按显示尺寸解码（cacheWidth）。
- **小程序性能（2026-09-28 iPhone 15 / 安卓真机调试实测）**：列表图向服务端要缩略图（`?w=` 四档）；小程序解码像素比封顶 2x（480 档，`12e8962`）；Web / 小程序上缩略图不传 cacheWidth（Web 引擎带 cacheWidth 每张解两遍，`79f47c0`）；mp-flutter perf-native 分支改原生解码后 decode-slow 归零，稳态滑动 iOS 43–60 / 安卓 45–62 fps。剩下首屏 3 次长帧（iOS 约 700–820ms、安卓约 300–360ms）全是回退字体分片到达 → 注册 → fontChange 全量重排版，mp-flutter 在做常用字合一字体（首帧前注册）+ 合并 fontChange，并查安卓 font-fetch 正好 1s。测量版做法：pubspec 临时切 perf 分支（不提交）、`dart run flutter_miniprogram … --perf-hud --dart-chunk-kb 900`（v0.3.0 前叫 mp_flutter）（真机调试强制 ES5，分片要小）、真机调试控制台右键 Save as 导出日志（开发者工具日志与端口都拿不到真机 console）。字体优化后（perf-font2 80f8de5，`--cjk-font=full`）实测冷启动安卓 3.9s / iOS 4.3s（之前 5.2 / 5.8），首屏只剩 iOS 一次回退分片（mp-flutter 在补符号）。mp-flutter 已合回 main `4e78c7e`（原生解码、常用字合一字体默认 full、private_infos），pubspec.lock 已锁、`make flutter-build-mp` 加了 `--no-licenses`，正式码在开发者工具模拟器验过。长时间滑动 + 进出详情（perf8）无内存告警、无空白图、无闪退。**剩下**：详情页首次打开 iOS 长帧 200–400ms —— 实测只有 15 段文字（Chrome 挂 CanvasKit Paragraph.layout 计数），是 iOS 上每段 10–16ms 的引擎开销（疑首次 shaping / w700 合成加粗）；keel 侧已等转场动画走完再换内容（`f61efc7`），引擎侧确认是合成加粗：v0.2.1 带真粗体后 iOS 详情首开 layout 从 102–243ms 降到 4–54ms，长帧剩 54–125ms（other，路由首次 build / paint），冷启动 3.9s 不变。
- **H5 乱点测试的 4 个 P2（2026-10-01）**：① 报错兜底文案改成面向买家（非 Problem 响应 / 2xx 解析失败 →「网络开小差了，请稍后重试」，网络层 →「网络异常，请检查网络后重试」，不再拼 HTTP 码与异常原文；细节 debugPrint 到 `keel.api:`）；② 「我的优惠券」未登录先判断、不发请求拿 401，显示登录入口（同领券中心；其它要登录的页由「我的」入口拦）；③ 输入含 `<` `>` / \u0000 / \u0001 的 TypeError：本机 Chrome 无头逐字打 + insertText 5 组都复现不了，记待查（疑真机浏览器输入法 / 自动填充通道）；④ 生僻字首帧豆腐块：Web 补字分片按需下载的时序，便宜的做法是把界面用字子集成小字体启动预载，但商品名等服务端文字照旧会遇到，收益有限，暂不做。
- **Web 端令牌存储说明（2026-10-01，P5）**：`flutter_app/README.md`「安全」一节写明 H5 的 access / refresh token 在 localStorage（XSS 可读走）、为什么四端共用 Bearer、以后换 httpOnly cookie 要改的 7 处（服务端 Set-Cookie、CSRF、session.dart、client.dart、凭证图、退出、测试）。代码未改。
- **小程序真机输入阻塞（2026-10-01，P2）**：安卓真机打字不进 Flutter 输入框（v0.2.2 / v0.2.4 都有，一直没在真机验过），iOS 聚焦时出现黑 / 绿两个光标且不对齐；已交 mp-flutter（mp-flutter-52）。小程序先别提审；P2 其余流程等修好再走。已验：问候「上午好」、定位后「送至 …」。
- **POI（2026-09-28，`4a5c308`，设计 docs/POI-设计.md）**：首页「送至 …」（定位 → resolve → 后台 /geo/reverse；拿不到就显示门店）可点换地址（`/place`：输入提示 / 小程序地图选点 / 收货地址 / 当前定位 → `StoreService.deliverTo` 按新坐标重新 resolve；老地址无坐标先用全文 suggest，失败回落默认店并提示）；地址编辑页「搜索地点，自动填写地址」（suggest → reverse 补全 → 填省市区、street、region_code、lat/lng；手改省市区清掉坐标与区划码）。地址表单 PUT 带回 lat/lng。GCJ⇄WGS 在 `api/geo.dart`（对照服务端 coord.go）。**待**：演示站地图 key（用户去高德申请，服务端会话配）后联调；小程序 chooseLocation 要 mp-flutter 在 app.json 声明 requiredPrivateInfos（已提）+ 小程序后台开通接口。
- **踩过的坑**：`go('/login')` 再 `go` 回来会在转场中重建外壳（Duplicate GlobalKey）→ 登录页一律 push / pop；主题按钮最小宽度无穷大放进 Row 布局失败 → 页面测试必须用 `keelTheme()`；Web e2e 同一输入框第二次 `enterText` 收不到；`timeout(onTimeout: () => null)` 遇到运行时不可空的 Future 会因协变当场抛错（坐标被吞）；本机真实位置会被围栏解析到别的店 → e2e 用 `KEEL_LOCATE=off`；Gradle 不读 http_proxy → android 仓库走阿里云镜像。

## 里程碑

| 阶段 | 状态 |
|---|---|
| M1–M4 | ✅ v0.1.0 |
| 交易链路补齐（购物车、地址、/me、取消、发货、确认收货与自动确认、售后退款与寄回物流、买家上传、后台订单页与售后页、平台幂等、员工重签登录 token） | ✅ |
| M5 搜索质量可量化 | 大半：业务重排（库存因子）、search_logs、/search/events 回传、`make search-metrics` ✅；**cross-encoder 精排**（等 infero `/v1/rerank`）、**离线评测集** 待做 |
| M6 能开店 | 站内通知（00053）✅、运费模板与包邮券（00055–00056）✅；真实支付 / 微信登录 / 短信 **需资质，用户明确暂不做** |
| M7 能做生意 | 批量导入与 AI 类目推荐（00054）✅、经营报表看板（00057）✅、营销活动（满减满折 / 限时特价 / 秒杀 / 新人礼，00058）✅；拼团、循环满减未做 |
| **M9–M11 AI 经营（AI 员工）** | 规划与 M9 设计已定稿（2026-09-28）：`docs/AI经营-规划.md`、`docs/AI经营-M9设计.md`。外部 harness（Claude Code 等）经 MCP 接入，写操作默认走提案审批；迁移号段 M9 = 00090–00099。任务 0（README 改诚实）✅、任务 1（AI 员工与接入密钥：00090、/admin/agents、/agent/whoami、kagt_ 密钥）✅。任务 2（/api/v1/mcp，10 个读工具，00093 审计，限流）✅。任务 3（restock_plan：日均分母去掉断货天，库存服务新增 StockoutDays 本地 + HTTP）✅。任务 4（提案：propose_inventory_adjust / list_my_proposals、/admin/agent-proposals 批准即以 AI 员工身份执行、48 小时过期，00091）✅。任务 5（简报：post_brief、/admin/agent-briefs，00092；站内通知挪到 M10）✅。任务 7（agent/AGENTS.md + CLAUDE.md 软链、skills/巡店日报.md 与 补货.md、mcp.json.example、runner/claude-daily.sh、cmd/keel-mcp stdio 桥）✅。任务 6（后台「AI 员工」页：提案批准/驳回、简报、AI 员工与密钥、首页待处理横幅）✅。任务 8（演示站：AI 店长（演示）staff_id 5，密钥 ~/.config/keel/demo-agent-key 0600；cron 每天 08:00 `~/.local/share/keel-eshop/bin/cron-agent-daily.sh`、每小时 :17 `cron-simulate.sh`，日志 `~/.local/share/keel-eshop/logs/`）✅。任务 9（端到端验收：2026-09-28 首跑出简报 #2 与 10 条补货提案，批准 #1 后示例小店 sku 3 可售 3→73）✅。**M9 完成。** 遗留：`shop_overview` 客单价按买家数算（待核）；演示库 SKU created_at 早于回填订单才算得出正常置信（已把 skus.created_at 前移 30 天）。POI 见下一行 |
| **M10 / M11 AI 经营** | 2026-09-28 完成，v0.5.0，设计 `docs/AI经营-M10M11设计.md`。M10：四种新提案 00120（限时折扣 / 发券 / 改文案 / 售后审核，批准后以 AI 员工身份执行）、事件与 webhook 00121（stock_low / refund_created / search_zero_spike / proposal_decided，MCP 拉 + HMAC 签名推）、执行后复盘与成绩单 00122、`slow_movers` / `promotion_review`、五本手册、`agent/runner/claude-events.sh`。M11：自动执行策略 00130、只读 SQL 00131（角色 keel_agent_ro + schema agent_ro 脱敏视图 + 静态拒 set_config + 执行后复核租户）、公开 AI 经营日志 00132（web/ai-log/，演示站 /ai-log/ 已开）。第二个 harness：官方 Python SDK 2.2.0 实测通；Gemini CLI / opencode 能连但模型供应商拒绝无头调用（见 docs/AI接口.md 实测记录）。演示站：cron 每 10 分钟 `cron-agent-events.sh`（没事件不启动 agent）；AI 店长的加库存自动执行策略：单笔 ≤50 件、24 小时 ≤5 条。 |
| **AI 经营演示站实跑验收（2026-09-28 晚）** | 演示站当前 `4fbd2f9`、库 00151（部署前备份 `backup-pre-00140-202609282024.dump`；验收时 `2cd7244`，备份 `backup-pre-aiaccept-{core,inv}-202609281940.dump`）。**五种提案全部实跑到执行**：补货（#13 命中自动执行策略 3→18 件）、限时折扣（#11 → 活动 #3，9/29–10/6 9 折）、售后审核（#12 → 退款单 40，AI 员工身份审核）、改文案（#14 商品 18）、发券（#15 → 券模板 #4）。事件唤醒（stock_low / refund_created）✅；webhook 8/8 送达、HMAC 全部验过（临时接收端跑在 app 容器网络里，验完已删、webhook 配置已撤）；成绩单与公开日志 ✅。复盘：强制把 #13/#14 的 outcome_due_at 提前验过流水线，**结论是假的已撤回**，恢复 10/05 到期。实跑修了两处：**MCP 时间改店铺时区**（0519577）、**list_refunds 加 order_shipped_at + 售后审核手册按它判**（2cd7244，AI 把 50 退款中当成已发货）。AI 在自主巡检里守住了「不硬提」（搜索缺口、活动复盘、改马克杯文案三轮都给了成立的不提理由）。**实跑发现**：① ~~向量召回无相关度下限~~ **已修（56e1d03，00140，演示站已上）**：只被向量路捞到的相似度 ≥ 0.40 才算可信，没有可信命中回「猜你想要」+ `fallback: true`，无结果口径含 fallback。演示站 55 条探测：店里没有的 20 词 13 个 fallback（保温杯 / 手机壳 / 羽绒服这类近义品类仍命中，要等精排）；有货的 35 词 33 个正常、低分尾巴砍掉，意图类「提神的饮品」「送女朋友的礼物」走 fallback 但仍展示；② 其余验收发现 **2026-09-28 晚全部修完**（演示站 `3da1a11`、库 00141，部署前备份 `backup-pre-00141-202609282053.dump`）：复盘窗口没走完就推迟、`promotion_review` 券分支加 `refunded_order_count`、`propose_coupon` 支持固定时段、`search_insights` 加按词点击 / 下单与 `low_click_queries`、简报更正 `corrects_brief_id`（00141）、`shop_overview` 写明客单价口径（支付 ÷ 买家数是行业口径，不是 bug）。顺带：商品列表没有 SKU 的商品不再顶到首位（Flutter 会话 e2e 报的；搜索里它按缺货降权是对的；「精确标题排第一」目前不保证，要不要加权待用户定）|
| **POI** | 后端 /geo/reverse、/geo/suggest（高德 Web 服务，key 在 ~/.config/keel/amap-key，demo-env.sh 读入）；收货地址带坐标（00100）；下单 / 试算校验地址在门店围栏内（422 address-out-of-range，无坐标不拦、默认店不拦）；后台门店地址搜索回填；Flutter 首页「送至…」与填地址搜索（Flutter 会话）。已上演示站（ee999e3）。待定：是否收紧为「无坐标也拦」 |
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
### 深度审查留下的（2026-09-28 清了一轮）
- ~~0 元订单付不了~~ 已修（自动入账，TestZeroPayableOrderSettlesOnPlacement）。~~单字搜索无结果~~ 已修（00086）。
- ~~后台订单 / 售后日期筛选按浏览器时区切天~~ **已修（957dda7）**：契约加 `created_date_from` / `created_date_to`（YYYY-MM-DD，两端含），服务端按店铺时区切（`AdminOrderService.ShopDayRange`，与报表同一个 reportLocation）；与 created_from/to 混传 422；后台 dayRange 改传日期。
- ~~促销 SKU 移除与并发预占被静默跳过、ReservePromotionSku 不复核活动状态~~ **拆分后已不成立**：配额在库存服务 activity_stocks，移除走 InvLockPromotionActivity 同一把锁、DELETE 带 sold = 0；扣减查不到配额行即拒；下单算价只取上线且在时间窗内的活动（priceOrder 与建单同一事务）。
- ~~测试缺口：买家 B 操作 A 的订单、已支付订单二次入账~~ **已补（855a986）**：别人确认收货 404、别人申请售后 404 order-not-found、已支付订单来第二笔不同流水的回调（200，落支付单留痕，实收与 paid_at 不变）。
- ~~列表卡片不体现限时特价~~ **已修（fdcf78e）**：契约 `ProductSummary.promo_min_price_cents`（活动价低于门店最低价才给），列表与检索都填；**检索结果之前从来不填活动标签**，一起补上；Flutter 卡片活动价 + 门店价划线（单规格打特价不显示「起」）。uni-app x 冻结只重新生成类型。
- ~~确认页「技术信息」对买家可见~~ **Flutter 已改（fd977ae）**：release 包不显示，debug / profile 保留。uni-app x 未改（冻结）。
- 仍未修：订单号 / 退款单号前缀日期是 UTC（order.go newOrderNo，纯展示）；后台订单按手机号筛选 OR 用不上索引（admin_orders.sql AdminListOrders，要表达式索引 `receiver_snapshot->>'phone'` + 改写成两路，得在 20 万单数据上 EXPLAIN 验证）、has_open_refund 子计划、按门店数售后总数；e2e 结算 / 券 / 活动下单几乎不断言金额；售后 4 条在缺 staff token 时静默跳过。
- 下一版：新迁移 DROP products.total_stock（旧版本全下线之后）。

### 破坏性测试（2026-09-28 晚，私有栈 keelfz，三路：钱与并发 / 输入校验 / 状态机与 AI 工具；测完已 down -v）
已修（演示站 `3f31007`、库 00142，部署前备份 `backup-pre-00142-202609282145.dump`）：
- **P0 单价无上限**：int64 最大值作价格，结算求和溢出成负数 500、单买能下天价订单 → 后台 / 定价 / 特价 ≤ 一亿元（与导入同一个 `catalogimport.MaxPriceCents`），00142 约束兜底（NOT VALID）。
- **P0 未发货订单按行分别退完**：运费不退、订单停在 20 还能发货 → 前一张在途时拒「加上它正好退完」的申请（引导撤回后整单退）；发货兜底「每件都已退 / 在退不能发」。
- P1 退款原路挂到最早一笔成功支付（可能是金额不符那笔）→ 按 orders.paid_cents / paid_at 对上认账的那笔。
- P1 提案批准时目标已变 → 500 且永久卡在 15 → 只有临时性故障留 15，其余记 40 执行失败。
- P1 发券提案校验比后台宽 → 复用 validateCouponTemplate；券金额上限一百亿元。
- P1 自动执行 daily_limit 并发突破（12 路、上限 2 → 4 条）→ 策略行 FOR UPDATE；keelfz 上用同一脚本复验恰好 2 条（进程内测试窗口太小抓不住）。
- P2 query_sql 无字节上限（响应 400MB）→ 1MB / 单值 4KB 截断。契约里券接口的权限描述对齐代码。
- **多收款：少发生 + 兜住（a7d3862，00150，演示站已上，部署前备份 `backup-pre-00150-202609282220.dump`）**：`payment_intents` 一单至多一个有效支付意图（同渠道复用流水号、换渠道旧的作废）；`payment_returns` 订单不认的到账（重复 / 取消或关单后 / 金额不符）原路退回——支付回调同事务开单当场提交（沙箱当场退回；真实渠道等 /webhooks/refunds/{channel}，PR 单号分派），每分钟兜底扫描补开 + 重试。后台「多收款退回」页、买家订单详情 payment_returns（Flutter 已显示）。演示站实测：同单微信 + 支付宝都付，第二笔 ¥159 以「重复支付」退回（40）。**真实渠道接入时还要**：换渠道作废旧意图时调渠道关单接口；退回单提交调渠道退款接口（现在非沙箱只推到 30 等回调）。
**其余待拍板 2026-09-28 晚按「对买家和商家最合理」的口径全部处理（演示站 `4fbd2f9`、库 00151，部署前备份 `backup-pre-00151-202609282256.dump`）**：② 未发货退款释放活动配额、**每人限购不释放**（防特价买了退、退了再买）；③ 试算每行回 `available_qty`，Flutter 结算页标缺货、禁提交；④ 售后期 `after_sale_days`（默认 15 天，完成后起算，00151），超期 409 after-sale-window-closed，订单详情 `after_sale_deadline`，后台店铺设置可改；⑤ 停业门店的待支付单不能再发起支付（409 store-unavailable），已支付照常发货；⑥ 平台会话带 X-Keel-Merchant 管的是那家店的员工（建 / 列 / 改 / 重签），不带头管平台员工；⑦ 精确同名且有货的商品排第一（exactTitleFirst）。
**未覆盖**：越权 / 跨租户这一路没跑完（单商家模式开不了第二家店），下一轮用多商家栈补。

### 多实例实测（2026-09-27，3 实例 + nginx 轮询 + 1 个 Postgres）
- 修了：refresh_token 并发轮换（原来并发 5 次全成功）；登录锁定计数进库（00065，原来阈值 ×3、重启解锁）；连接池可配
  （`KEEL_DB_MAX_CONNS` + `DTMRS_DB_POOL`，默认 3 实例就打满 max_connections=100，读请求 7% 500）。
- 实测成立：定时任务并发扫（无重复处置）、支付回调 / 退款审核重复打、无业务缓存、协调器用 Postgres 存储可多实例。
- 仍不准：/search 限流每实例一份（×N）；上传必须共享存储（S3 driver 未实现）；迁移无锁（只能跑一次性任务）。
- 压测（同一台 20 核机器上全部同跑，是下限）：列表 6300 req/s（PG ~10 核，读瓶颈在 PG）；下单 650 单/s p95 178ms（PG ~5.5 核）。
- 拆库结论：到指标再拆（见最终报告里的指标线）。
### 商品卡片
- ~~卡片没有划线价、搜索结果没有活动标签~~ 已修（fdcf78e，见上）。

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
- 待办：下一版删 promotion_skus.stock_qty / sold_qty。
- 大规模秒杀：**用户 2026-09-28 定为暂不做**，方案记在 `docs/大规模秒杀-方案备忘.md`（先量基线 → 准入 / 配额分片 / dtmrs 推荐的 Redis 预扣 + Redis 屏障 + SAGA 建单）。今天的秒杀正确但吞吐没量过；dtmrs 的 Redis 存储、Redis 屏障、msg 模式 Keel 一样都还没用。

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
