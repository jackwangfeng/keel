import 'package:flutter/foundation.dart';

import 'catalog.dart';
import 'client.dart';
import 'schema.g.dart';
import 'session.dart';

/// 购物车件数（tab 角标、详情页底栏）。加购 / 改数量的响应里带着整车，直接 set；
/// 其余时候 refresh 查一次。没登录是 0；查失败不打扰 —— 角标只是提示。
class CartCount extends ChangeNotifier {
  CartCount(this._c, this._session) {
    _session.addListener(_onSession);
  }
  final ApiClient _c;
  final Session _session;
  int n = 0;

  void set(int v) {
    if (v == n) return;
    n = v;
    notifyListeners();
  }

  Future<void> refresh() async {
    if (!_session.loggedIn) return set(0);
    try {
      final res = await _c.send('GET', '/cart', decode: (j) => Cart.fromJson(j as Map<String, dynamic>));
      set(cartQuantity(res.data));
    } on ApiFailure {
      // 角标只是提示。
    }
  }

  void _onSession() {
    if (!_session.loggedIn) {
      set(0);
    } else {
      refresh();
    }
  }

  @override
  void dispose() {
    _session.removeListener(_onSession);
    super.dispose();
  }
}
