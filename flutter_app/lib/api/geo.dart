import 'dart:math' as math;

import 'package:mp_flutter_wechat/mp_flutter_wechat.dart';

import 'client.dart';
import 'schema.g.dart';

/// 地点：逆地理编码与输入提示（docs/POI-设计.md）。服务端代理地图服务商、key 不下发，坐标一律 WGS-84。
/// 没配服务商时两条都回 501 —— 那是降级（退回手填），不是错误；服务商挂了回 503。

typedef LatLng = ({double lat, double lng});

/// 一个地点（页面用的行模型；契约的 GeoPlace 只在这一层读）。字段与收货地址对齐，adcode 就是 region_code。
class Place {
  final String name;
  final String address;
  final String province;
  final String city;
  final String district;
  final String adcode;
  /// 街道办 / 乡镇（收货地址的第四级）。
  final String street;
  final double lat;
  final double lng;
  const Place({required this.name, required this.address, this.province = '', this.city = '', this.district = '', this.adcode = '',
      this.street = '', required this.lat, required this.lng});
}

Place _place(GeoPlace g) => Place(name: g.name, address: g.address, province: g.province, city: g.city, district: g.district,
    adcode: g.adcode, street: g.street, lat: g.lat, lng: g.lng);

/// 坐标 → 地点（省市区、街道、adcode 齐全）。
Future<Place> reverseGeocode(ApiClient c, double lat, double lng) async => (await c.send('GET', '/geo/reverse',
        query: {'lat': '$lat', 'lng': '$lng'}, decode: (j) => _place(GeoPlace.fromJson(j as Map<String, dynamic>))))
    .data;

/// 输入提示：只含有坐标的候选；给了 near 就按离得近的排。候选里省市可能不全，选中后再 reverse 取完整的。
Future<List<Place>> suggestPlaces(ApiClient c, String q, {LatLng? near, String? city}) async {
  final k = q.trim();
  if (k.isEmpty) return const [];
  return (await c.send('GET', '/geo/suggest',
          query: {
            'q': k,
            if (near != null) 'lat': '${near.lat}',
            if (near != null) 'lng': '${near.lng}',
            if (city != null && city.isNotEmpty) 'city': city,
          },
          decode: (j) => ((j as Map<String, dynamic>)['items'] as List).map((e) => _place(GeoPlace.fromJson(e as Map<String, dynamic>))).toList()))
      .data;
}

/// 没配地图服务商。
bool geoOff(ApiFailure f) => f.status == 501;

/// 服务商暂时不可用。
bool geoDown(ApiFailure f) => f.status == 503;

/// 首页「送至 …」里的那一截：优先地点名（「黄龙时代广场」），没有就用地址。
String geoLabel(Place p) => p.name.isNotEmpty ? p.name : p.address;

/// 选中了一个点（输入提示的候选 / 地图选点）：再 reverse 一次取完整的省市区、街道、adcode；
/// 地点名、地址、坐标保留选中的那个。拿不到（501 / 503 / 网络）就原样用，省市区留给用户手填。
Future<Place> completePlace(ApiClient c, Place p) async {
  try {
    final r = await reverseGeocode(c, p.lat, p.lng);
    return Place(
      name: p.name.isNotEmpty ? p.name : r.name,
      address: p.address.isNotEmpty ? p.address : r.address,
      province: r.province,
      city: r.city,
      district: r.district,
      adcode: r.adcode,
      street: r.street,
      lat: p.lat,
      lng: p.lng,
    );
  } on ApiFailure {
    return p;
  }
}

/// wx.chooseLocation 的返回 → 地点（坐标已转 WGS-84，省市区空着，交给 [completePlace]）。没有坐标 = 没选。
Place? mapPick(Map<String, Object?> r) {
  final lat = r['latitude'], lng = r['longitude'];
  if (lat is! num || lng is! num) return null;
  final w = gcj02ToWgs84(lat.toDouble(), lng.toDouble());
  return Place(name: '${r['name'] ?? ''}', address: '${r['address'] ?? ''}', lat: w.lat, lng: w.lng);
}

/// 小程序里才有的地图选点（微信自带的选点页，免 key）。用户取消是 null。
bool get canPickOnMap => MpWechat.isAvailable;

Future<Place?> pickOnMap({LatLng? near}) async {
  final g = near == null ? null : wgs84ToGcj02(near.lat, near.lng);
  try {
    return mapPick(await MpWechat.call('chooseLocation', {'latitude': ?g?.lat, 'longitude': ?g?.lng}));
  } on MpWechatException {
    return null;
  }
}

// ---- GCJ-02 ⇄ WGS-84 ----
// 微信 wx.chooseLocation 只给 GCJ-02（火星坐标）；围栏、门店、收货地址全是 WGS-84。
// 与服务端 internal/geo/coord.go 同一套公式（Krasovsky 1940 椭球），GCJ → WGS 迭代求逆。

const _a = 6378245.0;
const _ee = 0.00669342162296594323;

bool _outOfChina(double lat, double lng) => lng < 72.004 || lng > 137.8347 || lat < 0.8293 || lat > 55.8271;

double _tLat(double x, double y) {
  var r = -100.0 + 2.0 * x + 3.0 * y + 0.2 * y * y + 0.1 * x * y + 0.2 * math.sqrt(x.abs());
  r += (20.0 * math.sin(6.0 * x * math.pi) + 20.0 * math.sin(2.0 * x * math.pi)) * 2.0 / 3.0;
  r += (20.0 * math.sin(y * math.pi) + 40.0 * math.sin(y / 3.0 * math.pi)) * 2.0 / 3.0;
  r += (160.0 * math.sin(y / 12.0 * math.pi) + 320 * math.sin(y * math.pi / 30.0)) * 2.0 / 3.0;
  return r;
}

double _tLng(double x, double y) {
  var r = 300.0 + x + 2.0 * y + 0.1 * x * x + 0.1 * x * y + 0.1 * math.sqrt(x.abs());
  r += (20.0 * math.sin(6.0 * x * math.pi) + 20.0 * math.sin(2.0 * x * math.pi)) * 2.0 / 3.0;
  r += (20.0 * math.sin(x * math.pi) + 40.0 * math.sin(x / 3.0 * math.pi)) * 2.0 / 3.0;
  r += (150.0 * math.sin(x / 12.0 * math.pi) + 300.0 * math.sin(x / 30.0 * math.pi)) * 2.0 / 3.0;
  return r;
}

LatLng wgs84ToGcj02(double lat, double lng) {
  if (_outOfChina(lat, lng)) return (lat: lat, lng: lng);
  var dLat = _tLat(lng - 105.0, lat - 35.0);
  var dLng = _tLng(lng - 105.0, lat - 35.0);
  final rad = lat / 180.0 * math.pi;
  var magic = math.sin(rad);
  magic = 1 - _ee * magic * magic;
  final sq = math.sqrt(magic);
  dLat = (dLat * 180.0) / ((_a * (1 - _ee)) / (magic * sq) * math.pi);
  dLng = (dLng * 180.0) / (_a / sq * math.cos(rad) * math.pi);
  return (lat: lat + dLat, lng: lng + dLng);
}

LatLng gcj02ToWgs84(double lat, double lng) {
  if (_outOfChina(lat, lng)) return (lat: lat, lng: lng);
  var w = (lat: lat, lng: lng);
  for (var i = 0; i < 30; i++) {
    final g = wgs84ToGcj02(w.lat, w.lng);
    final dl = g.lat - lat, dg = g.lng - lng;
    if (dl.abs() < 1e-9 && dg.abs() < 1e-9) break;
    w = (lat: w.lat - dl, lng: w.lng - dg);
  }
  return w;
}
