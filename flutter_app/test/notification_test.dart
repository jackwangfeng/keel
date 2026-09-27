import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/notification.dart';
import 'package:keel_buyer/api/schema.g.dart' as s;
import 'package:keel_buyer/api/session.dart';
import 'package:shared_preferences/shared_preferences.dart';

Map<String, dynamic> n(int id, Map<String, dynamic> target, {String? readAt}) => {
      'id': id, 'kind': 'order_shipped', 'title': '已发货', 'body': '你的订单已发货', 'target': target,
      'read_at': ?readAt, 'created_at': '2026-09-26T00:05:12Z',
    };

void main() {
  test('跳转目标：售后 / 订单；跳不了（库存类或字段缺了）是空串；未读看 read_at', () {
    expect(notificationRow(s.Notification.fromJson(n(1, {'type': 'refund', 'refund_no': 'R1'}))).route, '/refunds/R1');
    expect(notificationRow(s.Notification.fromJson(n(2, {'type': 'order', 'order_no': 'N 1'}))).route, '/orders/N%201');
    expect(notificationRow(s.Notification.fromJson(n(3, {'type': 'sku', 'sku_id': 5}))).route, '');
    expect(notificationRow(s.Notification.fromJson(n(4, {'type': 'order'}))).route, '');
    expect(notificationRow(s.Notification.fromJson(n(5, {'type': 'order'}))).unread, isTrue);
    expect(notificationRow(s.Notification.fromJson(n(6, {'type': 'order'}, readAt: '2026-09-26T01:00:00Z'))).unread, isFalse);
  });

  test('未读数：登录后查，退出清零；读一条后用服务端回的数', () async {
    SharedPreferences.setMockInitialValues({'keel.access': 'a', 'keel.refresh': 'r'});
    final session = Session();
    await session.load();
    final c = ApiClient(base: 'http://h/api/v1', session: session, http: MockClient((r) async {
      final body = r.url.path.endsWith('/read') ? {'unread_count': 2} : {'unread_count': 3};
      return http.Response(jsonEncode(body), 200, headers: {'content-type': 'application/json'});
    }));
    final u = UnreadCount(c, session);
    await u.refresh();
    expect(u.n, 3);
    u.set(await markNotificationRead(c, 1));
    expect(u.n, 2);
    await session.clear();
    expect(u.n, 0);
  });
}
