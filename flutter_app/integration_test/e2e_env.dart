/// e2e 专用买家的账号经 --dart-define 传入（tool/e2e_web.sh 从 ~/.config/keel/e2e.env 读出来再传），
/// 不写进仓库。不回退到演示买家：那是公开访客在用的账号，e2e 不去碰。
const e2ePhone = String.fromEnvironment('KEEL_E2E_PHONE');
const e2ePassword = String.fromEnvironment('KEEL_E2E_PASSWORD');
