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
  final _phone = TextEditingController();
  final _password = TextEditingController();
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

  @override
  Widget build(BuildContext context) => Scaffold(
        appBar: AppBar(title: const Text('登录')),
        body: ListView(padding: const EdgeInsets.all(20), children: [
          const Text('欢迎回来', style: KeelText.display),
          const SizedBox(height: 24),
          TextField(key: const Key('login.phone'), controller: _phone, keyboardType: TextInputType.phone,
              decoration: const InputDecoration(labelText: '手机号')),
          TextField(key: const Key('login.password'), controller: _password, obscureText: true,
              decoration: const InputDecoration(labelText: '密码')),
          const SizedBox(height: 24),
          FilledButton(key: const Key('login.submit'), onPressed: _busy ? null : _submit,
              child: Text(_busy ? '登录中…' : '登录')),
          if (_message.isNotEmpty)
            Padding(padding: const EdgeInsets.only(top: 12),
                child: Text(_message, key: const Key('login.message'), style: KeelText.err)),
        ]),
      );
}
