# 渠道接入：Shopify

把一家 Shopify 店接进 keel：商品从 Shopify 同步进来、库存与价格由 keel 推出去、Shopify 上的订单
进 keel 走正常的发货与售后流程。接入过程涉及 Shopify Partner / Dev Dashboard 的几步操作，
**由店铺的开发者或管理员完成**；接进 keel 之后由商家后台的操作员日常维护。

设计上的取舍见 [渠道适配层设计](./superpowers/specs/2026-10-02-channel-adapter-design.md)。

---

## 一、开店

1. 注册 [Shopify Partner](https://www.shopify.com/partners)（免费）。
2. 在 Partner Dashboard 建一家 **development store**（开发店，免费、不限时长，适合联调）；
   已经有正式店铺的话也可以直接用正式店，跳过这一步。

## 二、建应用、拿凭据

1. 去 <https://dev.shopify.com/dashboard> 建一个应用，装到上一步的店铺上。
2. 配置这些 **scope**（access scopes）：

   - `read_products` / `write_products`
   - `write_inventory`
   - `read_locations`
   - `read_orders` / `write_orders`
   - `read_merchant_managed_fulfillment_orders` / `write_merchant_managed_fulfillment_orders`

   > 最后一对是商家自管位置（非第三方履约服务）上发货的权限；Shopify 旧文档里常见的
   > `write_fulfillments` 是过时的写法，不要用它。

3. **加了 scope 之后必须发布新版本才会生效**：Dev Dashboard 里 Versions → 选中改动 → Release，
   然后回到店铺后台把应用**重新安装 / 更新**一次（已安装的应用不会自动拿到新 scope）。
   这一步很容易漏——联调时报 `ACCESS_DENIED` 或某个字段/操作莫名其妙不通，先回来检查有没有发布
   + 重装。
4. 从应用的 Overview 页拿到 **Client ID** 和 **Client Secret**，店铺域名记作 `xxx.myshopify.com`。

## 三、受保护客户数据（订单与收货人必需）

Dev Dashboard 本身**没有**申请受保护客户数据的入口，要去 **Partner Dashboard**：

1. <https://partners.shopify.com> → Apps → 选中这个应用。
2. 如果还没选过分发方式，先选 **Custom distribution**（自定义分发）。
3. 进 **API access requests** → **Protected customer data access** → Request access。
4. 勾选 **Protected customer data**，并勾选姓名、地址、电话、邮箱这几个字段（keel 读订单的收货人
   需要它们），按提示填 **Data protection details**（怎么存、怎么用、保留多久）。
5. 保存。**开发店不需要人工审核**，保存即生效；正式店是否需要审核以 Shopify 当时的提示为准。

没开这一步的症状很明确：拉订单时报错

```
This app is not approved to access the Order object
```

回来确认上面四步都做完，尤其是字段有没有勾全。

## 四、接入 keel

进商家后台「渠道」页（只有 `KEEL_CHANNELS` 开着、且账号是全店范围时才看得到账号管理；
门店范围的员工只看得到「订单」视图，见下面「日常使用」）。

### 1. 新建渠道账号

「渠道」→「账号」标签 →「新建渠道账号」，填：

| 字段 | 填什么 |
|---|---|
| 渠道 | Shopify |
| 店铺账号 | `xxx.myshopify.com`（建好之后不能改，换店铺要新建一个账号） |
| 名称 | 后台里看的名字，随便起，如「Shopify 海外店」 |
| 角色 | 勾「商品源」+「销售渠道」（不勾「库存源」——那是给 ERP 用的，Shopify 场景下 keel 是库存权威） |
| 默认类目 | **必填**，首拉商品时新商品挂在哪个类目下 |
| 价格源门店 | 可选，渠道上全店一个价时价格从哪家门店出；不选就用门店映射里 id 最小的那家 |
| 回调地址前缀 | keel 对外可访问的地址，如 `https://shop.example.com`；首次拉商品时按它在 Shopify 上装回调订阅，留空则不装 |

新建的账号一律是**停用**状态。保存后进详情页继续配置。

### 2. 配置凭据

详情页「配置凭据」，填 Client ID / Client Secret。**凭据只写不回显**——保存之后页面上看不到
之前填的值，要换凭据直接覆盖填新的。

### 3. 门店映射

「门店映射」标签：把 keel 的门店和 Shopify 的 location 一一对应（keel 门店 ↔ 外部门店 ID，
即 Shopify 的 Location ID）。没映射的 location 来的订单会标异常，见下面「排错」。

### 4. 库存规则与价格规则

- **库存规则**（「库存规则」标签）：按渠道 / 门店 / SKU 三层设置**可售比例**（对 keel 可售数打折后
  再推给渠道）、**安全库存**（先扣掉再推，防止压线超卖）、**上限**（0 = 在这个渠道下架这个 SKU）。
- **价格规则**（「价格规则」标签）：渠道级或 SKU 级的**加价**（百分比，负数相当于打折）或 **固定价**
  （优先于加价）。

不设规则时按 keel 该门店的有效价与可售数原样推送。

### 5. 启用

回到详情页，点「启用」。启用的前提是账号本身准备好（有凭据，必要的映射与规则不缺，界面会提示
「还没准备好启用」并列出缺什么）。启用之后：

- **首拉商品**：新 SKU 以 Shopify 当前的库存数作为初始库存（不会把 keel 现有库存覆盖成 0，
  第一次推送是空操作而不是清零）；货号已经在 keel 里存在的，直接认领（adopt）成同一个 SKU，
  不会重复建。
- 由 Shopify 管理的商品字段（标题、描述、图片、规格）在 keel 侧**锁定**，后台改这些字段会被拒绝
  （`409 managed-by-channel`），回填同样的值不算改动；账号停用后锁定解除。
- 如果填了回调地址前缀，这一步会顺带在 Shopify 上装好商品、库存与**订单**相关的 webhook 订阅。

### 已有账号升级：补装订单回调

如果这个账号是渠道二期（只有商品 / 库存 / 价格）就建好的，三期上线后订单功能不会自动生效——
进详情页点「重新同步商品」，会按当前 `webhook_base_url` 重新核对并补装缺的回调订阅（包括订单
相关的），202 表示已经排进后台队列。

---

## 五、日常使用

**订单进来**：Shopify 上**顾客付款成功**（`PAID` / 后续部分退款）的订单才会进 keel；还在
`PENDING`/`AUTHORIZED`/`PARTIALLY_PAID`（授权未扣款）的订单当作未付款，不会提前接单。
Shopify 渠道不需要人工接单，收到就自动生成 keel 订单（已支付状态），扣同一份库存，进现有的
发货队列。

缺货或门店 / SKU 映射不全时渠道单会**标异常**，不会生成 keel 订单：补齐库存或映射之后，在
「渠道」→「订单」里对这一单点**「重试」**。

**在 keel 发货**：照常在订单详情页发货、填物流单号；keel 会把单号回传 Shopify
（`fulfillmentCreate`），顾客能在 Shopify 上收到发货通知。

**Shopify 上的动作会同步到 keel**：顾客 / 商家在 Shopify 上取消订单、退款、标记发货，都会
转成 keel 订单上对应的动作（取消、退款、发货）。

**门店范围的员工**（非全店范围）在「渠道」页只看得到「订单」视图，只能看、处理自己门店的渠道单
（接单/拒单、申请决定）；账号、凭据、映射、规则仍然只有全店范围的账号能看和改。

---

## 六、已知限制

- **币种不换算**：金额按分原样记，Shopify 店铺币种应当与 keel 商家的记账币种一致（当前实测
  开发店是 USD）。
- **税**：价外税的店，税额不进 keel 订单（记在渠道单的 `tax_cents` 上，平台总价 = keel 实付 +
  税）；价内税的店，keel 实付 = 顾客实际支付的总价。
- **一单分到多个门店**：Shopify 一张订单在发货层面被分给多个 location（或没有有效的发货单）时，
  keel 侧标异常、**不拆单**，需要人工处理。
- **订单编辑**：Shopify 的订单编辑（加行、减行）只留档，不自动改 keel 订单，由人工看异常处理。

---

## 七、排错

| 现象 | 原因 | 怎么处理 |
|---|---|---|
| `This app is not approved to access the Order object` | 受保护客户数据没开 | 见「三、受保护客户数据」，到 Partner Dashboard 申请并勾全字段 |
| 加了 scope 但发货 / 读单还是不通 | 只在 Dev Dashboard 改了 scope，没发布新版本，或发布了但店铺里没重装 | Versions → Release，再去店铺重新安装 / 更新应用 |
| 渠道单异常：「门店：平台门店 xxx 没有映射到 keel 门店」 | Shopify 的 location 还没在「门店映射」里对应 keel 门店 | 补上映射后点「重试」 |
| 渠道单异常：「第 N 行…没有链到 keel 的 SKU」 | 这一行的 Shopify 变体货号在 keel 里还没有对应 SKU，首拉时没能认领 | 在 keel 建好同货号的 SKU，或核对货号是否一致，再「重试」 |
| 渠道单异常：「缺货：… 要 N 件（可售 M）」 | 下单时 keel 侧库存不够扣 | 补库存后点「重试」 |
| 渠道单异常：「门店：平台没给出这张单的发货门店」/ 「平台门店分到了多个 location」 | 见「已知限制」里的一单多门店 | 人工核实这张单该在哪个门店处理，不支持自动拆单 |
| 商品字段改不动，报 `409 managed-by-channel` | 这个 SKU 的目录由 Shopify 管理，字段锁定 | 去 Shopify 改，keel 这边会同步过来；要解锁就停用这个渠道账号 |
| 升级后订单回调没生效 | 账号是旧版本建的，没补装订单相关 webhook | 账号详情页点「重新同步商品」 |
| 门店员工在「渠道」页看不到账号 / 规则 | 按设计：账号、凭据、映射、规则只对全店范围开放 | 用全店范围的账号登录配置，门店员工只处理「订单」 |
