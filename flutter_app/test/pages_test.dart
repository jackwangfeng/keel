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

Future<Widget> app(MockClient fake) async {
  SharedPreferences.setMockInitialValues({});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: fake);
  return Services(client: client, session: session, store: StoreService(client, locate: () async => null),
      cart: CartCount(client, session), trace: SearchTrace(client), unread: UnreadCount(client, session),
      child: MaterialApp.router(theme: keelTheme(), routerConfig: buildRouter(session)));
}

MockClient server() => MockClient((r) async {
      switch (r.url.path) {
        case '/api/v1/stores/resolve':
          return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true, 'distance_m': null}]});
        case '/api/v1/products':
          return j({'page': 1, 'page_size': 20, 'total': 1, 'store': {'match_type': 'default', 'store_id': 1},
            'items': [{'id': 23, 'title': '冷萃咖啡液 6 支', 'min_price_cents': 5580, 'status': 1,
              'promotion_tags': [{'promotion_id': 1, 'promotion_type': 1, 'label': '满199减20'}]}]});
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
    expect(find.text('由「示例小店」为你配送'), findsOneWidget);
    expect(find.byKey(const Key('product.card.23')), findsOneWidget);
    expect(find.text('¥55.80'), findsOneWidget);
    expect(find.text('满199减20'), findsOneWidget);
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
