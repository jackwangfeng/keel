import 'package:flutter/foundation.dart';

import 'client.dart';
import 'schema.g.dart';
import 'session.dart';
import 'view.dart';

/// 消息中心。标题 / 正文是服务端渲染好的，原样显示，不按 kind 拼文案（模板在服务端）。

class NotificationRow {
  final int id;
  final String title;
  final String body;
  final String time;
  /// 页面上点了就先标掉（不等请求）：点进去再回来这条不该还亮着。
  bool unread;
  /// 点了跳到哪（订单详情 / 售后详情）；跳不了是空串。
  final String route;
  NotificationRow({required this.id, required this.title, required this.body, required this.time, required this.unread,
      required this.route});
}

NotificationRow notificationRow(Notification n) {
  final t = n.target;
  var route = '';
  if (t.type == 'refund' && t.refundNo != null) {
    route = '/refunds/${Uri.encodeComponent(t.refundNo!)}';
  } else if (t.type == 'order' && t.orderNo != null) {
    route = '/orders/${Uri.encodeComponent(t.orderNo!)}';
  }
  return NotificationRow(id: n.id, title: n.title, body: n.body, time: shortTime(n.createdAt), unread: n.readAt == null, route: route);
}

class NotificationPage {
  final List<NotificationRow> rows;
  final int total;
  /// 未读总数（与分页无关）。
  final int unread;
  const NotificationPage(this.rows, this.total, this.unread);
}

Future<NotificationPage> fetchNotifications(ApiClient c, {int page = 1, int pageSize = 20}) async {
  final res = await c.send('GET', '/me/notifications', query: {'page': '$page', 'page_size': '$pageSize'},
      decode: (j) => NotificationList.fromJson(j as Map<String, dynamic>));
  return NotificationPage(res.data.items.map(notificationRow).toList(), res.data.total, res.data.unreadCount);
}

int _unread(dynamic j) => NotificationUnreadCount.fromJson(j as Map<String, dynamic>).unreadCount;

/// 读一条，返回服务端的未读数。
Future<int> markNotificationRead(ApiClient c, int id) async =>
    (await c.send('POST', '/me/notifications/$id/read', decode: _unread)).data;

Future<int> markAllNotificationsRead(ApiClient c) async =>
    (await c.send('POST', '/me/notifications/read-all', decode: _unread)).data;

/// 未读数（「我的」tab 与「消息」入口的角标）。没登录是 0；查失败不打扰 —— 角标只是提示。
class UnreadCount extends ChangeNotifier {
  UnreadCount(this._c, this._session) {
    _session.addListener(_onSession);
  }
  final ApiClient _c;
  final Session _session;
  int n = 0;

  void set(int v) {
    if (v == n) return;
    n = v;
    notifyListeners();
  }

  Future<void> refresh() async {
    if (!_session.loggedIn) return set(0);
    try {
      set((await _c.send('GET', '/me/notifications/unread-count', decode: _unread)).data);
    } on ApiFailure {
      // 角标只是提示。
    }
  }

  void _onSession() => _session.loggedIn ? refresh() : set(0);

  @override
  void dispose() {
    _session.removeListener(_onSession);
    super.dispose();
  }
}
