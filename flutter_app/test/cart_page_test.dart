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
import 'package:keel_buyer/theme.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

/// 有状态的假购物车：两行能买、一行无货。
class FakeCart {
  final items = <int, Map<String, dynamic>>{
    1: {'id': 1, 'sku_id': 101, 'product_id': 11, 'title': '豆子', 'price_cents': 1000, 'quantity': 1, 'selected': true, 'available': true, 'status': 'available'},
    2: {'id': 2, 'sku_id': 102, 'product_id': 12, 'title': '滤纸', 'price_cents': 500, 'quantity': 2, 'selected': false, 'available': true, 'status': 'available'},
    3: {'id': 3, 'sku_id': 103, 'product_id': 13, 'title': '手冲壶', 'price_cents': 9000, 'quantity': 1, 'selected': true, 'available': false, 'status': 'out_of_stock'},
  };
  final calls = <String>[];

  Map<String, dynamic> body() {
    final sel = items.values.where((i) => i['selected'] == true && i['status'] == 'available');
    return {'items': items.values.toList(), 'store': {'match_type': 'default', 'store_id': 1}, 'total_cents': 0,
      'selected_total_cents': sel.fold<int>(0, (n, i) => n + (i['price_cents'] as int) * (i['quantity'] as int)),
      'promotion_discount_cents': 0, 'promotions': []};
  }

  late final client = MockClient((r) async {
    final p = r.url.path.replaceFirst('/api/v1', '');
    calls.add('${r.method} $p');
    if (p == '/stores/resolve') return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true}]});
    if (p == '/products') return j({'page': 1, 'page_size': 20, 'total': 0, 'store': {'match_type': 'default', 'store_id': 1}, 'items': []});
    if (p == '/categories') return j([]);
    if (p == '/cart') return j(body());
    if (p.startsWith('/cart/items/') && r.method == 'PATCH') {
      final id = int.parse(p.split('/').last);
      final b = jsonDecode(r.body) as Map;
      if (b['quantity'] != null && (b['quantity'] as int) > 3) {
        return j({'type': 'https://keel.dev/problems/insufficient-stock', 'title': '库存不足', 'status': 409, 'detail': '只剩 3 件'}, 409);
      }
      items[id]!.addAll(b.cast<String, dynamic>());
      return j(body());
    }
    if (p == '/cart/selection') {
      final sel = (jsonDecode(r.body) as Map)['selected'];
      for (final i in items.values) {
        if (i['status'] == 'available') i['selected'] = sel;
      }
      return j(body());
    }
    if (p == '/cart/items/batch-delete') {
      expect(r.headers['Idempotency-Key'], isNotEmpty);
      for (final id in (jsonDecode(r.body) as Map)['item_ids'] as List) {
        items.remove(id);
      }
      return j(body());
    }
    return j({'type': 'x', 'title': 'nope', 'status': 404}, 404);
  });
}

Future<(Widget, CartCount)> app(FakeCart f) async {
  SharedPreferences.setMockInitialValues({'keel.access': 'a', 'keel.refresh': 'r', 'keel.nickname': 'e2e'});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: f.client);
  final count = CartCount(client, session);
  return (
    Services(client: client, session: session, store: StoreService(client), cart: count, trace: SearchTrace(client),
        child: MaterialApp.router(theme: keelTheme(), routerConfig: buildRouter(session))),
    count
  );
}

Future<void> openCart(WidgetTester t, Widget w) async {
  t.view.physicalSize = const Size(1170, 2532);
  t.view.devicePixelRatio = 3;
  addTearDown(t.view.reset);
  await t.pumpWidget(w);
  await t.pumpAndSettle();
  await t.tap(find.byKey(const Key('tab.cart')));
  await t.pumpAndSettle();
}

void main() {
  testWidgets('无货的行写原因、不能勾；勾选第二行后合计用服务端的数；角标是总件数', (t) async {
    final f = FakeCart();
    final (w, count) = await app(f);
    await openCart(t, w);
    expect(find.text('暂时无货'), findsOneWidget);
    expect(find.text('¥10.00'), findsWidgets);
    expect(find.text('去结算(1)'), findsOneWidget);
    expect(count.n, 4);
    await t.tap(find.byKey(const Key('cart.check.3')));
    await t.pumpAndSettle();
    expect(f.calls.where((c) => c.startsWith('PATCH')), isEmpty, reason: '无货的行点了不发请求');
    await t.tap(find.byKey(const Key('cart.check.2')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('cart.total')), findsOneWidget);
    expect((t.widget<Text>(find.byKey(const Key('cart.total')))).data, '¥20.00');
    expect(find.text('去结算(2)'), findsOneWidget);
  });

  testWidgets('改数量超库存：显示服务端原因，数量回到服务端的值', (t) async {
    final f = FakeCart();
    await openCart(t, (await app(f)).$1);
    for (var i = 0; i < 3; i++) {
      await t.tap(find.byKey(const Key('cart.1.plus')));
      await t.pumpAndSettle();
    }
    expect(find.byKey(const Key('cart.message')), findsOneWidget);
    expect(find.text('只剩 3 件'), findsOneWidget);
    expect(t.widget<Text>(find.byKey(const Key('cart.1.qty'))).data, '3');
  });

  testWidgets('管理：无货的行也能勾上删除，删除带幂等键，完成后退出管理', (t) async {
    final f = FakeCart();
    await openCart(t, (await app(f)).$1);
    await t.tap(find.byKey(const Key('cart.manage')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('cart.check.3')));
    await t.pumpAndSettle();
    expect(find.text('删除(2)'), findsOneWidget);
    await t.tap(find.byKey(const Key('cart.delete')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('cart.deleteConfirm')));
    await t.pumpAndSettle();
    expect(f.items.keys, [2]);
    expect(find.byKey(const Key('cart.row.3')), findsNothing);
    expect(find.text('去结算(0)'), findsOneWidget);
  });
}
