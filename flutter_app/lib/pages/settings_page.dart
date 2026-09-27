import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';

import '../api/profile.dart';
import '../api/services.dart';
import '../config.dart';
import '../theme.dart';

/// 服务地址（= 哪一家店 / 租户）。换了之后上一家的登录与门店都作废。
class SettingsPage extends StatefulWidget {
  const SettingsPage({super.key});
  @override
  State<SettingsPage> createState() => _SettingsPageState();
}

class _SettingsPageState extends State<SettingsPage> {
  late final _url = TextEditingController(text: Services.of(context).client.base);
  String _message = '';

  @override
  void dispose() {
    _url.dispose();
    super.dispose();
  }

  Future<void> _save() async {
    final s = Services.of(context);
    final loggedOut = await switchBase(s.client, s.session, s.store, _url.text, fallback: apiBase());
    s.cart.set(0);
    s.unread.set(0);
    if (!mounted) return;
    setState(() {
      _url.text = s.client.base;
      _message = loggedOut ? '已保存，并退出了上一家店的登录' : '已保存';
    });
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('服务地址'), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: ListView(padding: const EdgeInsets.all(16), children: [
        TextField(
          key: const Key('settings.url'),
          controller: _url,
          keyboardType: TextInputType.url,
          decoration: const InputDecoration(labelText: '服务地址', hintText: 'https://<店铺域名>/api/v1'),
        ),
        const SizedBox(height: 8),
        const Text('留空保存 = 回到安装包自带的地址。换了地址就是换了一家店：会退出当前登录。', style: KeelText.hint),
        // Web 版只能访问同源（服务端不发 CORS 头），换成别的域名会连不上。
        if (kIsWeb) const Text('网页版只能用本站的服务地址。', style: KeelText.hint),
        if (_message.isNotEmpty)
          Padding(padding: const EdgeInsets.only(top: 12), child: Text(_message, key: const Key('settings.message'), style: KeelText.ok)),
      ]),
      bottomNavigationBar: SafeArea(
        child: Padding(
          padding: const EdgeInsets.fromLTRB(16, 8, 16, 10),
          child: FilledButton(key: const Key('settings.save'), onPressed: _save, child: const Text('保存')),
        ),
      ),
    );
  }
}
