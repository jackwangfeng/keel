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

/// 两点间的球面距离（米，haversine）。只用来比远近，精度够用。
double distanceM(LatLng a, LatLng b) {
  const r = 6371000.0;
  double rad(double d) => d * math.pi / 180;
  final dLat = rad(b.lat - a.lat), dLng = rad(b.lng - a.lng);
  final h = math.pow(math.sin(dLat / 2), 2) + math.cos(rad(a.lat)) * math.cos(rad(b.lat)) * math.pow(math.sin(dLng / 2), 2);
  return 2 * r * math.asin(math.sqrt(h));
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

/// 底图配置（GET /geo/map；契约的 GeoMapConfig 只在这一层读）。[layers] 从下往上叠（天地图 base + label，label 透明底）。
/// 瓦片坐标 CGCS2000 ≈ WGS-84，与门店 / 围栏 / 收货地址同一套，不做 GCJ-02 转换。
class MapConfig {
  final bool enabled;
  final List<String> layers;
  final int maxZoom;
  final String attribution;
  const MapConfig({required this.enabled, this.layers = const [], this.maxZoom = 18, this.attribution = ''});
  static const off = MapConfig(enabled: false);
}

// 一次会话问一次；按服务地址分开记（「服务地址」页换了店就重新问）。
final _mapConfigs = <String, Future<MapConfig>>{};

/// 底图开没开。拿不到（没这条接口 / 网络）按没开算、不记下，下次再问；页面据此不给入口，不当错误。
Future<MapConfig> fetchMapConfig(ApiClient c) {
  final base = c.base;
  return _mapConfigs[base] ??= () async {
    try {
      final g = (await c.send('GET', '/geo/map', decode: (j) => GeoMapConfig.fromJson(j as Map<String, dynamic>))).data;
      return MapConfig(enabled: g.enabled && g.layers.isNotEmpty, layers: g.layers, maxZoom: g.maxZoom, attribution: g.attribution);
    } on ApiFailure {
      _mapConfigs.remove(base);
      return MapConfig.off;
    }
  }();
}

/// 测试用：清掉 [fetchMapConfig] 的缓存。
void resetMapConfigCache() => _mapConfigs.clear();

/// 某一层瓦片的 URL 模板（{z}/{x}/{y} 由地图组件填）。服务地址是相对的（Web 同源 /api/v1）就按页面地址补全。
String mapTileUrl(ApiClient c, String layer, {Uri? page}) {
  final t = '${c.base}/geo/tiles/$layer/{z}/{x}/{y}';
  final b = Uri.tryParse(c.base);
  if (b != null && b.hasScheme) return t;
  final origin = page ?? Uri.base;
  if (!origin.hasScheme || !origin.hasAuthority) return t;
  return '${origin.scheme}://${origin.authority}${t.startsWith('/') ? '' : '/'}$t';
}

/// 地图选点的入口给不给：小程序有微信自带的选点页（免 key）；其它平台要服务端开了底图。
Future<bool> canPickOnMap(ApiClient c) async => MpWechat.isAvailable || (await fetchMapConfig(c)).enabled;

/// 地图选点，返回省市区已补全的地点；用户取消是 null。
/// 小程序走 wx.chooseLocation（GCJ-02 → WGS-84，再 [completePlace]）；其它平台用 [openMap] 打开自带的地图选点页
/// （那一页确定时已经 reverse 过，原样返回）。
Future<Place?> pickOnMap(ApiClient c, {LatLng? near, required Future<Place?> Function(MapConfig cfg) openMap}) async {
  if (MpWechat.isAvailable) {
    final p = await _wxChooseLocation(near);
    return p == null ? null : completePlace(c, p);
  }
  final cfg = await fetchMapConfig(c);
  if (!cfg.enabled) return null;
  return openMap(cfg);
}

Future<Place?> _wxChooseLocation(LatLng? near) async {
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
