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

class Fake {
  int orderStatus = 20;
  int refundStatus = 10;
  int refundType = 1;
  Map? created;
  final shipments = <Map>[];
  final calls = <String>[];
  Map<String, dynamic> refund() => {
        'refund_no': 'R9', 'order_no': 'N1', 'refund_type': refundType, 'status': refundStatus, 'amount_cents': 5600,
        'items': [{'order_item_id': 7, 'title': '亚麻四件套', 'quantity': 1, 'amount_cents': 5600}], 'created_at': '2026-09-26T00:05:12Z',
      };
  late final client = MockClient((r) async {
    final p = r.url.path.replaceFirst('/api/v1', '');
    calls.add('${r.method} $p');
    if (p == '/stores/resolve') return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true}]});
    if (p == '/orders/N1') {
      return j({'order_no': 'N1', 'store_id': 1, 'status': orderStatus, 'refund_status': 0, 'payable_cents': 5600,
        'created_at': '2026-09-26T00:05:12Z',
        'items': [{'id': 7, 'sku_id': 1, 'title': '亚麻四件套', 'price_cents': 5600, 'quantity': 1, 'amount_cents': 5600}]});
    }
    if (p == '/orders/N1/refunds' && r.method == 'POST') {
      created = jsonDecode(r.body) as Map;
      return j(refund(), 201);
    }
    if (p == '/refunds/R9') return j(refund());
    if (p == '/refunds/R9/cancel') {
      refundStatus = 60;
      return j(refund());
    }
    if (p == '/refunds/R9/return-shipment') shipments.add(jsonDecode(r.body) as Map);
    if (p == '/refunds/R9/return-shipment') return j({...refund(), 'return_shipment': {...(jsonDecode(r.body) as Map), 'submitted_at': '2026-09-26T01:00:00Z'}});
    return j({'type': 'x', 'title': 'nope $p', 'status': 404}, 404);
  });
}

Future<Widget> app(Fake f, String at) async {
  SharedPreferences.setMockInitialValues({'keel.access': 'a', 'keel.refresh': 'r'});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: f.client);
  final router = buildRouter(session);
  router.go(at);
  return Services(client: client, session: session, store: StoreService(client), cart: CartCount(client, session),
      trace: SearchTrace(client), unread: UnreadCount(client, session),
      child: MaterialApp.router(theme: keelTheme(), routerConfig: router));
}

void phone(WidgetTester t) {
  t.view.physicalSize = const Size(1170, 2532);
  t.view.devicePixelRatio = 3;
  addTearDown(t.view.reset);
}

void main() {
  testWidgets('申请：未发货只能仅退款；一行时默认全选；选了类型与原因才能交；请求体不带金额；交完到售后详情', (t) async {
    phone(t);
    final f = Fake();
    await t.pumpWidget(await app(f, '/orders/N1/refund'));
    await t.pumpAndSettle();
    expect(t.widget<ChoiceChip>(find.byKey(const Key('apply.type.2'))).onSelected, isNull);
    expect(t.widget<FilledButton>(find.byKey(const Key('apply.submit'))).onPressed, isNull);
    await t.tap(find.byKey(const Key('apply.type.1')));
    await t.tap(find.byKey(const Key('apply.reason.5')));
    await t.pump();
    expect(t.widget<FilledButton>(find.byKey(const Key('apply.submit'))).onPressed, isNull, reason: '「其他」要写说明');
    await t.enterText(find.byKey(const Key('apply.text')), '颜色不对');
    await t.pump();
    await t.tap(find.byKey(const Key('apply.submit')));
    await t.pumpAndSettle();
    expect(f.created, {'items': [{'order_item_id': 7, 'quantity': 1}], 'refund_type': 1, 'reason_code': 5, 'reason_text': '颜色不对'});
    expect(t.widget<Text>(find.byKey(const Key('refund.status'))).data, '待审核');
  });

  testWidgets('详情：退货退款待寄回时填物流；撤回要点两下', (t) async {
    phone(t);
    final f = Fake()
      ..refundType = 2
      ..refundStatus = 20;
    await t.pumpWidget(await app(f, '/refunds/R9'));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('refund.carrier.sf')));
    await t.enterText(find.byKey(const Key('refund.tracking')), 'SF123');
    await t.pump();
    await t.tap(find.byKey(const Key('refund.ship')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('refund.returnFilled')), findsOneWidget);
    expect(find.text('修改物流信息'), findsOneWidget);
    // 再改一次：换承运商与单号，发出去的是新的。
    await t.tap(find.byKey(const Key('refund.carrier.jd')));
    await t.enterText(find.byKey(const Key('refund.tracking')), 'JD456');
    await t.pump();
    await t.tap(find.byKey(const Key('refund.ship')));
    await t.pumpAndSettle();
    expect(f.shipments.last, {'carrier_code': 'jd', 'tracking_no': 'JD456'});
    expect(find.textContaining('JD456'), findsWidgets);
    await t.scrollUntilVisible(find.byKey(const Key('refund.cancel')), 200, scrollable: find.byType(Scrollable).first);
    await t.tap(find.byKey(const Key('refund.cancel')));
    await t.pump();
    expect(f.calls.where((c) => c.endsWith('/cancel')), isEmpty);
    await t.tap(find.byKey(const Key('refund.cancel')));
    await t.pumpAndSettle();
    expect(t.widget<Text>(find.byKey(const Key('refund.status'))).data, '已取消');
  });
}
