# 商家后台（Keel Admin Console）

Vue 3 + Vite + Element Plus，中文界面。跟着 `docker compose up` 一起起来，
默认在 <http://localhost:8081>。

它存在的理由很具体：契约里 26 条 `/admin/` 操作全都能用，**但只能用 curl 打**。
路线图 M4 的产出标志写着「商家可自助发布」，而没有界面，那句话只对会用 curl
的人成立。

---

## 一、界面里没有一个手写的请求 / 响应类型

所有形状都来自 `web/src/api/schema.d.ts` —— `make generate` 从
`docs/电商系统-OpenAPI.yaml` 生成并入库的那份产物。**没有副本**：后台通过
tsconfig 的 `@contract/*` → `../src/api/*` 直接 import 那一份文件
（`scripts/check_admin_types.py` 会断言它真的在编译范围里，并拒绝
`web/admin/src` 下出现同名文件）。

### 也没有重写一个客户端

`src/api/client.ts` 直接用 `web/src/api/client.mts`：那份 SDK 的 49 条路径、
query / path / body 三个槽的必填性、Problem 与非 Problem 的分流，全部由契约
推导，而且已经有 `make schema-check` 守着。后台再写一份「差不多的」就是第二个
会分叉的真相源——而分叉的那一份不会有任何闸门抓到。

SDK 缺的两件事补在**它自己留的扩展点**上，一行没改它：

| 缺的东西 | 怎么补 |
| --- | --- |
| 动态的 `Authorization` 头（登录 / 登出会变，SDK 的 headers 构造时固定） | 用 `KeelClientOptions.fetch` 包一层注入 |
| multipart 上传（`POST /admin/uploads`，SDK 的 body 槽只认 `application/json`） | 单独一个 `uploadProductImage()`，但响应类型仍取 `ResponseBodyOf<"/admin/uploads", "post">` |

### 变异验证：改字段名真的会红吗

跑过，记在这里，因为「类型绑定住了」这句话不验证就只是一句口号：

```bash
# 1. 把契约里的 min_price_cents 改名
sed -i 's/min_price_cents/min_price_fen/g' docs/电商系统-OpenAPI.yaml
# 2. 重新生成入库产物
make generate-ts
# 3. 后台的类型闸门
make admin-type-check
```

实测输出（vue-tsc 3.3.11）：

```
src/views/ProductDetailView.vue(484,45): error TS2551: Property 'min_price_cents'
  does not exist on type '{ ... }'. Did you mean 'min_price_fen'?
src/views/ProductListView.vue(224,39): error TS2551: Property 'min_price_cents'
  does not exist on type '{ ... }'. Did you mean 'min_price_fen'?
退出码 = 2
```

改回去（`git checkout docs/电商系统-OpenAPI.yaml && make generate-ts`）之后恢复绿。

---

## 二、为什么后台在 `web/admin/` 而不是 `web/src/`

`web/src` 下有一道 `--strict` 闸门（`make schema-check` →
`scripts/check_ts_scope.py`），守着契约产物与那份零依赖 SDK。
**这次没有动它一个字节**：`web/tsconfig.json` 的 `include` 仍是 `["src"]`，
`check_ts_scope.py` 仍然走 `web/src`，仍然是 5 个文件。

考虑过的三条路与否决理由：

**1. 把 Vue 应用放进 `web/src`。否决。**
那道闸门的全部价值是「零 node_modules」——`npx` 拉一个钉死版本的 `tsc`，
`types: []` 不去扫任何 `@types`。Vue 应用一进去，这条性质立刻没了：
`vue` / `element-plus` 的 import 解析不了，而且 `tsc` 根本读不了 `.vue`
（SFC 不是 TypeScript，`--allowArbitraryExtensions` 只管 `.d.*.ts`）。
更糟的是 `check_ts_scope.py` 的后缀表里没有 `.vue`，于是几十个文件会
**静默地不在任何闸门的视野里**，而闸门照样报绿——那正是那个脚本的文件头
写着要防的那件事。

**2. 扩展 `check_ts_scope.py` 让它认识 `.vue`。否决。**
认识没用，`tsc` 编不了。要编就得把那一条的编译器换成 `vue-tsc`，
于是契约产物那道闸门的成败就绑在一棵 UI 框架的依赖树上：
element-plus 的某个 `.d.ts` 在新版 TS 下报错，契约的闸门会变红，
而那和契约一点关系都没有。**注意这条路的思路本身被采纳了**——
只是落在后台这个项目上（`scripts/check_admin_types.py` 认 `.vue`），
不落在契约产物那道零依赖闸门上。

**3. 把 `web/admin` 从闸门范围里 exclude 掉。不需要，也没做。**
`include: ["src"]` 本来就够不到 `web/admin`。这里写一句是为了说清：
**没有新增任何 exclude**，覆盖范围没有被缩小过。

采纳的形态：`web/admin/` 自带 `tsconfig.json` 与 `vue-tsc`，
由新增的 `scripts/check_admin_types.py` 核，接进 `./scripts/check-all.sh`
（以及 CI 的 gates job，在跑闸门之前 `make admin-install`）。

### 那道新闸门自己也验证过

`--listFiles` 那条范围断言不是摆设。实测（记在
`scripts/check_admin_types.py` 的文件头）：

- `vue-tsc` 的目录形式 `include: ["src"]` **是**收 `.vue` 的
  （和 `tsc` 漏掉 `.mts` 不一样，这里不抄一句没验过的话）；
- 被 import 到的文件，编译器顺着 import 照样会读，所以少一条 glob 不影响它们；
- 真正会漏的是「**没有任何人 import**、include 又不匹配」的那些——
  最常见的来源是**页面写完了还没接路由**。去掉 `.vue` 那条 glob 再放一个
  没被引用的坏 `.vue` 进来：`vue-tsc` 退出 0 报绿，`check_admin_types.py` 红。

---

## 三、跟着 compose 起来

`compose.yaml` 里的 `console` 服务，多阶段构建（`docker/Dockerfile.admin`）：
node 阶段 `npm ci` + `vite build`，nginx 阶段托管产物并把 `/api` 反代给 `app`。
于是浏览器看到的是**同源**，不需要 CORS。

没有选 `go:embed` 进二进制。代价说清楚：那要往 Go 的路由表里加一条 catch-all，
而那张表有两道锁（`internal/app/run_test.go` 的路由清单、
`internal/handler/contract_test.go` 的 `nonContractRoutes`），两边都要为一个
前端资源登记「契约之外的路由」。那两份清单存在的理由恰恰是「契约之外不该有
路由」。而且改一行 CSS 会变成重编 Go 二进制 + cgo + dtmrs（分钟级 vs 秒级）。

端口：`KEEL_CONSOLE_PORT`，默认 8081。**刻意不叫 `KEEL_ADMIN_PORT`**——
compose 里已经有一个 `KEEL_ADMIN_PASSWORD`，而那是数据库超级用户的口令。

---

## 四、服务端刻意设计过的东西，界面没让它们白费

### 库存 CAS 冲突（409 + `current`）

`PUT /admin/skus/{sku_id}/inventory` 的 `expected_available_qty` 是必填的
「我看到的那个值」，对不上就 409，并把当前真实值放在 `Problem.current` 里。
`src/components/InventoryDialog.vue` 把「你看到的」与「现在是」并排显示，
并给两个重试按钮：

- **用新值重试** —— `expected` 换成 `current.available_qty`，新值不动；
- **按差额重算** —— 保持「我本来想加/减多少」，在当前真实值上重算。
  补货场景（进货 100 件）走这条。

收窄用的是契约类型 `InventoryConflict`（= `Problem & { current: AdminInventory }`），
不是手写的 `{ current: ... }`。

这条接口**没有**幂等键（契约的 parameters 里只有 SkuId）：PUT + CAS 本身就是
幂等的。

### 合规拒绝（422 + `errors[]` 带 offset / length）

`src/components/HighlightedText.vue` 在输入框下方贴一条对照，把违禁词标红。

**为什么不直接在输入框里染色**：`<input>` / `<textarea>` 的内容是纯文本，
浏览器不给在里面放 `<mark>` 的办法；真要做只能拿 contenteditable 顶替输入框，
那会换掉 Element Plus 的整套表单行为（校验、清空、禁用、IME）。

**offset 按码点切，不是 UTF-16 下标。** 契约写明单位是 Unicode 码点，
而 JS 字符串是 UTF-16 —— 标题里有一个 emoji，`slice()` 就会切歪，
而歪掉的高亮比没有高亮更糟：它指着一个没问题的字说这里违规。
`sliceByCodePoints()` 用 `Array.from` 按码点拆。

503 `compliance-unavailable`（保守拒绝）也原样显示，并说清「没改成」。

### 幂等键

契约里 7 条 `/admin/` 的 POST 带 `Idempotency-Key`，界面全都带。
判据（`src/api/idempotency.ts`）不是「成功 / 失败」，是「这次提交结束了没有」：

| 结果 | 钥匙 |
| --- | --- |
| 网络错误 / 没拿到响应 | 留着 |
| 503（建议退避重试） | 留着 |
| 409 `idempotency-key-in-flight` | 留着 |
| 2xx | 换 |
| 其余 4xx（422 / 404 / 403 / 409） | 换 |

后半条不是可选的：一次**得到了明确拒绝**的提交，用户会改点什么再提交，
那是另一个请求体，沿用旧钥匙正好撞上 422 `idempotency-key-reused`。

---

## 五、哪些页面真能用，哪些是留位置

| 菜单 | 状态 | 用到的契约操作 |
| --- | --- | --- |
| 登录 | 真能用 | `POST /admin/auth/bootstrap`、`POST /admin/auth/session`；`POST /admin/auth/email-link` 按钮留着，服务端本轮回 501，界面把那个 Problem 原样显示 |
| 商品 | 真能用 | `GET/POST /admin/products`、`GET/PATCH/DELETE /admin/products/{id}`、`POST .../publication`、`PUT .../images`、`POST .../skus` |
| SKU / 库存 | 真能用 | `PATCH/DELETE /admin/skus/{id}`、`PUT /admin/skus/{id}/inventory` |
| 类目 | 真能用 | `GET/POST /admin/categories`、`PATCH/DELETE /admin/categories/{id}` |
| 上传 | 真能用 | `POST /admin/uploads` |
| 员工 | 真能用 | `GET/POST /admin/staff`、`PATCH /admin/staff/{id}` |
| 商家管理 | 真能用（仅平台级） | `GET/POST /admin/merchants`、`GET/PATCH /admin/merchants/{id}`，顶栏切换器带 `X-Keel-Merchant` |
| 订单 | 真能用 | `GET /admin/orders`（默认筛待发货 `status=20`）、`GET /admin/orders/{order_no}`（详情抽屉）、`POST /admin/orders/{order_no}/shipments`（发货，带 Idempotency-Key） |
| 售后 | 真能用 | `GET /admin/refunds`（默认筛待审核 `status=10`）、`GET /admin/refunds/{refund_no}`、`POST .../audit`（同意 / 驳回，退货退款可裁定运费）、`POST .../receipt`（确认收到退货）。每行退款金额只展示服务端算好的数 |
| 大区 | 真能用 | `GET/POST /admin/regions`、`PATCH/DELETE /admin/regions/{id}`、`GET /admin/regions/{id}/products`、`PUT .../products/{id}/listing`、`PUT/DELETE .../skus/{id}/price` |
| 门店 | 真能用 | `GET/POST /admin/stores`、`GET/PATCH/DELETE /admin/stores/{id}`、`PUT .../fence`、`PUT .../default`、`GET .../products`、`PUT .../products/{id}/listing`、`PUT/DELETE .../skus/{id}/price`、`GET .../inventories`、`PUT .../skus/{id}/inventory` |
| 运费模板 | 真能用 | `GET/POST /admin/freight-templates`、`GET/PUT/DELETE /admin/freight-templates/{id}`（新建带 Idempotency-Key，编辑整体替换）；商品详情「运费模板」下拉走 `PATCH /admin/products/{id}` 的 `freight_template_id`。按省多选与提交前校验是 `src/api/freightRules.ts` 的纯函数（`make admin-test`），运费金额不在前端算 |
| 顶栏铃铛（待办提醒） | 真能用 | `GET /admin/notifications`（下拉最近 20 条，响应带未读数）、`GET /admin/notifications/unread-count`（30 秒轮询、切页刷新）、`POST /admin/notifications/{id}/read`（点一条先标已读再跳订单 / 售后 / 门店库存页签）、`POST /admin/notifications/read-all`。范围与已读都是**调用者自己的**，跳转规则是 `src/api/notifications.ts` 的纯函数（`make admin-test`） |

门店与大区当初是占位页，接上时改的正是这里原先写的两步：换掉
`src/router/modules/stores.ts` / `regions.ts` 里的 component，`index.ts` 那一行没动。
菜单项、路由、页面标题都从同一个 `AdminSection` 对象读（`src/router/section.ts`）。

---

## 五之二、门店与大区：几处做错了不会报错的地方

### 电子围栏的坐标系：WGS-84，编辑器这条路上没有换算

库里是 `GEOGRAPHY(POLYGON, 4326)`，买家端按 `wgs84` 取定位。国内地图给的是
GCJ-02（高德 / 腾讯）或 BD-09（百度），城区偏几百米（`geo.test.ts` 里实测北京
约 550 米 / 1.4 公里）——偏过的多边形照样合法，ST_Intersects 照样给答案，
只是把买家判进错的门店。

所以编辑器是 **Leaflet 1.9.4 + OpenStreetMap 瓦片**（`src/components/FenceEditor.vue`）：
OSM 与 Leaflet 的 lat/lng 都是 WGS-84，点出来的顶点原样存。考虑过国内地图，
否决的理由是它把「存之前必须换算」变成一段只要漏一次就悄悄判错店的代码。
代价是 OSM 瓦片在国内有时加载慢、路网细节不如高德——所以旁边有「粘贴
GeoJSON / 坐标」入口，没网也能配；**换算只发生在那里**，且要运营显式选
「这批坐标来自 GCJ-02 / BD-09」，默认 WGS-84、不猜。

几何规则全在 `src/api/geo.ts`，有 14 条测试（`make admin-test`，接进 check-all.sh）：
经纬度换序只经 `toPosition` / `toLatLng` 两个函数；GCJ-02 逆变换迭代到 1 厘米内；
BD-09 往返约 5 厘米（它公开的那对公式本身不严格互逆，测试里写明了）；
中国境内「两个值都不越界」的经纬度写反给警告。

自交**不在前端判**：服务端 ST_IsValid 拒绝时 PostGIS 的原话原样显示，方括号里的
出错位置在地图上画红圈。

> **实测与契约不一致的一处**：契约说 ST_IsValidReason 在 Problem 的 `detail` 里，
> 服务端实际把它放在 `title` 里、没有 `detail`（`internal/handler/admin_store.go`
> 用的是只写 title 的 `problem.Write`）。界面两处都找，所以现在能用；
> 这是服务端该改的，不在这个后台的范围里。

### 409 按 type 处理，不按状态码

`src/api/errors.ts` 的 `problemHint` 给每个 type 写「下一步」，`ProblemAlert` 与
`notifyError` 都显示它。`store-code-conflict`（换编号会成功）、`default-store-conflict`
（在 POST 上重试永远不会成功，要走 `PUT .../default`）、`store-fence-required`、
`invalid-fence`、`store-ambiguous`、`sku-not-sold-in-store`、`region-has-stores`
各有各的动作。

### 旧的「改库存」在多门店下会拿到 409 store-ambiguous

`PUT /admin/skus/{sku_id}/inventory` 只在商家恰好一家门店时可用。它和 CAS 冲突共用
409，`InventoryDialog.vue` 按 type 分开：`store-ambiguous` 时不给重试按钮，
列出门店，一键跳到那家店的库存页（`/stores/{id}?tab=inventory&sku=...`）。
同一个对话框传了 `storeId` 就走门店维度那条路径。

### has_default 与「未完成」

没有默认门店时，框架上每一页都挂提示（契约要求后台首页挂）；门店列表里
「非默认、没围栏」标成「未完成」——建店按契约不收围栏，这是每家新店都会
经过的中间态，数据库不挡它。

### 可见性两层「与」

`ScopedProducts.vue` 把 `listed`（本层开关）与 `effective_listed`（买家看不看得见）
分开显示；本层开着而买家看不到时说原因（「被大区下架了」），点了上架但仍看不到时
不报成功。

### 契约的一个缺口：没有按 SKU 读出本层覆盖价的接口

商品这一行只给生效价区间与 `price_source`。展开到 SKU 时只能显示基准价与这次会话里
PUT 回来的结果。要逐 SKU 回显，契约得加一条读接口。

---

## 五之三、商家管理与租户切换（平台级）

### 请求头在哪一层生效

`X-Keel-Merchant: <商家 code>`，只在 **`internal/auth/staff_tenant.go`** 里读，由
`StaffBearer` 在验完签名、查完会话、确认在岗**之后**调用（StaffBearer 里只多了一次调用）。
平台级会话带了它：按 code 查出商家（含停用、不含软删），**替换** ctx 里由 Host 解析出的
租户，之后 `WithTenant` 的 `SET LOCAL app.merchant_id` 就落在这家店上，行级安全照旧兜底。

放在 StaffBearer 里而不是一道单独的中间件：「商家级员工带头要 403」必须对**每一条**后台
路由成立，单独的中间件漏挂一条，那条路由就会静默忽略这个头。租户解析器
（`internal/tenant/resolver.go`）一个字没改，它照旧只看配置与 Host，从不读请求头；
那句「刻意不支持用请求头指定租户」旁边补了一段为什么这个例外不违背它（前提是平台级鉴权）。

| 谁带了头 | 结果 |
| --- | --- |
| 平台级会话 | 落在头指定的那家店（读写都是）；停用的店也能切进去 |
| 商家级会话 | **403 `tenant-switch-forbidden`**，不生效，也不静默忽略 |
| 买家接口 / 公开接口 / 未登录 | 根本不读；后台接口上未登录先是 401 |
| code 不存在、已软删、空值、出现两次 | **422 `unknown-merchant`**，不回落到 Host 那家 |

界面这一侧：头只从 `src/api/merchantScope.ts` 一处出来，判据是会话本身
（`staff.merchant_id === null`），商家级员工的请求永远不带；选择绑在会话 token 的指纹上，
登出、换人登录都读不到上一个会话选的店。

### 写权限：不给 keel_app 加 UPDATE

改名与停用 / 启用走迁移 00024 的 `merchant_revisions`：只追加，keel_app 只有
SELECT + INSERT，INSERT 策略是 `WITH CHECK (platform_scope())`——租户作用域里插不进来，
数据库层就是 42501。merchants 那一行一个字节不改（keel_app 在它上面仍然没有 UPDATE）。
当前状态取最新一行修订，解析层、启动自检、定时任务与目录接口共用同一段 SQL
（`tenant.EffectiveMerchantFrom`）。完整论证在 00024 的文件头。

### 单商家部署

`POST /admin/merchants` 在配了 `KEEL_DEFAULT_MERCHANT` 时回 409 `single-merchant-mode`；
同一套部署里停用那唯一一家店、或启用另一家，也回 409——三者都会让下一次启动的自检失败。
商家管理页据 `single_merchant_mode` 把开店按钮置灰并照服务端那句话说明原因。

### 已知限制

- **多商家部署里，后台必须从一个能解析到活跃商家的域名打开**（例如 `shop-a.<基础域名>`）：
  租户中间件在鉴权之前就要一个租户，平台管理员的「入口店」由 Host 给出，头只是在那之后换掉它。
  那家入口店本身被停用的话，要换一个域名打开后台。
- 新开的店没有大区与门店（开店这条路不建），买家打开它会是「不在服务范围」，
  要平台管理员切过去先建门店。
- 平台管理员切到某家店之后「加员工」加的仍是平台操作员：员工的租户从会话继承，
  切换只换数据的租户，不换身份。

### 安全测试与变异结果

测试在 `internal/handler/tenant_switch_test.go`。每一条都做过变异：改掉被守护的那一行、
跑对应测试、用备份文件还原（不用 git checkout）。16 个变异全部让对应的测试变红：

| 变异 | 红的测试 |
| --- | --- |
| 平台带头但不替换 ctx 里的租户 | TestPlatformSwitchLandsOnTheNamedMerchant |
| 商家级带头照样生效 | TestMerchantStaffWithSwitchHeaderIsRejected、TestSwitchHeaderIsRefusedForMerchantStaffOnEveryStaffRoute |
| 商家级带头静默忽略（200，落在自己店） | 同上 |
| 买家侧解析器读这个头 | TestBuyerRequestIgnoresSwitchHeader |
| 鉴权之前就读头（零值身份 = 平台级） | TestAnonymousRequestIgnoresSwitchHeader（得到 422 而不是 401） |
| code 不存在时回落到 Host 那家 | TestUnknownMerchantCodeIsRejectedNotFallenBack |
| 单商家部署照样开店 | TestSingleMerchantModeRefusesToOpenAShop（201，库里多一家） |
| 单商家部署照样停用默认店 | TestSingleMerchantModeRefusesToDisableTheDefaultShop |
| 平台切换只认营业中的店 | TestDisabledMerchantIs404ForBuyersButSwitchable（422） |
| 买家侧解析忽略修订、直接读 m.status | 同上（停用后买家侧仍 200） |
| 商家列表漏掉停用的店 | 同上 |
| 修订表写策略放成 `WITH CHECK (true)` | TestTenantScopeCannotReviseTheMerchantDirectory；migrate_test 的 TestTenantPoliciesArePresentAndExact |
| 有一条后台路由没挂 StaffBearer | TestSwitchHeaderIsRefusedForMerchantStaffOnEveryStaffRoute |
| 契约里有一条后台操作漏声明 KeelMerchant | TestKeelMerchantHeaderDeclaredExactlyOnStaffOperations |

## 六、本地开发

```bash
make admin-install      # npm ci，版本由入库的 package-lock.json 锁定
make admin-type-check   # vue-tsc --strict + 范围核对（已接进 check-all.sh）
make admin-test         # 围栏几何、金额换算、订单与售后界面规则、铃铛跳转的单元测试（node --test，不需要 node_modules）
make admin-build        # 静态产物（compose 起栈时会自己构建，日常不用跑）

cd web/admin && npm run dev   # 开发服务器，/api 由 vite proxy 转给 127.0.0.1:8080
```

依赖版本**全部钉死**（`package.json` 里没有一个 `^`），理由与仓库 Makefile
文件头那条 `@latest` 禁令一样。后台的 `typescript` 钉在 `5.9.2`，与
Makefile 里的 `TSC` 同一个版本 —— 免得两道 TS 闸门用两个编译器。

## 七、会话 token 放在哪

`sessionStorage`，不是 `localStorage`。契约里这串 token 的描述是
「能读全部订单与客户手机号、能改价、能发起退款——和支付密钥同一个量级」。
`localStorage` 会让它在这台机器上活满 7 天，任何一次 XSS 都能整个取走；
`sessionStorage` 关掉标签页就没了，刷新（F5）还在。

更好的形态是服务端下发 HttpOnly Cookie，但契约把 token 放在响应体里
（`StaffSession.token`）并要求 `Authorization: Bearer` —— 前端**没有**把它
藏进 HttpOnly 的办法。这是契约层面的事，不是界面能自己解决的。
