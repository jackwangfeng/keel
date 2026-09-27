# Flutter 买家端第二阶段（对齐 uni-app x 全部功能）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 把 uni-app x 买家端剩下的功能移植到 `flutter_app/`：领券中心 / 我的优惠券、消息中心（「我的」tab 未读角标）、个人资料（昵称 / 性别 / 第三方绑定）、服务地址设置、「我的」页全部入口、售后（申请、列表、详情、撤回、重新申请、寄回物流、凭证上传与查看）。做完后 Flutter 覆盖 uni-app x 的全部页面。

**Architecture:** 与第一阶段相同（契约字段只在 `lib/api/`，按领域分文件：`coupon.dart`、`notification.dart`、`profile.dart`、`refund.dart`）。行为的权威出处是 uni-app x 的 `app/src/pages/{coupon,me,refund,settings}/**` 与 `app/src/api/view.uts`、`client.uts`。

**Spec:** `docs/superpowers/specs/2026-09-27-flutter-buyer-app-design.md`（§1 第二阶段范围）

## Global Constraints

- 与第一阶段相同（页面闸门、服务端的金额、幂等键、登录页 push、e2e 专用买家、不碰公开访客的数据、后台前置状态请服务端会话代做）。
- 凭证选图是唯一需要平台能力的地方：小程序走 `mp_flutter_wechat` 的 `MpWechat.call('chooseMedia')` + `MpWechat.call('uploadFile')`；Web / 原生用 `image_picker`（Web 端是 `<input type=file>`，不碰原生）。加依赖后必须 `make flutter-build-mp` 仍能编过。

## Review Focus

1. 领券：领过又领完的显示「已领取」而不是「已领完」；重复点领取用同一个幂等键。
2. 未读角标：读一条 / 全部已读后角标跟着变；没登录清掉。
3. 售后申请：请求体不带金额；每行最多退「买的 − 已退 − 退款中」件；在途售后时不能再申请。
4. 寄回物流：只有退货退款且待买家退货（20）能填；409 刷新成现在的状态；改一次是新请求（换键）。
5. 凭证图要带令牌读（本人才能看）：不能直接 `Image.network`。

---

### Task 1: 优惠券（领券中心、我的优惠券）

- 移植：`view.uts` 的 `claimRow`、`couponRow`、`templateListQuery`、`couponListQuery`；`pages/coupon/center.uvue`、`pages/coupon/mine.uvue`（四个 tab：可用 / 锁定 / 已用 / 过期）。
- 测试：`test/coupon_test.dart`（按钮文案次序、剩余张数、有效期写法）。e2e：移植 `app/e2e/coupon.test.js` 前两条。

### Task 2: 消息中心、「我的」页、未读角标

- 移植：`notificationRow`、`notificationListQuery`；`pages/me/notifications.uvue`（分页、读一条、全部已读、按通知类型跳订单 / 售后）；`pages/me/index.uvue` 全部入口；`badge.uts` 的未读角标（挂在「我的」tab）。
- 测试：行模型、角标。e2e：移植 `app/e2e/notifications.test.js`。

### Task 3: 个人资料、服务地址设置

- 移植：`meView`、`meUpdateRequest`、`identityRow`；`pages/me/profile.uvue`（昵称、性别、第三方绑定解绑，last-credential 照实说）；`pages/settings/base.uvue`（运行时改服务地址，存本机，重启生效前提示）。
- 测试：行模型、请求体。

### Task 4: 售后

- 移植：`refundRow`、`refundDetailView`、`refundableItems`、`refundRequest`、`refundReturnAllowed`、`carriers`、`returnShipmentRequest`、`refundListQuery`、状态 / 类型 / 原因文案；`pages/refund/{apply,list,detail}.uvue`；订单详情加售后区与「申请售后」按钮；凭证上传（幂等键每张图一个）与带令牌读图。
- 测试：可退件数、请求体不带金额、状态文案。e2e：移植 `app/e2e/aftersale.test.js`（后台前置状态经 `KEEL_E2E_*` 传入，没有就跳过）。
