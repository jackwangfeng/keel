/// e2e 专用买家的账号经 --dart-define 传入（tool/e2e_web.sh 从 ~/.config/keel/e2e.env 读出来再传），
/// 不写进仓库。不回退到演示买家：那是公开访客在用的账号，e2e 不去碰。
const e2ePhone = String.fromEnvironment('KEEL_E2E_PHONE');
const e2ePassword = String.fromEnvironment('KEEL_E2E_PASSWORD');

/// 后台前置状态（请服务端会话代做后把单号交过来）：没传的那几条用例跳过。
const fixtureRejectedRefund = String.fromEnvironment('KEEL_E2E_REJECTED_REFUND');
const fixtureRefundedRefund = String.fromEnvironment('KEEL_E2E_REFUNDED_REFUND');
const fixtureReturnRefund = String.fromEnvironment('KEEL_E2E_RETURN_REFUND');
const fixtureShippedOrder = String.fromEnvironment('KEEL_E2E_SHIPPED_ORDER');
