import 'dart:async';

import 'package:flutter/foundation.dart';

import 'client.dart';
import 'schema.g.dart';

class CurrentStore {
  final int? storeId;
  final String name;
  const CurrentStore(this.storeId, this.name);
}

/// 「哪家店在服务你」。价格与在售范围都跟着门店走，列表 / 详情 / 购物车 / 结算用同一家。
/// 第一阶段不接定位（原生定位插件 mp-flutter 接管不了）：不带坐标问 /stores/resolve，走「回落默认店」。
class StoreService extends ChangeNotifier {
  StoreService(this._c);
  final ApiClient _c;
  CurrentStore? current;
  Future<CurrentStore>? _inflight;

  /// 换了服务地址：门店要重新解析。
  void reset() {
    current = null;
    _inflight = null;
    notifyListeners();
  }

  Future<CurrentStore> ensure() {
    final cur = current;
    if (cur != null) return Future.value(cur);
    return _inflight ??= () async {
      try {
        final res = await _c.send('GET', '/stores/resolve',
            decode: (j) => StoreResolveResult.fromJson(j as Map<String, dynamic>));
        final stores = res.data.stores;
        final s = res.data.matchType == 'none' || stores.isEmpty
            ? const CurrentStore(null, '')
            : CurrentStore(stores.first.id, stores.first.name);
        current = s;
        notifyListeners();
        return s;
      } finally {
        _inflight = null;
      }
    }();
  }
}
