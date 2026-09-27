import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/notification.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/states.dart';

/// 消息中心：分页、点一条（标已读并跳订单 / 售后）、全部已读。
class NotificationsPage extends StatefulWidget {
  const NotificationsPage({super.key});
  @override
  State<NotificationsPage> createState() => _NotificationsPageState();
}

class _NotificationsPageState extends State<NotificationsPage> {
  static const _pageSize = 20;
  List<NotificationRow> _rows = [];
  int _total = 0;
  int _page = 1;
  int _unread = 0;
  bool _loaded = false;
  bool _loading = false;
  String _error = '';
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _fetch(1);
    }
  }

  Future<void> _fetch(int page) async {
    if (_loading) return;
    final s = Services.of(context);
    setState(() {
      _loading = true;
      _error = '';
    });
    try {
      final p = await fetchNotifications(s.client, page: page, pageSize: _pageSize);
      if (!mounted) return;
      s.unread.set(p.unread);
      setState(() {
        _rows = page == 1 ? p.rows : [..._rows, ...p.rows];
        _total = p.total;
        _page = page;
        _unread = p.unread;
        _loaded = true;
        _loading = false;
      });
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _error = f.message;
          _loaded = true;
          _loading = false;
        });
      }
    }
  }

  Future<void> _open(NotificationRow r) async {
    final s = Services.of(context);
    if (r.unread) {
      // 先在页面上标掉，不等请求。
      setState(() {
        r.unread = false;
        _unread = _unread > 0 ? _unread - 1 : 0;
      });
      markNotificationRead(s.client, r.id).then((n) {
        s.unread.set(n);
        if (mounted) setState(() => _unread = n);
      }, onError: (_) {});
    }
    if (r.route.isEmpty) return;
    await context.push(r.route);
    // 从订单 / 售后详情回来时，别处可能又产生了新通知。
    if (mounted) _fetch(1);
  }

  Future<void> _readAll() async {
    final s = Services.of(context);
    try {
      final n = await markAllNotificationsRead(s.client);
      s.unread.set(n);
      if (!mounted) return;
      setState(() {
        for (final r in _rows) {
          r.unread = false;
        }
        _unread = n;
      });
    } on ApiFailure catch (f) {
      if (mounted) setState(() => _error = f.message);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('消息', style: KeelText.title), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: RefreshIndicator(
        onRefresh: () => _fetch(1),
        child: ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
          if (_rows.isNotEmpty)
            Row(children: [
              Text(_unread > 0 ? '$_unread 条未读' : '全部已读', key: const Key('notes.unread'), style: KeelText.hint),
              const Spacer(),
              if (_unread > 0) TextButton(key: const Key('notes.readAll'), onPressed: _readAll, child: const Text('全部标为已读')),
            ]),
          if (_error.isNotEmpty) ErrorCard(message: _error, onRetry: () => _fetch(1)),
          for (final r in _rows)
            GestureDetector(
              key: Key('notes.row.${r.id}'),
              onTap: () => _open(r),
              child: Container(
                margin: const EdgeInsets.only(bottom: 10),
                padding: const EdgeInsets.all(14),
                decoration: BoxDecoration(color: r.unread ? const Color(0xFFFFF8EE) : KeelColors.card, borderRadius: BorderRadius.circular(14)),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  Row(children: [
                    if (r.unread)
                      Container(key: Key('notes.dot.${r.id}'), width: 8, height: 8, margin: const EdgeInsets.only(right: 6),
                          decoration: const BoxDecoration(color: KeelColors.err, shape: BoxShape.circle)),
                    Expanded(child: Text(r.title, style: KeelText.section)),
                    Text(r.time, style: KeelText.hint),
                  ]),
                  const SizedBox(height: 4),
                  Text(r.body, style: KeelText.sub),
                ]),
              ),
            ),
          if (_rows.length < _total)
            Center(child: TextButton(key: const Key('notes.more'), onPressed: () => _fetch(_page + 1),
                child: Text(_loading ? '加载中…' : '加载更多'))),
          if (_loaded && _rows.isEmpty && _error.isEmpty)
            const Padding(
              padding: EdgeInsets.only(top: 60),
              child: Column(children: [
                Text('还没有消息', key: Key('notes.empty'), style: KeelText.sub),
                Text('支付、发货、售后进度会在这里通知你', style: KeelText.hint),
              ]),
            ),
        ]),
      ),
    );
  }
}
