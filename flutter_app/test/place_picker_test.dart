import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:go_router/go_router.dart';
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
import 'package:keel_buyer/pages/place_picker_page.dart';
import 'package:keel_buyer/theme.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'map_tiles.dart';

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

http.Response off() => j({'type': 'https://keel.dev/problems/not-implemented', 'title': '未开通', 'status': 501}, 501);

Map<String, Object> place(String name, {String province = '浙江省', double lat = 30.28, double lng = 120.16}) => {
      'name': name, 'address': '中山北路', 'province': province, 'city': province.isEmpty ? '' : '杭州市',
      'district': province.isEmpty ? '' : '西湖区', 'adcode': province.isEmpty ? '' : '330106', 'street': '', 'lat': lat, 'lng': lng,
    };

Map<String, Object?> addr(int id, String detail, {double? lat, double? lng}) => {'id': id, 'receiver_name': '张三', 'phone': '13900000000',
      'province': '浙江省', 'city': '杭州市', 'district': '西湖区', 'detail': detail, 'is_default': id == 1, 'lat': lat, 'lng': lng};

/// 起一个壳：首页一个按钮 push 选择页，选完的结果放进 [got]。
Future<(Widget, List<PickedPlace?>, List<Uri>)> host(http.Response Function(http.Request r) h, {bool forAddress = false}) async {
  SharedPreferences.setMockInitialValues({'keel.access': 'a'});
  final session = Session();
  await session.load();
  final seen = <Uri>[];
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: MockClient((r) async {
    seen.add(r.url);
    return h(r);
  }));
  final got = <PickedPlace?>[];
  final router = GoRouter(routes: [
    GoRoute(path: '/', builder: (c, _) => Scaffold(body: TextButton(
        onPressed: () async => got.add(await c.push<PickedPlace>('/p')), child: const Text('open')))),
    GoRoute(path: '/p', builder: (_, _) => PlacePickerPage(forAddress: forAddress, mapTiles: NoNetworkTiles(), mapLocate: () async => null)),
  ]);
  final w = Services(client: client, session: session, store: StoreService(client, locate: () async => null),
      cart: CartCount(client, session), trace: SearchTrace(client), unread: UnreadCount(client, session),
      child: MaterialApp.router(theme: keelTheme(), routerConfig: router));
  return (w, got, seen);
}

Future<void> open(WidgetTester t, Widget w) async {
  await t.pumpWidget(w);
  await t.tap(find.text('open'));
  await t.pumpAndSettle();
}

Future<void> type(WidgetTester t, String q) async {
  await t.enterText(find.byKey(const Key('place.input')), q);
  await t.pump(const Duration(milliseconds: 400)); // 输入防抖
  await t.pumpAndSettle();
}

void main() {
  setUp(resetMapConfigCache);

  testWidgets('没配地图服务商（501）：搜索框说明暂未开通，不当错误；收货地址照样能选（带坐标的直接用）', (t) async {
    final (w, got, _) = await host((r) => switch (r.url.path) {
          '/api/v1/addresses' => j([addr(1, '文三路 1 号', lat: 30.27, lng: 120.15)]),
          _ => off(),
        });
    await open(t, w);
    await type(t, '西湖');
    expect(find.byKey(const Key('place.off')), findsOneWidget);
    expect(find.textContaining('出错'), findsNothing);
    await t.tap(find.byKey(const Key('place.address.1')));
    await t.pumpAndSettle();
    final p = got.single!;
    expect((p.label, p.at), ('文三路 1 号', (lat: 30.27, lng: 120.15)));
    expect(p.device, isFalse);
  });

  testWidgets('输入提示：按关键词出候选，选中后再 reverse 取完整省市区', (t) async {
    final (w, got, seen) = await host((r) => switch (r.url.path) {
          '/api/v1/geo/suggest' => j({'items': [place('西湖文化广场', province: ''), place('西湖', province: '')]}),
          '/api/v1/geo/reverse' => j(place('某大厦')),
          '/api/v1/addresses' => j([]),
          _ => off(),
        });
    await open(t, w);
    await type(t, '西湖');
    expect(find.text('西湖文化广场'), findsOneWidget);
    await t.tap(find.text('西湖文化广场'));
    await t.pumpAndSettle();
    final p = got.single!;
    expect(p.label, '西湖文化广场');
    expect(p.place!.province, '浙江省');
    expect(p.place!.adcode, '330106');
    expect(seen.where((u) => u.path == '/api/v1/geo/suggest').last.queryParameters['q'], '西湖');
  });

  testWidgets('503：说暂时不可用、可以稍后再试', (t) async {
    final (w, _, _) = await host((r) => switch (r.url.path) {
          '/api/v1/addresses' => j([]),
          _ => j({'type': 'https://keel.dev/problems/geo-unavailable', 'title': '不可用', 'status': 503}, 503),
        });
    await open(t, w);
    await type(t, '西湖');
    expect(find.byKey(const Key('place.down')), findsOneWidget);
  });

  testWidgets('老地址没有坐标：先用地址全文搜一次取坐标', (t) async {
    final (w, got, seen) = await host((r) => switch (r.url.path) {
          '/api/v1/addresses' => j([addr(2, '文三路 9 号')]),
          '/api/v1/geo/suggest' => j({'items': [place('文三路 9 号', lat: 30.2, lng: 120.1)]}),
          _ => off(),
        });
    await open(t, w);
    await t.tap(find.byKey(const Key('place.address.2')));
    await t.pumpAndSettle();
    expect(got.single!.at, (lat: 30.2, lng: 120.1));
    expect(seen.last.queryParameters['q'], '浙江省 杭州市 西湖区 文三路 9 号');
  });

  testWidgets('老地址没有坐标、也搜不到（501）：照样选中，坐标为 null（首页按默认店并提示）', (t) async {
    final (w, got, _) = await host((r) => switch (r.url.path) {
          '/api/v1/addresses' => j([addr(2, '文三路 9 号')]),
          _ => off(),
        });
    await open(t, w);
    await t.tap(find.byKey(const Key('place.address.2')));
    await t.pumpAndSettle();
    expect(got.single!.label, '文三路 9 号');
    expect(got.single!.at, isNull);
  });

  testWidgets('「使用当前定位」', (t) async {
    final (w, got, _) = await host((r) => j([]));
    await open(t, w);
    await t.tap(find.byKey(const Key('place.device')));
    await t.pumpAndSettle();
    expect(got.single!.device, isTrue);
  });

  testWidgets('从地址编辑页进来：只有搜索，不列收货地址、不给当前定位', (t) async {
    final (w, _, seen) = await host((r) => off(), forAddress: true);
    await open(t, w);
    expect(find.byKey(const Key('place.device')), findsNothing);
    expect(seen.where((u) => u.path == '/api/v1/addresses'), isEmpty);
  });

  group('地图选点（App / Web）', () {
    final mapOn = {'enabled': true, 'layers': ['vec', 'cva'], 'max_zoom': 18, 'attribution': '© 天地图'};

    testWidgets('服务端没开底图（enabled=false）：不给「在地图上选点」', (t) async {
      final (w, _, seen) = await host((r) => switch (r.url.path) {
            '/api/v1/geo/map' => j({'enabled': false, 'layers': [], 'max_zoom': 0, 'attribution': ''}),
            '/api/v1/addresses' => j([]),
            _ => off(),
          });
      await open(t, w);
      expect(seen.where((u) => u.path == '/api/v1/geo/map'), hasLength(1));
      expect(find.byKey(const Key('place.map')), findsNothing);
      expect(find.byKey(const Key('place.device')), findsOneWidget);
    });

    testWidgets('开了：入口出来，点开是地图页；确定后带着补全的地点回来', (t) async {
      final (w, got, seen) = await host((r) => switch (r.url.path) {
            '/api/v1/geo/map' => j(mapOn),
            '/api/v1/geo/reverse' => j(place('黄龙时代广场', lat: 39.9, lng: 116.4)),
            '/api/v1/addresses' => j([]),
            _ => off(),
          }, forAddress: true);
      await open(t, w);
      await t.tap(find.byKey(const Key('place.map')));
      await t.pumpAndSettle();
      expect(find.byType(MapPickerPage), findsOneWidget);
      expect(find.text('当前位置：黄龙时代广场'), findsOneWidget);
      await t.tap(find.byKey(const Key('map.confirm')));
      await t.pumpAndSettle();
      final p = got.single!;
      expect(p.label, '黄龙时代广场');
      expect((p.place!.province, p.place!.adcode), ('浙江省', '330106'));
      // 地图页已经 reverse 过，确定时不再多问一次。
      expect(seen.where((u) => u.path == '/api/v1/geo/reverse'), hasLength(1));
      // 一次会话只问一次底图配置。
      expect(seen.where((u) => u.path == '/api/v1/geo/map'), hasLength(1));
    });
  });
}
