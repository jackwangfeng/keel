import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/cart_count.dart';
import 'package:keel_buyer/api/catalog.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/notification.dart';
import 'package:keel_buyer/api/services.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';
import 'package:keel_buyer/router.dart';
import 'package:keel_buyer/theme.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

Future<Widget> app(MockClient fake, {StoreService Function(ApiClient c)? store}) async {
  SharedPreferences.setMockInitialValues({});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: fake);
  return Services(client: client, session: session, store: store?.call(client) ?? StoreService(client, locate: () async => null),
      cart: CartCount(client, session), trace: SearchTrace(client), unread: UnreadCount(client, session),
      child: MaterialApp.router(theme: keelTheme(), routerConfig: buildRouter(session)));
}

MockClient server({void Function(http.Request r)? spy}) => MockClient((r) async {
      spy?.call(r);
      switch (r.url.path) {
        case '/api/v1/stores/resolve':
          return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true, 'distance_m': null}]});
        case '/api/v1/products':
          return j({'page': 1, 'page_size': 20, 'total': 1, 'store': {'match_type': 'default', 'store_id': 1},
            'items': [{'id': 23, 'title': '冷萃咖啡液 6 支', 'min_price_cents': 5580, 'status': 1,
              'promotion_tags': [{'promotion_id': 1, 'promotion_type': 1, 'label': '满199减20'}]}]});
        case '/api/v1/addresses':
          return j([
            {'id': 1, 'receiver_name': '张三', 'phone': '13900000000', 'province': '上海市', 'city': '上海市', 'district': '黄浦区',
              'detail': '人民大道 200 号', 'is_default': true, 'lat': 31.23, 'lng': 121.47},
            {'id': 2, 'receiver_name': '张三', 'phone': '13900000000', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区',
              'detail': '文三路 9 号', 'is_default': false},
          ]);
        case '/api/v1/geo/suggest' || '/api/v1/geo/reverse':
          return j({'type': 'https://keel.dev/problems/not-implemented', 'title': '未开通', 'status': 501}, 501);
        case '/api/v1/auth/login':
          final b = jsonDecode(r.body) as Map;
          if (b['password'] != 'pw') return j({'type': 'x', 'title': '手机号或密码错误', 'status': 401}, 401);
          return j({'access_token': 'a', 'refresh_token': 'r', 'token_type': 'Bearer', 'expires_in': 7200,
            'user': {'id': 2, 'nickname': 'e2e 买家'}});
      }
      return j({'type': 'x', 'title': 'nope', 'status': 404}, 404);
    });

void main() {
  testWidgets('首页：门店行与商品卡（标题、价格、活动标签）', (t) async {
    await t.pumpWidget(await app(server()));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('home.store')), findsOneWidget);
    expect(find.text('由「示例小店」为你配送 ›'), findsOneWidget);
    expect(find.byKey(const Key('product.card.23')), findsOneWidget);
    expect(find.text('¥55.80'), findsOneWidget);
    expect(find.text('满199减20'), findsOneWidget);
  });

  testWidgets('首页：定位到了、逆地理编码有结果 → 显示「送至 …」，不显示门店', (t) async {
    await t.pumpWidget(await app(server(),
        store: (c) => StoreService(c, locate: () async => (lat: 30.27, lng: 120.15), reverse: (_, _) async => '黄龙时代广场')));
    await t.pumpAndSettle();
    expect(find.text('送至 黄龙时代广场 ›'), findsOneWidget);
    expect(find.textContaining('示例小店'), findsNothing);
  });

  testWidgets('首页换地址：选一条带坐标的收货地址 → 按它的坐标重新解析门店，显示送至那里', (t) async {
    final resolves = <Map<String, String>>[];
    await t.pumpWidget(await app(server(spy: (r) {
      if (r.url.path == '/api/v1/stores/resolve') resolves.add(r.url.queryParameters);
    })));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('home.store')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('place.address.1')));
    await t.pumpAndSettle();
    expect(resolves.last, {'lat': '31.23', 'lng': '121.47'});
    expect(find.text('送至 人民大道 200 号 ›'), findsOneWidget);
    expect(find.byKey(const Key('product.card.23')), findsOneWidget);
  });

  testWidgets('首页换地址：老地址没有坐标、也查不到（501）→ 按默认店，并照实提示', (t) async {
    await t.pumpWidget(await app(server()));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('home.store')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('place.address.2')));
    await t.pumpAndSettle();
    expect(find.text('送至 文三路 9 号 ›'), findsOneWidget);
    expect(find.text('这条地址没有位置信息，暂按默认门店'), findsOneWidget);
  });

  testWidgets('我的 → 登录：密码错显示服务端原因；对了回到我的并显示昵称', (t) async {
    await t.pumpWidget(await app(server()));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('tab.me')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('me.login')));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const Key('login.phone')), '13800000001');
    await t.enterText(find.byKey(const Key('login.password')), 'wrong');
    await t.tap(find.byKey(const Key('login.submit')));
    await t.pumpAndSettle();
    expect(find.text('手机号或密码错误'), findsOneWidget);
    await t.enterText(find.byKey(const Key('login.password')), 'pw');
    await t.tap(find.byKey(const Key('login.submit')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('me.nickname')), findsOneWidget);
    expect(find.text('e2e 买家'), findsOneWidget);
    await t.scrollUntilVisible(find.byKey(const Key('me.logout')), 200);
    await t.tap(find.byKey(const Key('me.logout')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('me.login')), findsOneWidget);
    expect(find.text('已退出'), findsOneWidget);
    // 从「我的」页直接再登录（tab 一直是「我的」，没切走过）：回来不能还挂着「已退出」。
    await t.tap(find.byKey(const Key('me.login')));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const Key('login.phone')), '13800000001');
    await t.enterText(find.byKey(const Key('login.password')), 'pw');
    await t.tap(find.byKey(const Key('login.submit')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('me.nickname')), findsOneWidget);
    // 提示在列表最底下（退出按钮下面）：先滚过去，免得它只是没被构建出来、而不是真的没了。
    await t.scrollUntilVisible(find.byKey(const Key('me.logout')), 200);
    await t.pumpAndSettle();
    expect(find.byKey(const Key('me.message')), findsNothing);
  });

  testWidgets('登录页还在转场时就登录成功：外壳不重复（不报 Duplicate GlobalKey）', (t) async {
    await t.pumpWidget(await app(server()));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('tab.me')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('me.login')));
    await t.pump();
    await t.pump(const Duration(milliseconds: 50));
    await t.enterText(find.byKey(const Key('login.phone')), '13800000001');
    await t.enterText(find.byKey(const Key('login.password')), 'pw');
    await t.tap(find.byKey(const Key('login.submit')));
    await t.pumpAndSettle();
    expect(t.takeException(), isNull);
    expect(find.byKey(const Key('me.nickname')), findsOneWidget);
  });
}
