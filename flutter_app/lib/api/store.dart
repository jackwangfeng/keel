import 'dart:async';

import 'package:flutter/foundation.dart';

import 'client.dart';
import 'locate.dart';
import 'schema.g.dart';

class CurrentStore {
  final int? storeId;
  final String name;
  const CurrentStore(this.storeId, this.name);
}

/// 「哪家店在服务你」。价格与在售范围都跟着门店走，列表 / 详情 / 购物车 / 结算用同一家。
/// 先定位（最多等 5 秒），带坐标问 /stores/resolve；拿不到坐标就不带 —— 服务端回落默认店。只解析一次、只在内存里
/// （定位会变：上周在公司解析出来的店，这周在家还用它就把围栏规则悄悄关掉了），换了服务地址重来。
class StoreService extends ChangeNotifier {
  StoreService(this._c, {Future<({double lat, double lng})?> Function()? locate}) : _locate = locate ?? locateDevice;
  final ApiClient _c;
  final Future<({double lat, double lng})?> Function() _locate;

  /// 定位最多等这么久：有的机器没开 GPS 时定位既不成功也不失败，就那么挂着 —— 首页要等门店，挂着就是白屏。
  static const locateTimeout = Duration(seconds: 5);

  Future<({double lat, double lng})?> _coords() async {
    try {
      // 先 then 成可空的 Future 再 timeout：传进来的函数运行时可能是 Future<坐标>（不可空），
      // 直接 timeout(onTimeout: () => null) 会因为泛型协变当场抛 TypeError，坐标就这么被吞了。
      var timedOut = false;
      final v = await _locate().then<({double lat, double lng})?>((v) => v).timeout(locateTimeout, onTimeout: () {
        timedOut = true;
        return null;
      });
      // 一行诊断（小程序 / 真机上看控制台就知道这次是按坐标还是回落默认店解析的）。
      debugPrint(v != null ? 'keel.store: 定位成功，按坐标解析门店' : 'keel.store: ${timedOut ? '定位超时' : '没有坐标'}，回落默认店');
      return v;
    } catch (e) {
      debugPrint('keel.store: 定位出错（$e），回落默认店');
      return null;
    }
  }
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
        final at = await _coords();
        final res = await _c.send('GET', '/stores/resolve',
            query: at == null ? null : {'lat': '${at.lat}', 'lng': '${at.lng}'},
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
