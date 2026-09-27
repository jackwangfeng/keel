import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/address.dart';
import '../api/client.dart';
import '../api/services.dart';
import '../theme.dart';

/// 新建 / 编辑地址。422 按 errors[].field 标红对应的那一格；PUT 不切默认，想切默认 PUT 之后另调专用接口。
class AddressEditPage extends StatefulWidget {
  const AddressEditPage({super.key, this.addressId = 0});
  /// 0 = 新建。
  final int addressId;
  @override
  State<AddressEditPage> createState() => _AddressEditPageState();
}

class _AddressEditPageState extends State<AddressEditPage> {
  AddressForm _form = AddressForm();
  final _c = {for (final k in _fields.keys) k: TextEditingController()};
  bool _wantDefault = false;
  Map<String, String> _errors = const {};
  String _message = '';
  bool _failed = false;
  bool _busy = false;
  bool _loading = false;
  // 新建的幂等键：这张表单活着期间不变（超时后再点保存是重放）。同键异体 422 时换一个。
  String _key = newIdempotencyKey();
  bool _started = false;

  static const _fields = {
    'receiverName': ('收货人', '姓名'),
    'phone': ('手机号', '11 位手机号'),
    'province': ('省份', '如 上海市'),
    'city': ('城市', '如 上海市'),
    'district': ('区县', '如 徐汇区'),
    'detail': ('详细地址', '街道、门牌号'),
  };

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      if (widget.addressId > 0) _load();
    }
  }

  @override
  void dispose() {
    for (final c in _c.values) {
      c.dispose();
    }
    super.dispose();
  }

  Future<void> _load() async {
    setState(() => _loading = true);
    try {
      final f = await fetchAddressForm(Services.of(context).client, widget.addressId);
      if (!mounted) return;
      if (f == null) {
        setState(() {
          _loading = false;
          _failed = true;
          _message = '这条地址已经不存在了';
        });
        return;
      }
      _form = f;
      _c['receiverName']!.text = f.receiverName;
      _c['phone']!.text = f.phone;
      _c['province']!.text = f.province;
      _c['city']!.text = f.city;
      _c['district']!.text = f.district;
      _c['detail']!.text = f.detail;
      setState(() {
        _wantDefault = f.isDefault;
        _loading = false;
      });
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _loading = false;
          _failed = true;
          _message = f.message;
        });
      }
    }
  }

  void _collect() {
    _form
      ..receiverName = _c['receiverName']!.text.trim()
      ..phone = _c['phone']!.text.trim()
      ..province = _c['province']!.text.trim()
      ..city = _c['city']!.text.trim()
      ..district = _c['district']!.text.trim()
      ..detail = _c['detail']!.text.trim();
  }

  Future<void> _save() async {
    if (_busy) return;
    _collect();
    final c = Services.of(context).client;
    setState(() {
      _busy = true;
      _errors = const {};
      _message = '';
    });
    try {
      if (widget.addressId == 0) {
        await createAddress(c, _form, wantDefault: _wantDefault, idempotencyKey: _key);
      } else {
        try {
          await updateAddress(c, widget.addressId, _form);
        } on ApiFailure catch (f) {
          // 按理不会走到（带的是当前值）；真走到了就照契约改调专用接口。
          if (!f.isType('use-default-endpoint')) rethrow;
        }
        if (_wantDefault && !_form.isDefault) {
          try {
            await setDefaultAddress(c, widget.addressId);
          } on ApiFailure catch (f) {
            // 地址已经存上了，只是没设成默认：照实说，不回退已保存的修改。
            if (!mounted) return;
            setState(() {
              _busy = false;
              _failed = true;
              _message = '地址已保存，但设为默认失败：${f.message}';
            });
            return;
          }
        }
      }
      if (mounted) context.pop(true);
    } on ApiFailure catch (f) {
      if (!mounted) return;
      final errs = formErrors(f);
      setState(() {
        _busy = false;
        _failed = true;
        _errors = errs;
        _message = errs.isNotEmpty ? '请检查标红的几项' : f.message;
        // 同键异体：表单改过了。这次没建成，换一个键下次保存就是一次新请求。
        if (f.status == 422 && errs.isEmpty) _key = newIdempotencyKey();
      });
    }
  }

  Future<void> _remove() async {
    final ok = await showDialog<bool>(
      context: context,
      builder: (d) => AlertDialog(
        title: const Text('删除这条地址？'),
        content: const Text('已下的订单不受影响（订单里存的是收货信息的快照）。'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(d, false), child: const Text('取消')),
          TextButton(key: const Key('address.deleteConfirm'), onPressed: () => Navigator.pop(d, true), child: const Text('删除')),
        ],
      ),
    );
    if (ok != true || !mounted) return;
    try {
      await deleteAddress(Services.of(context).client, widget.addressId);
      if (mounted) context.pop(true);
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
    return Scaffold(
      appBar: AppBar(title: Text(widget.addressId > 0 ? '编辑地址' : '新建地址', style: KeelText.title),
          backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: _loading
          ? const Center(child: Text('正在加载…', style: KeelText.hint))
          : ListView(padding: const EdgeInsets.fromLTRB(16, 8, 16, 24), children: [
              Container(
                padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 4),
                decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(14)),
                child: Column(children: [
                  for (final e in _fields.entries)
                    TextField(
                      key: Key('address.field.${e.key}'),
                      controller: _c[e.key],
                      keyboardType: e.key == 'phone' ? TextInputType.phone : TextInputType.text,
                      decoration: InputDecoration(
                        labelText: e.value.$1,
                        hintText: e.value.$2,
                        errorText: _errors[e.key],
                        border: InputBorder.none,
                      ),
                    ),
                  SwitchListTile(
                    key: const Key('address.default'),
                    contentPadding: EdgeInsets.zero,
                    title: const Text('设为默认地址', style: KeelText.body),
                    value: _wantDefault,
                    onChanged: (v) => setState(() => _wantDefault = v),
                  ),
                ]),
              ),
              if (_message.isNotEmpty)
                Padding(
                  padding: const EdgeInsets.only(top: 12),
                  child: Text(_message, key: const Key('address.message'), style: _failed ? KeelText.err : KeelText.ok),
                ),
              if (widget.addressId > 0)
                Padding(
                  padding: const EdgeInsets.only(top: 20),
                  child: Center(child: TextButton(key: const Key('address.delete'), onPressed: _remove, child: const Text('删除这条地址'))),
                ),
            ]),
      bottomNavigationBar: SafeArea(
        child: Padding(
          padding: const EdgeInsets.fromLTRB(16, 8, 16, 10),
          child: FilledButton(key: const Key('address.save'), onPressed: _busy ? null : _save, child: Text(_busy ? '保存中…' : '保存')),
        ),
      ),
    );
  }
}
