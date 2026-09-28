// Package problem 按 RFC 9457 写错误响应。
//
// 单独成包，是因为写 Problem 的地方跨了两层：handler 要写（业务错误），
// 租户解析中间件也要写（解析不到商家的 404）。中间件在 internal/tenant 里，
// 而 tenant 被 repository 依赖、repository 被 handler 依赖 ——
// 让 tenant 去 import handler 会成环。
//
// 为什么中间件那条也必须写 body：契约里 /products 的响应集合只有 `200` 和
// `default: $ref Problem`。一个 Content-Length: 0 的 404 不在这个集合里，
// 按契约生成的客户端会拿到一个解析不出来的响应 —— 而 404 恰恰是它最需要读懂
// 的那一个（「这家店不存在」和「服务挂了」得区分得开）。
package problem

import (
	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// 目前用到的 problem type。它们是 URI 形式的稳定标识，客户端按它分支，
// 所以改一个等于改契约的一部分 —— 不要顺手改措辞。
const (
	TypeNotFound         = "https://keel.dev/problems/not-found"
	TypeMethodNotAllowed = "https://keel.dev/problems/method-not-allowed"
	TypeInternal         = "https://keel.dev/problems/internal"

	// 鉴权相关的四个。它们分得这么细，是因为客户端对它们的处置**各不相同**：
	//
	//   unauthorized         → 重新登录
	//   token-expired        → 去 /auth/refresh，不必打扰用户
	//   token-tenant-mismatch→ 这串令牌不属于本店；客户端该丢掉它，
	//                          而运维该知道有人在跨店用令牌
	//   account-disabled     → 封禁 / 注销，重新登录也没用
	//
	// 全都压成一个 unauthorized 的话，客户端只能靠猜，而猜错的那一半会
	// 把用户踢回登录页；跨店那一条更是会淹没在一片普通的鉴权失败里。
	TypeUnauthorized        = "https://keel.dev/problems/unauthorized"
	TypeTokenExpired        = "https://keel.dev/problems/token-expired"
	TypeTokenTenantMismatch = "https://keel.dev/problems/token-tenant-mismatch"
	TypeAccountDisabled     = "https://keel.dev/problems/account-disabled"

	// 请求体本身不合法（缺字段、两种凭据都给了或都没给）。
	TypeInvalidRequest = "https://keel.dev/problems/invalid-request"

	// 下单链路的四个。它们同样分得细，因为客户端对它们的处置各不相同：
	//
	//   insufficient-stock        → 让用户改数量或换商品
	//   price-changed             → 重新试算再提交（**不要**直接重试原请求）
	//   idempotency-key-in-flight → 按 Retry-After 退避重试，**不是**业务失败，
	//                               不该弹窗，更不该让用户再点一次「提交订单」
	//   idempotency-key-reused    → 客户端自己的 bug：同一把钥匙配了两个请求体
	//
	// 前两个都是 409，后两个一个 409 一个 422 —— 只看状态码分不开，而
	// 「退避重试」与「让用户改购物车」是完全相反的动作。
	//
	// 契约里这几个写的是 https://errors.example.com/... 那个示例域名；
	// 本仓库统一用 keel.dev（见本常量块开头那句话：改一个等于改契约的一部分，
	// 所以也不顺手去改那几个）。这处不一致已在报告里列为 defer。
	TypeInsufficientStock      = "https://keel.dev/problems/insufficient-stock"
	TypePriceChanged           = "https://keel.dev/problems/price-changed"
	TypeIdempotencyKeyInFlight = "https://keel.dev/problems/idempotency-key-in-flight"
	TypeIdempotencyKeyReused   = "https://keel.dev/problems/idempotency-key-reused"

	// 发起支付那条接口上的业务冲突。契约里 POST /orders/{order_no}/payments 的
	// 409 描述逐字写着这个 type：「订单当前状态不允许支付（非 10 待支付，
	// 或已超时关闭）」。
	//
	// 它与 insufficient-stock / price-changed 同为 409，而客户端的处置完全不同：
	// 这一个要刷新订单（这一单可能已经付过了，再付一次是重复付款），
	// 那两个要改购物车。只看状态码分不开。
	TypeOrderStatusNotPayable = "https://keel.dev/problems/order-status-not-payable"

	// 订单后半程（取消、确认收货、发货、售后）的业务冲突，逐字取自契约各条接口的
	// 409 / 404 / 422 描述。全是「状态不对」一类，而客户端的处置各不相同：
	//
	//   order-status-not-cancelable   409  只有待支付能取消。刷新订单（多半刚付完或刚超时）
	//   order-status-not-confirmable  409  只有已发货能确认收货
	//   order-status-not-shippable    409  后台：非已支付，或已发过货
	//   order-has-pending-full-refund 409  后台：先去审那张整单退款单，再谈发货
	//   tracking-no-duplicated        409  后台：运单号录重了，不是新包裹
	//   order-not-found               404  申请退款时订单不存在或不是你的（契约在这一条上
	//                                      点名了这个 type，别的 404 仍是通用的 not-found）
	//   order-status-not-refundable   409  待支付 / 已关闭 / 整单退款中的订单不能申请售后
	//   refund-quantity-exceeded      409  退的件数超过「购买 - 已退」
	//   refund-already-in-progress    409  这一行已经在一张进行中的退款单里
	//   order-item-mismatch           422  order_item_id 不属于这一单
	//   refund-status-not-cancelable  409  只有待审核 / 待买家退货能撤回
	//   refund-status-not-auditable   409  只有待审核能审
	//   refund-status-not-receivable  409  只有待买家退货能确认收到退货
	//   refund-status-not-returnable  409  买家填寄回物流：只有退货退款、且在待买家退货
	//   refund-freight-exceeded       422  审核裁定的退运费超过订单实收运费
	TypeOrderStatusNotCancelable  = "https://keel.dev/problems/order-status-not-cancelable"
	TypeOrderStatusNotConfirmable = "https://keel.dev/problems/order-status-not-confirmable"
	TypeOrderStatusNotShippable   = "https://keel.dev/problems/order-status-not-shippable"
	TypeOrderHasPendingFullRefund = "https://keel.dev/problems/order-has-pending-full-refund"
	TypeTrackingNoDuplicated      = "https://keel.dev/problems/tracking-no-duplicated"
	TypeOrderNotFound             = "https://keel.dev/problems/order-not-found"
	TypeOrderStatusNotRefundable  = "https://keel.dev/problems/order-status-not-refundable"
	TypeRefundQuantityExceeded    = "https://keel.dev/problems/refund-quantity-exceeded"
	TypeRefundAlreadyInProgress   = "https://keel.dev/problems/refund-already-in-progress"
	TypeOrderItemMismatch         = "https://keel.dev/problems/order-item-mismatch"
	TypeRefundStatusNotCancelable = "https://keel.dev/problems/refund-status-not-cancelable"
	TypeRefundStatusNotAuditable  = "https://keel.dev/problems/refund-status-not-auditable"
	TypeRefundStatusNotReceivable = "https://keel.dev/problems/refund-status-not-receivable"
	TypeRefundStatusNotReturnable = "https://keel.dev/problems/refund-status-not-returnable"
	TypeRefundFreightExceeded     = "https://keel.dev/problems/refund-freight-exceeded"

	// 后台身份那四个（数据模型 §14 / 契约 AdminAuth 与 Admin 两个 tag）。
	//
	// 它们同样分得细，理由与上面那几组一字不差 —— 契约里这四种全都是
	// 403 或 409，而只看状态码分不开，但客户端与运维对它们的处置完全不同：
	//
	//   bootstrap-closed   → 这个部署已经引导过了；别再拿引导 token 试，
	//                        走邮箱链接。这是个**正常**状态，不是故障。
	//   staff-forbidden    → 你是操作员不是管理员；换个人来做这件事。
	//   staff-email-taken  → 换个邮箱（或者那个人已经在了）。
	//   last-admin         → 先加一个管理员，再来降级 / 停用这一个。
	//                        数据模型 §14 那条进不了数据库的约束。
	//
	// last-admin 与 staff-email-taken 都是 409 且都出现在同一条 PATCH /
	// POST 上，压成一个之后前端只能把两句完全不同的话写成一句。
	//   platform-only      → 你是某一家店的管理员，而这件事只有平台级能做。
	//                        与 staff-forbidden 分开：那一个是「你不是管理员」，
	//                        这一个是「你是管理员，但你是那一层的」。压成一个
	//                        之后，商家老板调开店接口会收到「需要管理员权限」，
	//                        一句让他去检查自己角色的假话。
	//   merchant-code-taken→ 换一个 code。它是全局唯一的（tenancy.json 的
	//                        unique_global_ok：它就是租户标识本身）。
	TypeBootstrapClosed   = "https://keel.dev/problems/bootstrap-closed"
	TypeStaffForbidden    = "https://keel.dev/problems/staff-forbidden"
	TypeStaffEmailTaken   = "https://keel.dev/problems/staff-email-taken"
	TypeLastAdmin         = "https://keel.dev/problems/last-admin"
	TypePlatformOnly      = "https://keel.dev/problems/platform-only"
	TypeMerchantCodeTaken = "https://keel.dev/problems/merchant-code-taken"
	// staff-disabled → 给一个停用的员工重签登录 token。先启用再签：签给他也
	//                  换不出会话（停用账号的一次性 token 在兑换时被拒）。
	TypeStaffDisabled = "https://keel.dev/problems/staff-disabled"

	// 同一租户内的分级权限（v0.1.0，internal/service/authz.go）。两个 type
	// 而不是一个，理由与 staff-forbidden / platform-only 分开报一样：
	//   role-forbidden → 你这个角色做不了这类事（门店管理员建员工、大区管理员
	//                    建大区、操作员设默认门店、任何人改自己的角色）。
	//                    该去找你的上级。
	//   out-of-scope   → 这类事你能做，但这一个不归你管（华北的大区管理员改
	//                    华东的门店）。该去找管那个大区 / 门店的人。
	// 压成一个之后，界面只能说一句「没有权限」，而被拒的人不知道该找谁。
	TypeRoleForbidden = "https://keel.dev/problems/role-forbidden"
	TypeOutOfScope    = "https://keel.dev/problems/out-of-scope"

	// 商家写路径那一组（M4，契约 Admin + Catalog 两个 tag 的 16 条）。
	//
	// 它们**全部是 404 或 409 或 422**，而只看状态码分不开 —— 契约在每一条
	// 端点上都写着「按 Problem type 区分」。分得细的理由与上面那几组同构：
	// 客户端对它们的处置完全不同，而这里有两处混掉会直接造出无限重试：
	//
	//   inventory-precondition-failed → 刷新那一格（响应体里带 current）再试一次，
	//                                   **会成功**
	//   （对照）404 SKU 不在本租户    → 重试**永远**不会成功
	//
	//   product-deleted               → 这件商品已经软删，改它没有意义
	//   product-still-published       → 先下架再删（两步都是调用方做得到的）
	//   product-has-no-sku            → 先加一个 SKU 再上架
	//   sku-code-duplicated           → 换一个货号
	//   sku-last-of-published-product → 先下架商品，或者先加一个兄弟规格
	//   category-has-children /
	//   category-has-products         → 先把子分类 / 商品挪走，再从叶子往上删
	//   category-cycle                → 目标父节点是自己的后代，换一个
	//   upload-not-found /
	//   upload-wrong-purpose /
	//   product-image-duplicated      → 这三条都是 422，客户端要改的东西各不相同
	//
	// 逐字对着契约里那几段 description 里写出来的 URI，不要顺手改措辞。
	TypeProductDeleted        = "https://keel.dev/problems/product-deleted"
	TypeProductStillPublished = "https://keel.dev/problems/product-still-published"
	TypeProductHasNoSKU       = "https://keel.dev/problems/product-has-no-sku"
	TypeSKUCodeDuplicated     = "https://keel.dev/problems/sku-code-duplicated"
	TypeSKULastOfPublished    = "https://keel.dev/problems/sku-last-of-published-product"
	TypeInventoryPrecondition = "https://keel.dev/problems/inventory-precondition-failed"
	// inventory-insufficient 是相对调整（POST .../inventory/adjustments）扣完会变负时的 409。
	// 与上一条共用 409 与 InventoryConflict 响应体（都带 current），但处置相反：
	// 上一条重读重试**会**成功，这一条原样重试**不会** —— 要改小扣减量或先补货。
	// 也不复用买家那条 insufficient-stock：那一个的响应体里没有 current，
	// 语境是「这单买不了」，不是「这次调整做不了」。
	TypeInventoryInsufficient = "https://keel.dev/problems/inventory-insufficient"
	// inventory-unavailable 是 503：库存服务此刻没回答（只在拆分部署下出现，微服务拆分阶段 1a）。
	// 读：取决于水位的接口（商品详情、购物车、后台商品 / SKU / 门店库存页、库存预警）答不出来；
	// 写：库存那一笔**可能已经生效也可能没有** —— 客户端原样重试（带同一个 Idempotency-Key），
	// 服务端保证重试不会多加一遍。与 internal 的 500 分开：这不是谁写错了代码，是一个会自己
	// 好的依赖暂时不在，503 + 退避重试是它在 HTTP 上的准确说法。
	TypeInventoryUnavailable = "https://keel.dev/problems/inventory-unavailable"
	TypeUploadNotFound       = "https://keel.dev/problems/upload-not-found"
	// upload-forbidden 是**读**那条路上的 403：文件在、也属于这家店，
	// 但它的 purpose 不是「所有人可读」的那一类（典型：别人的退款凭证）。
	// 契约在 GET /uploads/{upload_id} 上明写这里不能用 404 掩盖存在性。
	// 与 upload-not-found 分开：那一个合并了「不存在」与「是别家店的」，
	// 因为 upload id 是全局自增的 —— 两条的掩盖策略刚好相反，
	// 压成一个之后必然有一条是错的。
	TypeUploadForbidden        = "https://keel.dev/problems/upload-forbidden"
	TypeUploadWrongPurpose     = "https://keel.dev/problems/upload-wrong-purpose"
	TypeProductImageDuplicated = "https://keel.dev/problems/product-image-duplicated"
	TypeCategoryHasChildren    = "https://keel.dev/problems/category-has-children"
	TypeCategoryHasProducts    = "https://keel.dev/problems/category-has-products"
	TypeCategoryCycle          = "https://keel.dev/problems/category-cycle"
	TypeUploadTooLarge         = "https://keel.dev/problems/upload-too-large"
	TypeUploadUnsupportedMedia = "https://keel.dev/problems/upload-unsupported-media-type"

	// 合规检查那两条（M4 阶段 2，商品理解服务设计 §2）。
	//
	// 它们分成两个 type，而且状态码也不同（422 / 503），因为商家要做的事
	// 完全相反：
	//
	//   compliance-rejected    → 你的文案里有违禁词，**改了再来**。
	//                            errors[] 里逐条写着哪个字段第几个字
	//                            （契约的 FieldError）。重试原请求永远失败。
	//   compliance-unavailable → 我们没查出来，**过一会儿原样再试一次**。
	//                            你的文案可能一个字都不用改。
	//
	// 压成一个的话，客户端只能把「改文案」和「退避重试」写成同一段逻辑，
	// 而它们一个是死路一个是活路。
	//
	// **unavailable 是 503 而不是 500**：这一条是全系统唯一一处「宁可误拒」
	// （§7 的降级表 / docs/ai-capabilities.md 的三条纪律第三条）——
	// 它不是服务端出 bug，是我们主动选择在答不出来时拒绝。503 + Retry-After
	// 是这件事在 HTTP 上的准确说法，而 500 会让客户端以为有人写错了代码。
	TypeComplianceRejected    = "https://keel.dev/problems/compliance-rejected"
	TypeComplianceUnavailable = "https://keel.dev/problems/compliance-unavailable"

	// 门店 / 大区那一组（00020，契约 Store tag 的 23 条）。
	//
	// 和上面那一组同一条理由：状态码分不开它们，契约在每条端点上都写着
	// 「按 type 区分」。这一组里有**三对**混掉之后会让客户端做错事：
	//
	//   store-code-conflict     → 换一个门店编号，重试**会**成功
	//   default-store-conflict  → 已经有一家默认店了，要切换得换一条端点
	//                             （PUT /admin/stores/{id}/default），
	//                             在 POST 上重试**永远**不会成功
	//
	//   store-fence-required    → 这家店不是默认店，清空围栏会让它永远接不到单；
	//                             调用方要做的是先把它设成默认店，或者别清
	//   invalid-fence           → 多边形本身画错了（自交、未闭合、顶点不足）。
	//                             detail 里转述 PostGIS 的 ST_IsValidReason，
	//                             那句话是运营唯一能拿来定位自己画错在哪儿的东西
	//
	//   inventory-precondition-failed → 刷新那一格再试，**会**成功
	//   store-ambiguous               → 本租户门店数不是 1，「这个 SKU 的库存」
	//                                   没有唯一答案。重试没有用，
	//                                   要换 /admin/stores/{id}/skus/{id}/inventory 那条路径。
	//                                   **它和上一条共用 409 是契约刻意定的**：
	//                                   两者都是「服务端现在的状态与你的假设不符」，
	//                                   type 已经把差别说清楚，状态码不必再分一次。
	//
	// sku-not-sold-in-store 刻意是 422 而不是 409：409 在本契约里被客户端读成
	// 「重读一次再试」（Retry-After、InventoryConflict.current 都在教它这么读），
	// 而「这家店不卖这件商品」重试永远不会成功 —— 客户端该做的是换一家店。
	TypeRegionCodeConflict   = "https://keel.dev/problems/region-code-conflict"
	TypeRegionHasStores      = "https://keel.dev/problems/region-has-stores"
	TypeStoreCodeConflict    = "https://keel.dev/problems/store-code-conflict"
	TypeDefaultStoreConflict = "https://keel.dev/problems/default-store-conflict"
	TypeStoreFenceRequired   = "https://keel.dev/problems/store-fence-required"
	TypeStoreUnavailable     = "https://keel.dev/problems/store-unavailable"
	TypeStoreAmbiguous       = "https://keel.dev/problems/store-ambiguous"
	TypeInvalidFence         = "https://keel.dev/problems/invalid-fence"
	// store-location-required / store-outside-fence：门店必须有坐标，有围栏时门店必须在围栏内
	// （2026-09-27）。都是 422：不改请求重试永远不会成功 —— 前者要先选点，后者要挪点或重画围栏。
	// proposal-not-open：AI 员工的提案已经处理过（已执行 / 已驳回）或已过期，不能再批准 / 驳回（AI 经营 M9）。
	TypeProposalNotOpen       = "https://keel.dev/problems/proposal-not-open"
	TypeStoreLocationRequired = "https://keel.dev/problems/store-location-required"
	TypeStoreOutsideFence     = "https://keel.dev/problems/store-outside-fence"
	TypeSKUNotSoldInStore     = "https://keel.dev/problems/sku-not-sold-in-store"

	// 优惠券那一组（数据模型 §7，契约 Coupon tag）。分得细的理由同上：
	//
	//   coupon-not-applicable     409  下单 / 试算带的券本单用不了。客户端换一张或不用券
	//   coupon-sold-out           409  领完了（或定向发放剩余不够整批）。重试没有用
	//   coupon-claim-limit-reached 409 这个买家已达每人限领。按钮该变灰
	//   coupon-claim-ended        409  绝对时间模式下活动已结束
	//   coupon-template-locked    409  已发出过券，券面与范围不能再改。后台该提示新建一批
	//   coupon-template-disabled  409  定向发放时模板已停用
	//
	// 全是 409、处置各不相同，只看状态码分不开。
	TypeCouponNotApplicable     = "https://keel.dev/problems/coupon-not-applicable"
	TypeCouponSoldOut           = "https://keel.dev/problems/coupon-sold-out"
	TypeCouponClaimLimitReached = "https://keel.dev/problems/coupon-claim-limit-reached"
	TypeCouponClaimEnded        = "https://keel.dev/problems/coupon-claim-ended"
	TypeCouponTemplateLocked    = "https://keel.dev/problems/coupon-template-locked"
	TypeCouponTemplateDisabled  = "https://keel.dev/problems/coupon-template-disabled"

	// 营销活动那一组（数据模型 §7「营销活动」，契约 Promotion tag）：
	//
	//   promotion-limit-exceeded  409  限时折扣 / 秒杀超出每人限购。客户端减数量
	//   promotion-sold-out        409  秒杀配额在试算之后被抢光。客户端重新试算（会按门店价报价）
	//   promotion-online          409  后台想改上线中活动的规则。先下线再改
	TypePromotionLimitExceeded = "https://keel.dev/problems/promotion-limit-exceeded"
	TypePromotionSoldOut       = "https://keel.dev/problems/promotion-sold-out"
	TypePromotionOnline        = "https://keel.dev/problems/promotion-online"

	// 运费那一组（数据模型 §7「运费模板」「运费怎么算」，00055 / 00056）：
	//
	//   region-not-deliverable     422  试算 / 下单时有商品送不到这个收货地址。响应体的
	//                                   undeliverable_items 逐行给出 SKU 与原因；**不是 409**：
	//                                   重试不会成功，客户端该去掉那几行或换地址
	//   freight-template-conflict  409  这家门店已经有门店模板（每店至多一个）
	//   freight-template-in-use    409  还有商品挂着这个模板（删除、或改成门店模板时）
	TypeRegionNotDeliverable    = "https://keel.dev/problems/region-not-deliverable"
	TypeFreightTemplateConflict = "https://keel.dev/problems/freight-template-conflict"
	TypeFreightTemplateInUse    = "https://keel.dev/problems/freight-template-in-use"

	// 买家侧地址簿 / 购物车 / 个人信息那三组（契约 User 与 Cart tag）。
	// 三个名字都是契约里早就写好的，这里只是第一次有人发它们：
	//
	//   use-default-endpoint     422  想用 PUT /addresses/{id} 切换默认地址。客户端该改调
	//                                 PUT /addresses/{id}/default，而不是改表单再提交
	//   cart-quantity-exceeded   422  加购累加后超过 999。detail 给出当前数量与上限，
	//                                 客户端据此提示「最多还能加几件」，**不静默截断**
	//   last-credential          409  解绑之后账号就没有任何可登录的凭据了。客户端该先
	//                                 引导用户设密码或绑别的身份
	TypeUseDefaultEndpoint   = "https://keel.dev/problems/use-default-endpoint"
	TypeCartQuantityExceeded = "https://keel.dev/problems/cart-quantity-exceeded"
	TypeLastCredential       = "https://keel.dev/problems/last-credential"

	// 契约声明了、本轮刻意没有实现的路径。用一个**专门的** type 而不是复用
	// internal：客户端能据此分辨「这个功能还没有」与「服务器炸了」，
	// 而这两件事的重试策略完全相反。
	TypeNotImplemented = "https://keel.dev/problems/not-implemented"

	// 被限流挡住。契约里已经有这个 type（POST /auth/sms-code 的 429 描述
	// 逐字写着它），所以这里复用，不新造一个 —— 同一件事两个 type，
	// 客户端的退避逻辑就要写两遍。
	//
	// 429 配 Retry-After：契约在那条接口上把这个头写进了响应定义
	// （「建议退避秒数」）。没有它的话，客户端能做的只有立刻重试，
	// 而那正好是限流要挡的行为。
	TypeRateLimited = "https://keel.dev/problems/rate-limited"

	// 商家管理与平台级租户切换（契约 components/parameters/KeelMerchant、
	// /admin/merchants*）。三个都得按 type 分，因为它们各自要客户端做的事相反：
	//
	//   tenant-switch-forbidden → 商家级员工带了 X-Keel-Merchant。**不生效、不静默忽略**：
	//                             静默忽略的话，一个以为自己切过去了的客户端会往
	//                             自己的店里写本该写给别家的数据。客户端该做的是
	//                             别带这个头。403。
	//   unknown-merchant        → 头里的 code 不存在或已软删。**不回落**到 Host 那家——
	//                             回落意味着运营以为在管 B 店，实际改的是 A 店。
	//                             422：头不是路径，按本契约的分法「请求其余部分
	//                             指名的东西不存在」是 422 而不是 404。
	//   single-merchant-mode    → 单商家部署（KEEL_DEFAULT_MERCHANT）里开店，或停用
	//                             那唯一一家店。重试、换 code 都没有用，要改的是部署形态。
	//                             409。
	TypeTenantSwitchForbidden = "https://keel.dev/problems/tenant-switch-forbidden"
	TypeUnknownMerchant       = "https://keel.dev/problems/unknown-merchant"
	TypeSingleMerchantMode    = "https://keel.dev/problems/single-merchant-mode"

	// 商品批量导入（契约 /admin/product-imports）。与上传商品图那两条分开命名，
	// 因为上限不同（5 MB 对 10 MB）、客户端要做的事也不同：
	//
	//   import-file-too-large      → 拆成多个文件。413。
	//   import-unsupported-format  → 另存为 xlsx 或 csv（老 .xls、加密工作簿）。415。
	//   import-file-invalid        → 整份文件不成立（缺列、超行数、表头合并……），
	//                                errors[] 逐条列原因。改文件再传。422。
	//   import-nothing-to-import   → 没有一件商品能导入（全都有错或都没选类目）。
	//                                回预检结果去改。422。
	TypeImportFileTooLarge    = "https://keel.dev/problems/import-file-too-large"
	TypeImportUnsupportedFmt  = "https://keel.dev/problems/import-unsupported-format"
	TypeImportFileInvalid     = "https://keel.dev/problems/import-file-invalid"
	TypeImportNothingToImport = "https://keel.dev/problems/import-nothing-to-import"
)

// Write 写一个 RFC 9457 响应并中止后续 handler。
//
// 刻意只收 type 和 title，不收 detail：detail 是给人看的自由文本，而这里的
// 调用点全都在匿名可访问的路径上，err 的内容里常常带着表名、列名和参数值。
// 真要加 detail，加的是一句人写的话，不是 err.Error()。
func Write(c *gin.Context, status int, kind, title string) {
	// 先设 Content-Type：gin 的 JSON 渲染只在它还没被设过时才写自己那个
	// application/json，所以顺序反了的话 problem+json 会被吃掉。
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(status, api.Problem{
		Type:   kind,
		Title:  title,
		Status: status,
	})
}

// WriteValue 写一个**带扩展成员**的 problem 响应。
//
// RFC 9457 允许 problem 对象带扩展成员，而契约里真的用了一处：
// PUT /admin/skus/{sku_id}/inventory 的 409 回的是 InventoryConflict ——
// Problem 加一个必填的 current。Write 那个函数只收 type / title / status，
// 表达不了它。
//
// 收 any 而不是给 Write 加一个 extras map：扩展的形状由契约决定（InventoryConflict
// 是一个有具体字段的 schema），而 map 会让调用方自己拼键名，
// 那正是「响应体用生成类型，不手写结构体」要挡的东西 —— 调用方传进来的
// 应当是 api.InventoryConflict 本身。
//
// Content-Type 与 Write 走同一句：两处各写一遍的话，分叉时的症状是某一条
// 错误响应的 Content-Type 退回 application/json，而按契约生成的客户端
// 会在它最需要读懂的那类响应上走错分支。
func WriteValue(c *gin.Context, status int, body any) {
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(status, body)
}
