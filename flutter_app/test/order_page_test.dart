import 'dart:convert';
import 'package:flutter/services.dart';

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

// 订单详情页的物流信息卡片（2026-10-16）：发货后买家看得到承运商与运单号，能一键复制。
MockClient fakeApi(List<Map> shipments) => MockClient((r) async {
      final p = r.url.path.replaceFirst('/api/v1', '');
      if (p == '/stores/resolve') return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true}]});
      if (p == '/orders/N1') {
        return j({'order_no': 'N1', 'store_id': 1, 'status': shipments.isEmpty ? 20 : 30, 'refund_status': 0, 'payable_cents': 5600,
          'created_at': '2026-10-16T00:05:12Z', 'shipments': shipments,
          'items': [{'id': 7, 'sku_id': 1, 'title': '亚麻四件套', 'price_cents': 5600, 'quantity': 1, 'amount_cents': 5600}]});
      }
      return j({'type': 'x', 'title': 'nope $p', 'status': 404}, 404);
    });

Future<Widget> app(MockClient http) async {
  SharedPreferences.setMockInitialValues({'keel.access': 'a', 'keel.refresh': 'r'});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: http);
  final router = buildRouter(session);
  router.go('/orders/N1');
  return Services(client: client, session: session, store: StoreService(client, locate: () async => null), cart: CartCount(client, session),
      trace: SearchTrace(client), unread: UnreadCount(client, session),
      child: MaterialApp.router(theme: keelTheme(), routerConfig: router));
}

void phone(WidgetTester t) {
  t.view.physicalSize = const Size(1170, 2532);
  t.view.devicePixelRatio = 3;
  addTearDown(t.view.reset);
}

void main() {
  testWidgets('已发货：物流信息卡片显示承运商与运单号，点「复制」进剪贴板', (t) async {
    phone(t);
    String? copied;
    t.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (call) async {
      if (call.method == 'Clipboard.setData') copied = (call.arguments as Map)['text'] as String?;
      return null;
    });
    await t.pumpWidget(await app(fakeApi([
      {'id': 1, 'carrier_code': 'sf', 'carrier_name': '顺丰', 'tracking_no': 'SF123', 'status': 1, 'shipped_at': '2026-10-16T02:00:00Z'},
    ])));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('order.shipments')), findsOneWidget);
    expect(find.text('顺丰 SF123'), findsOneWidget);
    await t.ensureVisible(find.byKey(const Key('order.copyTracking.SF123')));
    await t.tap(find.byKey(const Key('order.copyTracking.SF123')));
    await t.pump();
    expect(copied, 'SF123');
    expect(find.text('运单号已复制'), findsOneWidget);
  });

  testWidgets('未发货：没有物流信息卡片', (t) async {
    phone(t);
    await t.pumpWidget(await app(fakeApi(const [])));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('order.shipments')), findsNothing);
  });
}
