import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:go_router/go_router.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/cart_count.dart';
import 'package:keel_buyer/api/catalog.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/notification.dart';
import 'package:keel_buyer/api/services.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';
import 'package:keel_buyer/pages/address_edit_page.dart';
import 'package:keel_buyer/pages/place_picker_page.dart';
import 'package:keel_buyer/theme.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

Map<String, dynamic> addr(int id, bool def) => {'id': id, 'receiver_name': '张三', 'phone': '13900000000', 'province': '浙江省',
      'city': '杭州市', 'district': '西湖区', 'detail': '1 号', 'region_code': '330106', 'is_default': def};

Future<Widget> host(MockClient fake, Widget page) async {
  SharedPreferences.setMockInitialValues({'keel.access': 'a'});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: fake);
  final router = GoRouter(routes: [
    GoRoute(path: '/', builder: (c, _) => Scaffold(body: TextButton(onPressed: () => c.push('/e'), child: const Text('open')))),
    GoRoute(path: '/e', builder: (_, _) => page),
    GoRoute(path: '/place', builder: (_, st) => PlacePickerPage(forAddress: st.uri.queryParameters['for'] == 'address')),
  ]);
  return Services(client: client, session: session, store: StoreService(client, locate: () async => null), cart: CartCount(client, session),
      trace: SearchTrace(client), unread: UnreadCount(client, session), child: MaterialApp.router(theme: keelTheme(), routerConfig: router));
}

void main() {
  testWidgets('新建：422 按 field 标红；改对后保存，同一个幂等键重放', (t) async {
    t.view.physicalSize = const Size(1170, 2532); // 手机屏：顶上多了「搜索地点」一栏，800×600 放不下提示那一格
    t.view.devicePixelRatio = 3;
    addTearDown(t.view.reset);
    final keys = <String?>[];
    var n = 0;
    final w = await host(MockClient((r) async {
      keys.add(r.headers['Idempotency-Key']);
      if (n++ == 0) {
        return j({'type': 'https://keel.dev/problems/invalid-request', 'title': '参数不合法', 'status': 422,
          'errors': [{'field': 'receiver_name', 'message': '不能为空'}, {'field': 'phone', 'message': '手机号格式不对'}]}, 422);
      }
      return j(addr(9, false), 201);
    }), const AddressEditPage());
    await t.pumpWidget(w);
    await t.tap(find.text('open'));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const Key('address.field.phone')), 'abc');
    await t.tap(find.byKey(const Key('address.save')));
    await t.pumpAndSettle();
    expect(find.text('不能为空'), findsOneWidget);
    expect(find.text('手机号格式不对'), findsOneWidget);
    expect(find.text('请检查标红的几项'), findsOneWidget);
    await t.enterText(find.byKey(const Key('address.field.receiverName')), '李四');
    await t.enterText(find.byKey(const Key('address.field.phone')), '13900000001');
    await t.tap(find.byKey(const Key('address.save')));
    await t.pumpAndSettle();
    expect(find.text('open'), findsOneWidget, reason: '保存成功回到上一页');
    expect(keys.toSet().length, 1, reason: '字段错误的 422 不换键');
  });

  testWidgets('编辑：打开设为默认 → PUT 带原来的 false，再调设默认接口', (t) async {
    final calls = <String>[];
    final w = await host(MockClient((r) async {
      calls.add('${r.method} ${r.url.path}${r.method == 'PUT' && r.body.isNotEmpty ? ' ${(jsonDecode(r.body) as Map)['is_default']}' : ''}');
      if (r.method == 'GET') return j([addr(1, true), addr(5, false)]);
      return j(addr(5, r.url.path.endsWith('/default')));
    }), const AddressEditPage(addressId: 5));
    await t.pumpWidget(w);
    await t.tap(find.text('open'));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('address.default')));
    await t.tap(find.byKey(const Key('address.save')));
    await t.pumpAndSettle();
    expect(calls, ['GET /api/v1/addresses', 'PUT /api/v1/addresses/5 false', 'PUT /api/v1/addresses/5/default']);
  });

  testWidgets('搜索地点：选中后自动填省市区与详细地址，保存时带上街道、区划码与坐标', (t) async {
    Map? saved;
    final w = await host(MockClient((r) async {
      switch (r.url.path) {
        case '/api/v1/geo/suggest':
          return j({'items': [{'name': '黄龙时代广场', 'address': '杭大路 15 号', 'province': '', 'city': '', 'district': '', 'adcode': '',
            'street': '', 'lat': 30.27, 'lng': 120.15}]});
        case '/api/v1/geo/reverse':
          return j({'name': '某处', 'address': '某路', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区', 'adcode': '330106',
            'street': '北山街道', 'lat': 30.27, 'lng': 120.15});
        case '/api/v1/addresses':
          saved = jsonDecode(r.body) as Map;
          return j(addr(9, false), 201);
      }
      return j({}, 404);
    }), const AddressEditPage());
    await t.pumpWidget(w);
    await t.tap(find.text('open'));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('address.searchPlace')));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const Key('place.input')), '黄龙');
    await t.pump(const Duration(milliseconds: 400));
    await t.pumpAndSettle();
    await t.tap(find.text('黄龙时代广场'));
    await t.pumpAndSettle();
    String field(String k) => t.widget<TextField>(find.byKey(Key('address.field.$k'))).controller!.text;
    expect([field('province'), field('city'), field('district'), field('detail')], ['浙江省', '杭州市', '西湖区', '杭大路 15 号 黄龙时代广场']);
    await t.enterText(find.byKey(const Key('address.field.receiverName')), '李四');
    await t.enterText(find.byKey(const Key('address.field.phone')), '13900000001');
    await t.tap(find.byKey(const Key('address.save')));
    await t.pumpAndSettle();
    expect([saved!['street'], saved!['region_code'], saved!['lat'], saved!['lng']], ['北山街道', '330106', 30.27, 120.15]);
  });

  testWidgets('选完地点又手改了省市区：坐标与区划码不再可信，一起清掉（不然运费、门店按旧点算）', (t) async {
    Map? saved;
    final w = await host(MockClient((r) async {
      switch (r.url.path) {
        case '/api/v1/geo/suggest':
          return j({'items': [{'name': '黄龙时代广场', 'address': '杭大路 15 号', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区',
            'adcode': '330106', 'street': '', 'lat': 30.27, 'lng': 120.15}]});
        case '/api/v1/geo/reverse':
          return j({'type': 'https://keel.dev/problems/not-implemented', 'title': '未开通', 'status': 501}, 501);
        case '/api/v1/addresses':
          saved = jsonDecode(r.body) as Map;
          return j(addr(9, false), 201);
      }
      return j({}, 404);
    }), const AddressEditPage());
    await t.pumpWidget(w);
    await t.tap(find.text('open'));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('address.searchPlace')));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const Key('place.input')), '黄龙');
    await t.pump(const Duration(milliseconds: 400));
    await t.pumpAndSettle();
    await t.tap(find.text('黄龙时代广场'));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const Key('address.field.district')), '上城区');
    await t.enterText(find.byKey(const Key('address.field.receiverName')), '李四');
    await t.enterText(find.byKey(const Key('address.field.phone')), '13900000001');
    await t.tap(find.byKey(const Key('address.save')));
    await t.pumpAndSettle();
    expect(saved!.containsKey('lat') || saved!.containsKey('lng') || saved!.containsKey('region_code'), isFalse);
    expect(saved!['district'], '上城区');
  });
}
