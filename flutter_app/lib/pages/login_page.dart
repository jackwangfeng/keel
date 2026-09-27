import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/services.dart';
import '../api/view.dart';
import '../theme.dart';

class LoginPage extends StatefulWidget {
  const LoginPage({super.key, required this.from});
  final String from;
  @override
  State<LoginPage> createState() => _LoginPageState();
}

class _LoginPageState extends State<LoginPage> {
  // 预填公开的演示买家（与 uni-app x 的登录页同一个），直接点登录就行；
  // 要预填别的账号，编译时加 --dart-define=KEEL_DEMO_PHONE=... / KEEL_DEMO_PASSWORD=...。
  final _phone = TextEditingController(text: const String.fromEnvironment('KEEL_DEMO_PHONE', defaultValue: '13800000000'));
  final _password = TextEditingController(text: const String.fromEnvironment('KEEL_DEMO_PASSWORD', defaultValue: 'keel-demo-2026'));
  String _message = '';
  bool _busy = false;

  Future<void> _submit() async {
    if (_busy) return;
    final s = Services.of(context);
    setState(() {
      _busy = true;
      _message = '';
    });
    try {
      await login(s.client, s.session, _phone.text.trim(), _password.text);
      if (!mounted) return;
      // 登录页是压在外壳上面的（push），成功就退回去：外壳一直在，不会重建出第二个。
      // 直接打开 /login 的（刷新、深链）没有可退的，才按 from 跳。
      if (context.canPop()) {
        context.pop();
      } else {
        context.go(widget.from.isEmpty ? '/me' : widget.from);
      }
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() => _message = f.message);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  /// 一行：左边 72 宽的标签、右边输入框，52 高，底下一条细线（uni-app x 的 .field）。
  Widget _field(String label, Widget input) => Container(
        height: 52,
        decoration: const BoxDecoration(border: Border(bottom: BorderSide(color: KeelColors.line))),
        child: Row(children: [
          SizedBox(width: 72, child: Text(label, style: const TextStyle(fontSize: 14, color: KeelColors.textSub))),
          Expanded(child: input),
        ]),
      );

  @override
  Widget build(BuildContext context) {
    // 服务端刻意让 A 店签发的令牌在 B 店被拒：把当前是哪家店低调地写出来（同源相对地址就不写）。
    final base = Services.maybeOf(context)?.client.base ?? '';
    return Scaffold(
        bottomNavigationBar: base.contains('://')
            ? SafeArea(child: Padding(padding: const EdgeInsets.all(16),
                child: Text('当前店铺 $base', key: const Key('login.base'), textAlign: TextAlign.center, style: KeelText.hint)))
            : null,
        appBar: AppBar(),
        body: ListView(padding: const EdgeInsets.fromLTRB(28, 32, 28, 24), children: [
          Align(
            alignment: Alignment.centerLeft,
            child: Container(
              width: 56, height: 56,
              alignment: Alignment.center,
              decoration: BoxDecoration(color: KeelColors.primary, borderRadius: BorderRadius.circular(18)),
              child: const Text('K', style: TextStyle(fontSize: 26, fontWeight: FontWeight.w700, color: KeelColors.bg)),
            ),
          ),
          const SizedBox(height: 28),
          const Text('欢迎回来', style: KeelText.display),
          const SizedBox(height: 6),
          const Text('登录后即可下单、查看订单', style: KeelText.sub),
          const SizedBox(height: 36),
          _field('手机号', TextField(key: const Key('login.phone'), controller: _phone, keyboardType: TextInputType.phone,
              style: const TextStyle(fontSize: 15, color: KeelColors.text),
              decoration: const InputDecoration(border: InputBorder.none, hintText: '请输入手机号', isCollapsed: true))),
          _field('密码', TextField(key: const Key('login.password'), controller: _password, obscureText: true,
              style: const TextStyle(fontSize: 15, color: KeelColors.text),
              decoration: const InputDecoration(border: InputBorder.none, hintText: '请输入密码', isCollapsed: true))),
          const SizedBox(height: 36),
          FilledButton(key: const Key('login.submit'), onPressed: _busy ? null : _submit,
              child: Text(_busy ? '登录中…' : '登录')),
          if (_message.isNotEmpty)
            Padding(padding: const EdgeInsets.only(top: 14),
                child: Text(_message, key: const Key('login.message'), textAlign: TextAlign.center, style: KeelText.err)),
          if (Services.maybeOf(context)?.session.loggedIn ?? false)
            Padding(
              padding: const EdgeInsets.only(top: 24),
              child: Center(
                child: GestureDetector(
                  key: const Key('login.logout'),
                  onTap: () async {
                    final s = Services.of(context);
                    await logout(s.client, s.session);
                    if (mounted) setState(() {});
                  },
                  child: const Text('退出当前账号', style: KeelText.link),
                ),
              ),
            ),
        ]),
      );
  }
}
