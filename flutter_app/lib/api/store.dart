import 'dart:async';

import 'package:flutter/foundation.dart';

import 'client.dart';
import 'geo.dart';
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
///
/// 送货位置（POI）：默认是定位到的那一点，解析完后台再问一次逆地理编码，拿到「送至 …」的地址名（没配地图服务商就空着，
/// 首页退回显示门店）。用户换了地址（搜索地点 / 地图选点 / 收货地址）就 [deliverTo]：按它的坐标重新解析，不再定位；
/// 没有坐标的地址不带坐标解析（默认店）。同样只在内存里，下次打开重新定位。
class StoreService extends ChangeNotifier {
  StoreService(this._c, {Future<({double lat, double lng})?> Function()? locate, Future<String> Function(double lat, double lng)? reverse})
      : _locate = locate ?? locateDevice,
        _reverse = reverse;
  final ApiClient _c;
  final Future<({double lat, double lng})?> Function() _locate;
  final Future<String> Function(double lat, double lng)? _reverse;

  /// 「送至 …」的那一截。空 = 还不知道 / 没配地图服务商。
  String placeLabel = '';

  /// 这次解析门店用的坐标（搜索地点时按离它近的排）。
  LatLng? at;

  /// 用户选定的送货位置；null = 用定位。里面的 at 为 null 是「选了一条没坐标的地址」。
  ({LatLng? at})? _pinned;
  // 换过地址 / 服务地址之后，之前发出去的逆地理编码回来了也不认。
  int _gen = 0;

  /// 送到这里：按它的坐标重新解析门店（首页听到 current 作废会重拉）。
  void deliverTo({required LatLng? at, required String label}) {
    _pinned = (at: at);
    placeLabel = label;
    _invalidate();
  }

  /// 改回用当前定位。
  void useDeviceLocation() {
    _pinned = null;
    placeLabel = '';
    _invalidate();
  }

  void _invalidate() {
    _gen++;
    current = null;
    _inflight = null;
    notifyListeners();
  }

  Future<String> _label(double lat, double lng) =>
      _reverse != null ? _reverse(lat, lng) : reverseGeocode(_c, lat, lng).then(geoLabel);

  void _lookUpLabel(LatLng at) {
    final gen = _gen;
    _label(at.lat, at.lng).then((v) {
      if (gen != _gen || v.isEmpty) return;
      placeLabel = v;
      notifyListeners();
    }, onError: (Object e) {
      // 501 没配服务商、503、网络：地址名空着，首页显示门店。
      debugPrint('keel.store: 逆地理编码没拿到（$e）');
    });
  }

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

  /// 换了服务地址：门店要重新解析，选过的送货位置也作废（是另一家商家了）。
  void reset() => useDeviceLocation();

  Future<CurrentStore> ensure() {
    final cur = current;
    if (cur != null) return Future.value(cur);
    return _inflight ??= () async {
      try {
        final pinned = _pinned;
        final at = pinned != null ? pinned.at : await _coords();
        this.at = at;
        final res = await _c.send('GET', '/stores/resolve',
            query: at == null ? null : {'lat': '${at.lat}', 'lng': '${at.lng}'},
            decode: (j) => StoreResolveResult.fromJson(j as Map<String, dynamic>));
        final stores = res.data.stores;
        final s = res.data.matchType == 'none' || stores.isEmpty
            ? const CurrentStore(null, '')
            : CurrentStore(stores.first.id, stores.first.name);
        current = s;
        notifyListeners();
        if (pinned == null && at != null) _lookUpLabel(at);
        return s;
      } finally {
        _inflight = null;
      }
    }();
  }
}
