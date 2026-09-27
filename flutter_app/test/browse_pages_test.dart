import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/cart_count.dart';
import 'package:keel_buyer/api/catalog.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/services.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';
import 'package:keel_buyer/router.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

Map<String, dynamic> summary(int id, String title, {int? max}) =>
    {'id': id, 'title': title, 'min_price_cents': 1000, 'max_price_cents': ?max, 'status': 1};

Map<String, dynamic> sku(int id, int qty, String v) =>
    {'id': id, 'sku_code': 'S$id', 'price_cents': 1000, 'available_qty': qty, 'spec_values': {'规格': v}};

Map<String, dynamic> cart(int qty) => {
      'items': [
        {'id': 1, 'sku_id': 1, 'quantity': qty, 'selected': true, 'available': true, 'status': 'ok'}
      ],
      'store': {'match_type': 'default', 'store_id': 1}, 'total_cents': 0, 'selected_total_cents': 0,
      'promotion_discount_cents': 0, 'promotions': [],
    };

/// 假服务端：记下每个请求，按路径应答。
class Fake {
  final calls = <http.Request>[];
  late final client = MockClient((r) async {
    calls.add(r);
    final p = r.url.path.replaceFirst('/api/v1', '');
    switch (p) {
      case '/stores/resolve':
        return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true}]});
      case '/categories':
        return j([{'id': 5, 'name': '咖啡', 'level': 1}, {'id': 6, 'name': '茶', 'level': 1}]);
      case '/products':
        final cat = r.url.queryParameters['category_id'];
        final items = cat == '6' ? [summary(30, '白茶')] : [summary(21, '单规格豆'), summary(22, '多规格豆', max: 2000)];
        return j({'page': 1, 'page_size': 20, 'total': items.length, 'store': {'match_type': 'default', 'store_id': 1}, 'items': items});
      case '/products/21':
        return j({...summary(21, '单规格豆'), 'skus': [sku(211, 5, '一种')]});
      case '/products/22':
        return j({...summary(22, '多规格豆', max: 2000), 'skus': [sku(221, 0, '浅焙'), sku(222, 3, '中焙')]});
      case '/cart/items':
        return j(cart(jsonDecode(r.body)['quantity'] as int));
      case '/cart':
        return j(cart(0));
      case '/search':
        return j({'items': [{...summary(22, '多规格豆'), 'in_stock': false}], 'store': {'match_type': 'default', 'store_id': 1},
          'latency_ms': 3, 'strategy': 'hybrid', 'trace_id': 'tr-1'});
      case '/search/events':
        return http.Response('', 204);
    }
    return j({'type': 'x', 'title': 'nope', 'status': 404}, 404);
  });
  Iterable<http.Request> to(String path) => calls.where((c) => c.url.path.endsWith(path));
}

/// 手机尺寸（390×844）：默认 800×600 的测试窗口里网格卡片在首屏以外。
void phone(WidgetTester t) {
  t.view.physicalSize = const Size(1170, 2532);
  t.view.devicePixelRatio = 3;
  addTearDown(t.view.reset);
}

Future<(Widget, CartCount)> app(Fake f, {bool loggedIn = true}) async {
  SharedPreferences.setMockInitialValues(loggedIn
      ? {'keel.access': 'a', 'keel.refresh': 'r', 'keel.nickname': 'e2e'}
      : {});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: f.client);
  final count = CartCount(client, session);
  return (
    Services(client: client, session: session, store: StoreService(client), cart: count, trace: SearchTrace(client),
        child: MaterialApp.router(routerConfig: buildRouter(session))),
    count
  );
}

void main() {
  testWidgets('分类：点「茶」带 category_id 重新拉，标题跟着变；点回「全部」', (t) async {
    phone(t);
    final f = Fake();
    await t.pumpWidget((await app(f)).$1);
    await t.pumpAndSettle();
    expect(find.byKey(const Key('product.card.21')), findsOneWidget);
    await t.tap(find.byKey(const Key('home.cat.6')));
    await t.pumpAndSettle();
    expect(f.to('/products').last.url.queryParameters['category_id'], '6');
    expect(find.byKey(const Key('product.card.30')), findsOneWidget);
    expect(find.text('茶'), findsNWidgets(2));
    await t.tap(find.byKey(const Key('home.cat.all')));
    await t.pumpAndSettle();
    expect(f.to('/products').last.url.queryParameters.containsKey('category_id'), isFalse);
  });

  testWidgets('「＋」单规格直接加一件，件数更新；多规格弹浮层，默认选有货的', (t) async {
    phone(t);
    final f = Fake();
    final (w, count) = await app(f);
    await t.pumpWidget(w);
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('product.add.21')));
    await t.pumpAndSettle();
    expect(jsonDecode(f.to('/cart/items').single.body), {'sku_id': 211, 'quantity': 1});
    expect(count.n, 1);
    expect(find.text('已加入购物车'), findsOneWidget);

    await t.tap(find.byKey(const Key('product.add.22')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('quick.confirm')), findsOneWidget);
    await t.tap(find.byKey(const Key('quick.sku.221'))); // 无货：点了不换
    await t.tap(find.byKey(const Key('quick.plus')));
    await t.pump();
    await t.tap(find.byKey(const Key('quick.confirm')));
    await t.pumpAndSettle();
    expect(jsonDecode(f.to('/cart/items').last.body), {'sku_id': 222, 'quantity': 2});
    expect(count.n, 2);
    expect(find.byKey(const Key('quick.confirm')), findsNothing);
  });

  testWidgets('没登录点「＋」去登录页', (t) async {
    phone(t);
    final f = Fake();
    await t.pumpWidget((await app(f, loggedIn: false)).$1);
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('product.add.21')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('login.phone')), findsOneWidget);
  });

  testWidgets('详情：无货规格排后、点了提示；加购后出现「去结算」', (t) async {
    phone(t);
    final f = Fake();
    await t.pumpWidget((await app(f)).$1);
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('product.card.22')));
    await t.pumpAndSettle();
    expect(find.text('已选 中焙'), findsOneWidget);
    await t.tap(find.byKey(const Key('detail.sku.221')));
    await t.pump();
    expect(find.text('这个规格暂时无货'), findsOneWidget);
    await t.tap(find.byKey(const Key('detail.add')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('detail.added')), findsOneWidget);
  });

  testWidgets('搜索：缺货标出来；点进去回传 click（带 trace_id）', (t) async {
    phone(t);
    final f = Fake();
    await t.pumpWidget((await app(f)).$1);
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('home.search')));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const Key('search.input')), '咖啡');
    await t.tap(find.byKey(const Key('search.submit')));
    await t.pumpAndSettle();
    expect(find.text('「咖啡」共 1 件，按相关度排序'), findsOneWidget);
    expect(find.text('暂时缺货'), findsOneWidget);
    expect(find.byKey(const Key('product.add.22')), findsNothing, reason: '缺货不给「＋」');
    await t.tap(find.byKey(const Key('search.row.22')));
    await t.pumpAndSettle();
    expect(jsonDecode(f.to('/search/events').single.body), {'trace_id': 'tr-1', 'event': 'click', 'product_id': 22});
    expect(find.byKey(const Key('detail.title')), findsOneWidget);
  });
}
