import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/geo.dart';
import 'package:keel_buyer/api/session.dart';

const _json = {'content-type': 'application/json; charset=utf-8'};

Map<String, Object> place(String name, {String province = '浙江省', double lat = 30.27, double lng = 120.15}) => {
      'name': name, 'address': '文三路 90 号', 'province': province, 'city': '杭州市', 'district': '西湖区',
      'adcode': '330106', 'street': '文三路', 'lat': lat, 'lng': lng,
    };

void main() {
  group('GCJ-02 ⇄ WGS-84（对照点由服务端 internal/geo/coord.go 算出）', () {
    const pairs = [
      ((39.908692, 116.397477), (39.910095, 116.403721)),
      ((31.230416, 121.473701), (31.228474, 121.478224)),
      ((22.543096, 114.057865), (22.540379, 114.062979)),
      ((30.274084, 120.155070), (30.271755, 120.159765)),
    ];
    test('WGS → GCJ 与服务端一致（6 位小数）', () {
      for (final ((wl, wg), (gl, gg)) in pairs) {
        final g = wgs84ToGcj02(wl, wg);
        expect(g.lat, closeTo(gl, 1e-6));
        expect(g.lng, closeTo(gg, 1e-6));
      }
    });
    test('GCJ → WGS 迭代求逆，回到原点（误差 < 1e-6 度）', () {
      for (final ((wl, wg), (gl, gg)) in pairs) {
        final w = gcj02ToWgs84(gl, gg);
        expect(w.lat, closeTo(wl, 1e-6));
        expect(w.lng, closeTo(wg, 1e-6));
      }
    });
    test('国外坐标不偏移', () {
      final w = gcj02ToWgs84(51.5, -0.12);
      expect((w.lat, w.lng), (51.5, -0.12));
      final g = wgs84ToGcj02(51.5, -0.12);
      expect((g.lat, g.lng), (51.5, -0.12));
    });
  });

  group('/geo 接口', () {
    ApiClient client(Future<http.Response> Function(http.Request r) h) =>
        ApiClient(base: 'http://h/api/v1', session: Session(), http: MockClient(h));

    test('reverse 带坐标去问', () async {
      late Uri seen;
      final c = client((r) async {
        seen = r.url;
        return http.Response(jsonEncode(place('黄龙时代广场')), 200, headers: _json);
      });
      final p = await reverseGeocode(c, 30.27, 120.15);
      expect(seen.path, '/api/v1/geo/reverse');
      expect(seen.queryParameters, {'lat': '30.27', 'lng': '120.15'});
      expect(p.name, '黄龙时代广场');
    });

    test('suggest：带上附近坐标；空关键词不发请求', () async {
      final seen = <Uri>[];
      final c = client((r) async {
        seen.add(r.url);
        return http.Response(jsonEncode({'items': [place('西湖文化广场'), place('西湖')]}), 200, headers: _json);
      });
      final items = await suggestPlaces(c, ' 西湖 ', near: (lat: 30.27, lng: 120.15));
      expect(items.map((e) => e.name), ['西湖文化广场', '西湖']);
      expect(seen.single.queryParameters, {'q': '西湖', 'lat': '30.27', 'lng': '120.15'});
      expect(await suggestPlaces(c, '  '), isEmpty);
      expect(seen, hasLength(1));
    });

    test('501 = 没配地图服务商（降级，不是错误）；503 = 服务商暂时不可用', () {
      expect(geoOff(const ApiFailure(501, null, 'not-implemented')), isTrue);
      expect(geoOff(const ApiFailure(503, null, 'geo-unavailable')), isFalse);
      expect(geoDown(const ApiFailure(503, null, 'geo-unavailable')), isTrue);
      expect(geoDown(const ApiFailure(0, null, '网络')), isFalse);
    });
  });

  test('首页上显示的地址：优先地点名，没有就用地址', () {
    expect(geoLabel(const Place(name: '黄龙时代广场', address: '文三路 90 号', lat: 0, lng: 0)), '黄龙时代广场');
    expect(geoLabel(const Place(name: '', address: '文三路 90 号', lat: 0, lng: 0)), '文三路 90 号');
  });

  group('选中一个点之后', () {
    ApiClient client(Future<http.Response> Function(http.Request r) h) =>
        ApiClient(base: 'http://h/api/v1', session: Session(), http: MockClient(h));
    const picked = Place(name: '西湖文化广场', address: '中山北路', lat: 30.28, lng: 120.16);

    test('再 reverse 一次补全省市区；地点名、地址、坐标保留选中的', () async {
      final c = client((r) async => http.Response(jsonEncode(place('某大厦', lat: 30.2801, lng: 120.1601)), 200, headers: _json));
      final p = await completePlace(c, picked);
      expect([p.name, p.address, p.province, p.city, p.district, p.adcode, p.street], ['西湖文化广场', '中山北路', '浙江省', '杭州市', '西湖区', '330106', '文三路']);
      expect((p.lat, p.lng), (30.28, 120.16));
    });

    test('reverse 拿不到（501 / 503 / 网络）：原样用选中的，省市区留给用户手填', () async {
      final c = client((r) async => http.Response('{"type":"x/not-implemented","title":"t","status":501}', 501,
          headers: {'content-type': 'application/problem+json'}));
      final p = await completePlace(c, picked);
      expect(p, same(picked));
    });
  });

  test('小程序地图选点：GCJ-02 转成 WGS-84；名称、地址照搬', () {
    final p = mapPick({'name': '天安门', 'address': '北京市东城区', 'latitude': 39.910095, 'longitude': 116.403721})!;
    expect(p.lat, closeTo(39.908692, 1e-6));
    expect(p.lng, closeTo(116.397477, 1e-6));
    expect((p.name, p.address), ('天安门', '北京市东城区'));
    expect(mapPick({'errMsg': 'chooseLocation:ok'}), isNull, reason: '没有坐标就当没选');
  });

  group('底图配置 /geo/map', () {
    setUp(resetMapConfigCache);
    ApiClient client(Future<http.Response> Function(http.Request r) h, {String base = 'http://h/api/v1'}) =>
        ApiClient(base: base, session: Session(), http: MockClient(h));
    final on = {'enabled': true, 'layers': ['vec', 'cva'], 'max_zoom': 18, 'attribution': '© 天地图'};

    test('开了：一次会话只问一次', () async {
      var n = 0;
      final c = client((r) async {
        n++;
        expect(r.url.path, '/api/v1/geo/map');
        return http.Response(jsonEncode(on), 200, headers: _json);
      });
      final m = await fetchMapConfig(c);
      await fetchMapConfig(c);
      expect(await canPickOnMap(c), isTrue);
      expect(n, 1);
      expect((m.enabled, m.maxZoom, m.attribution), (true, 18, '© 天地图'));
      expect(m.layers, ['vec', 'cva']);
    });

    test('没开 / 没有图层：不给入口', () async {
      final c = client((r) async => http.Response(jsonEncode({'enabled': false, 'layers': [], 'max_zoom': 0, 'attribution': ''}), 200, headers: _json));
      expect(await canPickOnMap(c), isFalse);
    });

    test('拿不到（404 / 网络）按没开算，且不记下，下次再问', () async {
      var n = 0;
      final c = client((r) async {
        n++;
        return n == 1 ? http.Response('{}', 404, headers: _json) : http.Response(jsonEncode(on), 200, headers: _json);
      });
      expect(await canPickOnMap(c), isFalse);
      expect(await canPickOnMap(c), isTrue);
      expect(n, 2);
    });

    test('pickOnMap：没开就不打开地图页', () async {
      final c = client((r) async => http.Response(jsonEncode({'enabled': false, 'layers': [], 'max_zoom': 0, 'attribution': ''}), 200, headers: _json));
      var opened = false;
      final p = await pickOnMap(c, openMap: (_) async {
        opened = true;
        return null;
      });
      expect((p, opened), (null, false));
    });

    test('瓦片地址：跟服务地址走；Web 同源的相对地址按页面补全', () {
      final abs = client((r) async => http.Response('', 200));
      expect(mapTileUrl(abs, 'vec'), 'http://h/api/v1/geo/tiles/vec/{z}/{x}/{y}');
      final rel = client((r) async => http.Response('', 200), base: '/api/v1');
      expect(mapTileUrl(rel, 'cva', page: Uri.parse('https://shop.example.com/#/place')),
          'https://shop.example.com/api/v1/geo/tiles/cva/{z}/{x}/{y}');
    });
  });
}
