import 'package:flutter/material.dart';

import '../api/client.dart';
import '../api/profile.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/form_bits.dart';
import '../widgets/states.dart';

/// 个人资料：昵称、性别（只带改了的字段）、第三方账号绑定（解绑）。
class ProfilePage extends StatefulWidget {
  const ProfilePage({super.key});
  @override
  State<ProfilePage> createState() => _ProfilePageState();
}

class _ProfilePageState extends State<ProfilePage> {
  MeView? _me;
  final _nick = TextEditingController();
  int _gender = 0;
  List<IdentityRow> _ids = const [];
  String _error = '';
  String _message = '';
  bool _failed = false;
  String _nickErr = '';
  bool _busy = false;
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _load();
    }
  }

  @override
  void dispose() {
    _nick.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    final c = Services.of(context).client;
    try {
      final me = await fetchMe(c);
      if (!mounted) return;
      setState(() {
        _me = me;
        _nick.text = me.nickname;
        _gender = me.gender;
        _error = '';
      });
    } on ApiFailure catch (f) {
      if (mounted) setState(() => _error = f.message);
    }
    try {
      final ids = await fetchIdentities(c);
      if (mounted) setState(() => _ids = ids);
    } on ApiFailure {
      if (mounted) setState(() => _ids = const []);
    }
  }

  bool get _dirty => _me != null && (_nick.text != _me!.nickname || _gender != _me!.gender);

  Future<void> _save() async {
    final m = _me;
    if (m == null || _busy || !_dirty) return;
    final s = Services.of(context);
    setState(() {
      _busy = true;
      _message = '';
      _nickErr = '';
    });
    try {
      final v = await updateMe(s.client, s.session,
          nickname: _nick.text != m.nickname ? _nick.text : null, gender: _gender != m.gender ? _gender : null);
      if (!mounted) return;
      setState(() {
        _busy = false;
        _me = v;
        _nick.text = v.nickname;
        _gender = v.gender;
        _failed = false;
        _message = '已保存';
      });
    } on ApiFailure catch (f) {
      if (!mounted) return;
      final e = f.fieldErrors.where((e) => e.field == 'nickname').firstOrNull;
      setState(() {
        _busy = false;
        _failed = true;
        _nickErr = e == null ? '' : ((e.message ?? '').isNotEmpty ? e.message! : '昵称不合要求');
        _message = _nickErr.isNotEmpty ? '请检查标红的一项' : f.message;
      });
    }
  }

  Future<void> _unbind(IdentityRow r) async {
    final ok = await showDialog<bool>(
      context: context,
      builder: (d) => AlertDialog(
        title: Text('解绑${r.name}？'),
        content: const Text('解绑后不能再用它登录这个账号。'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(d, false), child: const Text('取消')),
          TextButton(onPressed: () => Navigator.pop(d, true), child: const Text('解绑')),
        ],
      ),
    );
    if (ok != true || !mounted) return;
    try {
      await unbindIdentity(Services.of(context).client, r.provider);
      if (!mounted) return;
      setState(() {
        _failed = false;
        _message = '已解绑${r.name}';
      });
      _load();
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _failed = true;
          _message = f.message;
        });
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final m = _me;
    return Scaffold(
      appBar: AppBar(title: const Text('个人资料'), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: m == null
          ? (_error.isNotEmpty ? ErrorCard(message: _error, onRetry: _load) : const EmptyState(text: '正在加载…'))
          : ListView(padding: const EdgeInsets.fromLTRB(16, 0, 16, 24), children: [
              KeelCard(
                padding: const EdgeInsets.symmetric(horizontal: 16),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  FieldRow(
                    label: '昵称',
                    bad: _nickErr.isNotEmpty,
                    child: TextField(
                      key: const Key('profile.nickname'),
                      controller: _nick,
                      maxLength: 32,
                      onChanged: (_) => setState(() {}),
                      style: inlineInputStyle,
                      decoration: inlineInput('最多 32 个字'),
                    ),
                  ),
                  if (_nickErr.isNotEmpty) Padding(padding: const EdgeInsets.symmetric(vertical: 4), child: Text(_nickErr, style: KeelText.err)),
                  FieldRow(
                    label: '性别',
                    child: Row(children: [
                      for (final (v, l) in [(0, '不设置'), (1, '男'), (2, '女')])
                        Padding(
                          padding: const EdgeInsets.only(right: 8),
                          child: OptChip(key: Key('profile.gender.$v'), label: l, on: _gender == v, small: true,
                              onTap: () => setState(() => _gender = v)),
                        ),
                    ]),
                  ),
                  FieldRow(
                    label: '手机号',
                    last: true,
                    child: Row(children: [
                      Expanded(child: Text(m.phone.isNotEmpty ? m.phone : '未绑定', style: KeelText.body)),
                      const Text('换绑暂未开通', style: KeelText.hint),
                    ]),
                  ),
                ]),
              ),
              if (_message.isNotEmpty)
                KeelCard(child: Text(_message, key: const Key('profile.message'), style: _failed ? KeelText.err : KeelText.ok)),
              const Padding(padding: EdgeInsets.fromLTRB(4, 24, 4, 0), child: Text('第三方账号', style: KeelText.overline)),
              KeelCard(
                padding: const EdgeInsets.symmetric(horizontal: 16),
                child: Column(children: [
                  for (final r in _ids)
                    Container(
                      key: Key('profile.identity.${r.provider}'),
                      constraints: const BoxConstraints(minHeight: 52),
                      decoration: const BoxDecoration(border: Border(bottom: BorderSide(color: KeelColors.line))),
                      child: Row(children: [
                        Expanded(
                          child: Column(crossAxisAlignment: CrossAxisAlignment.start, mainAxisSize: MainAxisSize.min, children: [
                            Text(r.name, style: KeelText.body),
                            Text('绑定于 ${r.boundAt}', style: KeelText.hint),
                          ]),
                        ),
                        GestureDetector(onTap: () => _unbind(r), child: const Text('解绑', style: TextStyle(fontSize: 13, color: KeelColors.err))),
                      ]),
                    ),
                  const SizedBox(
                    height: 52,
                    child: Row(children: [
                      Expanded(child: Text('绑定微信', style: KeelText.body)),
                      Text('暂未开通', style: KeelText.hint),
                    ]),
                  ),
                ]),
              ),
            ]),
      bottomNavigationBar: Container(
        decoration: const BoxDecoration(color: KeelColors.card, border: Border(top: BorderSide(color: KeelColors.line))),
        child: SafeArea(
          top: false,
          child: Padding(
          padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
          child: FilledButton(key: const Key('profile.save'), onPressed: _dirty && !_busy ? _save : null,
              child: Text(_busy ? '保存中…' : '保存')),
        ),
        ),
      ),
    );
  }
}
