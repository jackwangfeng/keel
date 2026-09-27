import 'package:mp_flutter_wechat/mp_flutter_wechat.dart';

/// 小程序右上角的胶囊按钮（「···」「⊙」）不算进安全区：顶栏右侧的按钮（「管理」「搜索」「领券中心」…）
/// 要自己让出它那一块，不然被盖住点不到。返回要让出的宽度；不在小程序里（或拿不到胶囊位置）是 0。
Future<double> capsuleInset(double screenWidth) async {
  try {
    final r = await MpWechat.menuButtonRect();
    if (r == null || r.left <= 0 || r.left >= screenWidth) return 0;
    return screenWidth - r.left + 8;
  } catch (_) {
    return 0;
  }
}
