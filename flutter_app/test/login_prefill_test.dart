import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/api/cart_count.dart';
import 'package:keel_buyer/api/catalog.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/notification.dart';
import 'package:keel_buyer/api/services.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';
import 'package:keel_buyer/pages/login_page.dart';

void main() {
  testWidgets('登录页预填演示买家，不用手输', (t) async {
    await t.pumpWidget(const MaterialApp(home: LoginPage(from: '')));
    expect(t.widget<TextField>(find.byKey(const Key('login.phone'))).controller!.text, '13800000000');
    expect(t.widget<TextField>(find.byKey(const Key('login.password'))).controller!.text, 'keel-demo-2026');
  });

  testWidgets('底部写着当前是哪家店（绝对地址时）；同源相对地址不写', (t) async {
    Widget host(String base) {
      final s = Session();
      final c = ApiClient(base: base, session: s);
      return Services(client: c, session: s, store: StoreService(c, locate: () async => null), cart: CartCount(c, s),
          trace: SearchTrace(c), unread: UnreadCount(c, s), child: const MaterialApp(home: LoginPage(from: '')));
    }
    await t.pumpWidget(host('https://shop.example/api/v1'));
    expect(find.text('当前店铺 https://shop.example/api/v1'), findsOneWidget);
    await t.pumpWidget(host('/api/v1'));
    expect(find.textContaining('当前店铺'), findsNothing);
  });
}
