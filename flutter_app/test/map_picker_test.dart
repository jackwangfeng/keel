import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/cart_count.dart';
import 'package:keel_buyer/api/catalog.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/geo.dart';
import 'package:keel_buyer/api/notification.dart';
import 'package:keel_buyer/api/services.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';
import 'package:keel_buyer/pages/map_picker_page.dart';
import 'package:keel_buyer/theme.dart';

import 'map_tiles.dart';

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

Map<String, Object> place(String name, double lat, double lng) => {
      'name': name, 'address': '中山北路 1 号', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区',
      'adcode': '330106', 'street': '天目山路街道', 'lat': lat, 'lng': lng,
    };

const cfg = MapConfig(enabled: true, layers: ['vec', 'cva'], maxZoom: 18, attribution: '© 天地图');

/// 首页一个按钮 push 地图选点页，返回的地点放进 got；reverse 按请求里的坐标起名。
Future<(Widget, List<Place?>, List<Uri>, NoNetworkTiles)> host({LatLng? near, Future<LatLng?> Function()? locate,
    http.Response Function(http.Request r)? reverse}) async {
  final session = Session();
  final seen = <Uri>[];
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: MockClient((r) async {
    seen.add(r.url);
    if (r.url.path == '/api/v1/geo/reverse') {
      if (reverse != null) return reverse(r);
      final q = r.url.queryParameters;
      return j(place('点${double.parse(q['lat']!).toStringAsFixed(2)}', double.parse(q['lat']!), double.parse(q['lng']!)));
    }
    return j({'status': 404}, 404);
  }));
  final tiles = NoNetworkTiles();
  final got = <Place?>[];
  final w = Services(client: client, session: session, store: StoreService(client, locate: () async => null),
      cart: CartCount(client, session), trace: SearchTrace(client), unread: UnreadCount(client, session),
      child: MaterialApp(
        theme: keelTheme(),
        home: Builder(builder: (c) => Scaffold(body: TextButton(
            onPressed: () async => got.add(await Navigator.of(c).push<Place>(MaterialPageRoute(builder: (_) =>
                MapPickerPage(config: cfg, near: near, locate: locate ?? () async => null, tileProvider: tiles)))),
            child: const Text('open')))),
      ));
  return (w, got, seen, tiles);
}

Future<void> open(WidgetTester t, Widget w) async {
  await t.pumpWidget(w);
  await t.tap(find.text('open'));
  await t.pumpAndSettle();
}

Iterable<Uri> reverses(List<Uri> seen) => seen.where((u) => u.path == '/api/v1/geo/reverse');

void main() {
  testWidgets('从传入的坐标开始：查一次地址显示出来；瓦片走服务端代理、两层都叠；右下角有署名', (t) async {
    final (w, _, seen, tiles) = await host(near: (lat: 30.27, lng: 120.15));
    await open(t, w);
    expect(reverses(seen).single.queryParameters, {'lat': '30.27', 'lng': '120.15'});
    expect(find.text('当前位置：点30.27'), findsOneWidget);
    expect(find.byKey(const Key('map.attribution')), findsOneWidget);
    expect(tiles.urls.any((u) => u.startsWith('http://h/api/v1/geo/tiles/vec/16/')), isTrue);
    expect(tiles.urls.any((u) => u.startsWith('http://h/api/v1/geo/tiles/cva/16/')), isTrue);
    // 没有设备定位：不给「回到我的位置」。
    expect(find.byKey(const Key('map.mine')), findsNothing);
  });

  testWidgets('拖动地图：停下 400ms 后才按新的中心 reverse，拖动过程中不发', (t) async {
    final (w, _, seen, _) = await host(near: (lat: 30.27, lng: 120.15));
    await open(t, w);
    expect(reverses(seen), hasLength(1));
    await t.drag(find.byKey(const Key('map.view')), const Offset(0, 200));
    await t.pump(const Duration(milliseconds: 100));
    expect(find.text('正在获取位置…'), findsOneWidget);
    expect(reverses(seen), hasLength(1));
    await t.pump(const Duration(milliseconds: 400));
    await t.pumpAndSettle();
    expect(reverses(seen), hasLength(2));
    final lat = double.parse(reverses(seen).last.queryParameters['lat']!);
    expect(lat, greaterThan(30.27)); // 往下拖 = 看北边
    expect(find.text('当前位置：点${lat.toStringAsFixed(2)}'), findsOneWidget);
  });

  testWidgets('确定：返回省市区齐全的地点，坐标用图钉的', (t) async {
    final (w, got, _, _) = await host(near: (lat: 30.27, lng: 120.15),
        reverse: (r) => j(place('黄龙时代广场', 30.2701, 120.1502)));
    await open(t, w);
    await t.tap(find.byKey(const Key('map.confirm')));
    await t.pumpAndSettle();
    final p = got.single!;
    expect((p.name, p.province, p.city, p.district, p.adcode, p.street), ('黄龙时代广场', '浙江省', '杭州市', '西湖区', '330106', '天目山路街道'));
    expect((p.lat, p.lng), (30.27, 120.15));
  });

  testWidgets('没传坐标：用设备定位开场，并给「回到我的位置」', (t) async {
    final (w, _, seen, _) = await host(locate: () async => (lat: 31.23, lng: 121.47));
    await open(t, w);
    expect(reverses(seen).single.queryParameters, {'lat': '31.23', 'lng': '121.47'});
    expect(find.byKey(const Key('map.mine')), findsOneWidget);
    await t.drag(find.byKey(const Key('map.view')), const Offset(300, 0));
    await t.pump(const Duration(milliseconds: 500));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('map.mine')));
    await t.pump(const Duration(milliseconds: 500));
    await t.pumpAndSettle();
    expect(reverses(seen).last.queryParameters, {'lat': '31.23', 'lng': '121.47'});
  });

  testWidgets('没坐标也定不了位：落在默认城市中心', (t) async {
    final (w, _, seen, _) = await host(locate: () async => throw StateError('denied'));
    await open(t, w);
    expect(reverses(seen).single.queryParameters,
        {'lat': '${MapPickerPage.fallback.lat}', 'lng': '${MapPickerPage.fallback.lng}'});
  });

  testWidgets('地址查询没开（501）：照样能确定，只带坐标、名字写「地图上选的位置」', (t) async {
    final (w, got, _, _) = await host(near: (lat: 30.27, lng: 120.15),
        reverse: (r) => j({'type': 'https://keel.dev/problems/not-implemented', 'title': '未开通', 'status': 501}, 501));
    await open(t, w);
    expect(find.textContaining('地址查询暂未开通'), findsOneWidget);
    await t.tap(find.byKey(const Key('map.confirm')));
    await t.pumpAndSettle();
    final p = got.single!;
    expect((p.name, p.province, p.lat, p.lng), ('地图上选的位置', '', 30.27, 120.15));
  });
}
