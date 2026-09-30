import 'package:flutter/foundation.dart';
import 'package:geolocator/geolocator.dart';
import 'package:mp_flutter_wechat/mp_flutter_wechat.dart';

/// 这台设备现在在哪（按围栏解析门店用）。**尽量拿到坐标，拿不到就给 null**：拒绝授权、没开定位、出错
/// 都不是错误 —— 服务端对「没坐标」的回答就是回落默认店（与已下线的 uni-app x 版 store.uts 同一套规则，见 tag uniapp-final）。
///
/// 坐标系必须是 WGS-84：围栏存在 SRID 4326。国内接口默认常给 GCJ-02（火星坐标），城区差一两百到五六百米，
/// 足够把人判到隔壁店或围栏外。所以小程序里显式要 wgs84；Web（浏览器 Geolocation）与原生（系统定位）本来就是。
///
/// 编译时 `--dart-define=KEEL_LOCATE=off`：不定位，总用默认店（只有一家店的商家；Web e2e 也用它，用例按默认店写）。
/// 原生定位参数。Android 上不走 Google Play 服务的融合定位：国内机器装了 GMS 也连不上 Google，
/// 请求就那么挂着（小米真机实测，权限给了、系统定位开着，5 秒超时回落默认店）；系统 LocationManager 用厂商网络定位，能拿到。
LocationSettings locationSettingsFor(TargetPlatform platform) => platform == TargetPlatform.android
    ? AndroidSettings(accuracy: LocationAccuracy.medium, forceLocationManager: true)
    : const LocationSettings(accuracy: LocationAccuracy.medium);

Future<({double lat, double lng})?> locateDevice() async {
  if (const String.fromEnvironment('KEEL_LOCATE') == 'off') return null;
  if (MpWechat.isAvailable) {
    final r = await MpWechat.getLocation(type: 'wgs84');
    return (lat: r.latitude, lng: r.longitude);
  }
  if (!await Geolocator.isLocationServiceEnabled()) return null;
  var perm = await Geolocator.checkPermission();
  if (perm == LocationPermission.denied) perm = await Geolocator.requestPermission();
  if (perm == LocationPermission.denied || perm == LocationPermission.deniedForever) return null;
  final p = await Geolocator.getCurrentPosition(locationSettings: locationSettingsFor(defaultTargetPlatform));
  return (lat: p.latitude, lng: p.longitude);
}
