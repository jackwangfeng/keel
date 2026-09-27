import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';

import '../api/profile.dart';
import '../api/services.dart';
import '../config.dart';
import '../theme.dart';
import '../widgets/form_bits.dart';

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
      // 客户端能配的只有这一个地址：单商家部署直接指过去，多商家部署指到那家店的域名。换了地址就是换了一家店。
      body: ListView(padding: const EdgeInsets.fromLTRB(16, 0, 16, 24), children: [
        KeelCard(
          title: '店铺服务地址',
          child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
            FieldRow(
              label: '地址',
              child: TextField(
                key: const Key('settings.url'),
                controller: _url,
                keyboardType: TextInputType.url,
                style: inlineInputStyle,
                decoration: inlineInput('http://192.168.1.10:8080/api/v1'),
              ),
            ),
            const SizedBox(height: 8),
            const Text(kIsWeb ? '每家店有自己的地址，包含 /api/v1 前缀。网页版保持 /api/v1 即可（只能用本站的服务地址）。'
                : '每家店有自己的地址，包含 /api/v1 前缀。留空保存 = 回到安装包自带的地址。', style: KeelText.hint),
            const SizedBox(height: 14),
            FilledButton(key: const Key('settings.save'), onPressed: _save, child: const Text('保存')),
            if (_message.isNotEmpty)
              Padding(padding: const EdgeInsets.only(top: 12),
                  child: Text(_message, key: const Key('settings.message'), textAlign: TextAlign.center, style: KeelText.ok)),
          ]),
        ),
        // 不提醒的话，用户下一次点什么都会撞上一个他解释不了的 401。
        Container(
          margin: const EdgeInsets.only(top: 12),
          padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 12),
          decoration: BoxDecoration(color: const Color(0xFFFBF1DF), borderRadius: BorderRadius.circular(12)),
          child: const Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text('切换地址会退出登录', style: TextStyle(fontSize: 13, fontWeight: FontWeight.w700, color: Color(0xFF8A5A12))),
            SizedBox(height: 2),
            Text('换了地址就是换了一家店，之前的登录状态在新店无效。', style: TextStyle(fontSize: 12, color: Color(0xFF8A5A12))),
          ]),
        ),
        Padding(
          padding: const EdgeInsets.symmetric(vertical: 16),
          child: Center(
            child: ListenableBuilder(
              listenable: Services.of(context).session,
              builder: (context, _) => Text(
                  '当前：${Services.of(context).client.base}${Services.of(context).session.loggedIn ? '（已登录）' : '（未登录）'}',
                  style: KeelText.hint),
            ),
          ),
        ),
      ]),
    );
  }
}
