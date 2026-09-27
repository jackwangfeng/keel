import 'package:flutter/foundation.dart';

/// 底部 tab 的下标。tab 页在 IndexedStack 里一直活着，切回来时要自己刷新（购物车、订单）：
/// 页面听这个值，变成自己的下标就重读。
abstract final class Tabs {
  static const home = 0;
  static const cart = 1;
  static const orders = 2;
  static const me = 3;
  static final current = ValueNotifier<int>(home);
}
