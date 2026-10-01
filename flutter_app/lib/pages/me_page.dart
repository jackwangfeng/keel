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
        // 一行 56 高、左右 18，行间一条从 18 开始的细线，右边浅色的「›」（uni-app x 的 .item / .line / .chev）。
        Widget item(String key, String title, VoidCallback onTap, {Widget? trailing}) => InkWell(
              key: Key(key),
              onTap: onTap,
              child: SizedBox(
                height: 56,
                child: Padding(
                  padding: const EdgeInsets.symmetric(horizontal: 18),
                  child: Row(children: [
                    Expanded(child: Text(title, style: KeelText.body)),
                    ?trailing,
                    const Padding(padding: EdgeInsets.only(left: 8), child: Text('›', style: TextStyle(fontSize: 20, color: Color(0xFFC4B8AA)))),
                  ]),
                ),
              ),
            );
        Widget lines(List<Widget> rows) => Column(children: [
              for (final (i, r) in rows.indexed) ...[
                if (i > 0) const Divider(height: 1, thickness: 1, indent: 18, color: KeelColors.line),
                r,
              ],
            ]);
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
                      radius: 30,
                      backgroundColor: KeelColors.primary,
                      child: Text(loggedIn ? name.substring(0, 1) : '客',
                          style: const TextStyle(fontSize: 24, color: KeelColors.bg, fontWeight: FontWeight.w700)),
                    ),
                    const SizedBox(width: 16),
                    Expanded(
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        loggedIn
                            ? Text(name, key: const Key('me.nickname'), style: KeelText.title)
                            : const Text('登录 / 注册', key: Key('me.login'), style: KeelText.title),
                        Text(loggedIn ? '欢迎回来' : '登录后可以下单、查看订单', style: KeelText.sub),
                      ]),
                    ),
                    const Text('›', style: TextStyle(fontSize: 20, color: Color(0xFFC4B8AA))),
                  ]),
                ),
              ),
              Container(
                margin: const EdgeInsets.only(top: 12),
                clipBehavior: Clip.antiAlias,
                decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
                child: lines([
                  if (loggedIn) item('me.profile', '个人资料', () => context.push('/profile')),
                  if (loggedIn)
                    item('me.notifications', '消息', () => context.push('/notifications'),
                        trailing: unread > 0
                            ? Container(
                                key: const Key('me.unread'),
                                padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 3),
                                decoration: BoxDecoration(color: const Color(0xFFF3DDD5), borderRadius: BorderRadius.circular(10)),
                                child: Text(unread > 99 ? '99+' : '$unread', style: const TextStyle(fontSize: 12, color: Color(0xFF9C3F28))),
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
                Container(
                  margin: const EdgeInsets.only(top: 12),
                  clipBehavior: Clip.antiAlias,
                  decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
                  child: InkWell(
                    key: const Key('me.logout'),
                    onTap: _logout,
                    child: const SizedBox(height: 56, child: Center(child: Text('退出登录', style: KeelText.link))),
                  ),
                ),
              // 只在未登录时显示（「已退出」）：从「我的」页直接再登录回来，tab 没切走过，_onTab 不会清它。
              if (_message.isNotEmpty && !loggedIn) Center(child: Text(_message, key: const Key('me.message'), style: KeelText.hint)),
              const SizedBox(height: 40),
              const Center(child: Text('Keel 买家端 · Flutter', style: KeelText.hint)),
            ]),
          ),
        );
      },
    );
  }
}
