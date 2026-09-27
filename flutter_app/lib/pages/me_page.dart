import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/services.dart';
import '../api/view.dart';
import '../theme.dart';

class MePage extends StatelessWidget {
  const MePage({super.key});

  @override
  Widget build(BuildContext context) {
    final s = Services.of(context);
    return ListenableBuilder(
      listenable: s.session,
      builder: (context, _) => Scaffold(
        body: SafeArea(
          child: ListView(padding: const EdgeInsets.all(20), children: [
            if (s.session.loggedIn) ...[
              Text(s.session.nickname.isEmpty ? '买家' : s.session.nickname, key: const Key('me.nickname'), style: KeelText.title),
              const Text('欢迎回来', style: KeelText.sub),
              const SizedBox(height: 24),
              OutlinedButton(key: const Key('me.logout'), onPressed: () => logout(s.client, s.session),
                  child: const Text('退出登录')),
            ] else ...[
              const Text('登录 / 注册', style: KeelText.title),
              const Text('登录后可以下单、查看订单', style: KeelText.sub),
              const SizedBox(height: 24),
              FilledButton(key: const Key('me.login'), onPressed: () => context.push('/login?from=/me'),
                  child: const Text('登录')),
            ],
            const SizedBox(height: 40),
            const Center(child: Text('Keel 买家端 · Flutter', style: KeelText.hint)),
          ]),
        ),
      ),
    );
  }
}
