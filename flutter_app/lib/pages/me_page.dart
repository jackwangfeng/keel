import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/services.dart';
import '../api/view.dart';
import '../tabs.dart';
import '../theme.dart';

/// 我的：头像 + 昵称、个人资料、消息（未读角标）、收货地址、订单、售后、优惠券、领券中心、服务地址、退出。
class MePage extends StatefulWidget {
  const MePage({super.key});
  @override
  State<MePage> createState() => _MePageState();
}

class _MePageState extends State<MePage> {
  String _message = '';
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      Tabs.current.addListener(_onTab);
    }
  }

  @override
  void dispose() {
    Tabs.current.removeListener(_onTab);
    super.dispose();
  }

  void _onTab() {
    if (Tabs.current.value == Tabs.me && mounted) {
      Services.of(context).unread.refresh();
      setState(() => _message = '');
    }
  }

  /// 要登录的入口：没登录先去登录。
  void _go(String route, {bool needLogin = true}) {
    final s = Services.of(context);
    if (needLogin && !s.session.loggedIn) {
      context.push('/login');
      return;
    }
    context.push(route);
  }

  Future<void> _logout() async {
    final s = Services.of(context);
    await logout(s.client, s.session);
    if (mounted) setState(() => _message = '已退出');
  }

  String _host(String base) {
    final u = Uri.tryParse(base);
    if (u == null || !u.hasScheme) return '本站';
    return u.authority;
  }

  @override
  Widget build(BuildContext context) {
    final s = Services.of(context);
    return ListenableBuilder(
      listenable: Listenable.merge([s.session, s.unread]),
      builder: (context, _) {
        final loggedIn = s.session.loggedIn;
        final name = s.session.nickname.isEmpty ? '买家' : s.session.nickname;
        final unread = s.unread.n;
        Widget item(String key, String title, VoidCallback onTap, {Widget? trailing}) => ListTile(
              key: Key(key),
              title: Text(title, style: KeelText.body),
              trailing: Row(mainAxisSize: MainAxisSize.min, children: [
                ?trailing,
                const Icon(Icons.chevron_right, color: KeelColors.textHint),
              ]),
              onTap: onTap,
            );
        return Scaffold(
          body: SafeArea(
            child: ListView(padding: const EdgeInsets.all(16), children: [
              GestureDetector(
                key: const Key('me.head'),
                onTap: () => loggedIn ? context.push('/profile') : context.push('/login?from=/me'),
                child: Padding(
                  padding: const EdgeInsets.symmetric(vertical: 16),
                  child: Row(children: [
                    CircleAvatar(
                      radius: 28,
                      backgroundColor: KeelColors.primary,
                      child: Text(loggedIn ? name.substring(0, 1) : '客',
                          style: const TextStyle(fontSize: 22, color: KeelColors.card, fontWeight: FontWeight.w700)),
                    ),
                    const SizedBox(width: 14),
                    Expanded(
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        loggedIn
                            ? Text(name, key: const Key('me.nickname'), style: KeelText.title)
                            : const Text('登录 / 注册', key: Key('me.login'), style: KeelText.title),
                        Text(loggedIn ? '欢迎回来' : '登录后可以下单、查看订单', style: KeelText.sub),
                      ]),
                    ),
                    const Icon(Icons.chevron_right, color: KeelColors.textHint),
                  ]),
                ),
              ),
              Card(
                child: Column(children: [
                  if (loggedIn) item('me.profile', '个人资料', () => context.push('/profile')),
                  if (loggedIn)
                    item('me.notifications', '消息', () => context.push('/notifications'),
                        trailing: unread > 0
                            ? Container(
                                key: const Key('me.unread'),
                                margin: const EdgeInsets.only(right: 4),
                                padding: const EdgeInsets.symmetric(horizontal: 7, vertical: 1),
                                decoration: BoxDecoration(color: KeelColors.err, borderRadius: BorderRadius.circular(10)),
                                child: Text(unread > 99 ? '99+' : '$unread', style: const TextStyle(fontSize: 11, color: KeelColors.card)),
                              )
                            : null),
                  item('me.addresses', '收货地址', () => _go('/addresses')),
                  item('me.orders', '我的订单', () => context.go('/orders')),
                  item('me.refunds', '售后 / 退款', () => _go('/refunds')),
                  // 两个券页自己会在未登录时显示登录入口，这里不拦。
                  item('me.coupons', '我的优惠券', () => _go('/coupons', needLogin: false)),
                  item('me.couponCenter', '领券中心', () => _go('/coupon-center', needLogin: false)),
                  item('me.settings', '服务地址', () => _go('/settings', needLogin: false),
                      trailing: Text(_host(s.client.base), style: KeelText.hint)),
                ]),
              ),
              if (loggedIn)
                Card(
                  child: ListTile(
                    key: const Key('me.logout'),
                    title: const Center(child: Text('退出登录', style: TextStyle(color: KeelColors.accent))),
                    onTap: _logout,
                  ),
                ),
              if (_message.isNotEmpty) Center(child: Text(_message, key: const Key('me.message'), style: KeelText.hint)),
              const SizedBox(height: 24),
              const Center(child: Text('Keel 买家端 · Flutter', style: KeelText.hint)),
            ]),
          ),
        );
      },
    );
  }
}
