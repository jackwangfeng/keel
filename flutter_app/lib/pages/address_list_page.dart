import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/address.dart';
import '../api/client.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/badge.dart';
import '../widgets/states.dart';

/// 地址簿。select = true（从结算页进来）时点一条就是选它：pop 回去它的 id。
class AddressListPage extends StatefulWidget {
  const AddressListPage({super.key, this.select = false});
  final bool select;
  @override
  State<AddressListPage> createState() => _AddressListPageState();
}

class _AddressListPageState extends State<AddressListPage> {
  List<AddressRow> _rows = const [];
  bool _loaded = false;
  String _error = '';
  String _message = '';
  bool _failed = false;
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _load();
    }
  }

  Future<void> _load() async {
    try {
      final rows = await fetchAddresses(Services.of(context).client);
      if (mounted) {
        setState(() {
          _rows = rows;
          _loaded = true;
          _error = '';
        });
      }
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _error = f.message;
          _loaded = true;
        });
      }
    }
  }

  Future<void> _edit(int? id) async {
    await context.push(id == null ? '/addresses/new' : '/addresses/$id');
    if (mounted) _load();
  }

  Future<void> _makeDefault(int id) async {
    try {
      await setDefaultAddress(Services.of(context).client, id);
      if (!mounted) return;
      setState(() {
        _failed = false;
        _message = '已设为默认地址';
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

  Future<void> _remove(int id) async {
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
      await deleteAddress(Services.of(context).client, id);
      if (!mounted) return;
      setState(() {
        _failed = false;
        _message = '已删除';
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
    return Scaffold(
      appBar: AppBar(title: Text(widget.select ? '选择收货地址' : '收货地址'),
          backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: _error.isNotEmpty
          ? ErrorCard(message: _error, onRetry: _load)
          : ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
              if (_message.isNotEmpty)
                Container(
                  margin: const EdgeInsets.only(top: 12),
                  padding: const EdgeInsets.all(18),
                  decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
                  child: Text(_message, key: const Key('address.listMessage'), style: _failed ? KeelText.err : KeelText.ok),
                ),
              if (_rows.isNotEmpty && !_rows.first.isDefault)
                const Padding(padding: EdgeInsets.only(top: 12), child: Text('还没有默认地址，下单时要手动选择', style: KeelText.hint)),
              if (_loaded && _rows.isEmpty) const EmptyState(text: '还没有收货地址'),
              for (final r in _rows)
                GestureDetector(
                  key: Key('address.row.${r.id}'),
                  behavior: HitTestBehavior.opaque,
                  onTap: () => widget.select ? context.pop(r.id) : _edit(r.id),
                  child: Container(
                    margin: const EdgeInsets.only(top: 12),
                    padding: const EdgeInsets.all(18),
                    decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
                    child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                      Wrap(crossAxisAlignment: WrapCrossAlignment.center, spacing: 10, runSpacing: 4, children: [
                        Text(r.name, style: KeelText.section),
                        Text(r.phone, style: KeelText.sub),
                        if (r.isDefault) ToneBadge(key: Key('address.default.${r.id}'), text: '默认', tone: 'ok'),
                        if (r.tagText.isNotEmpty) ToneBadge(text: r.tagText, tone: 'muted'),
                      ]),
                      const SizedBox(height: 4),
                      Text(r.fullText, style: KeelText.hint),
                      const Divider(height: 29, color: KeelColors.line),
                      Row(children: [
                        r.isDefault
                            ? const Text('默认地址', style: KeelText.hint)
                            : GestureDetector(key: Key('address.makeDefault.${r.id}'), onTap: () => _makeDefault(r.id),
                                child: const Text('设为默认', style: KeelText.link)),
                        const Spacer(),
                        GestureDetector(key: Key('address.edit.${r.id}'), onTap: () => _edit(r.id), child: const Text('编辑', style: KeelText.link)),
                        const SizedBox(width: 20),
                        GestureDetector(key: Key('address.remove.${r.id}'), onTap: () => _remove(r.id),
                            child: const Text('删除', style: TextStyle(fontSize: 13, color: KeelColors.err))),
                      ]),
                    ]),
                  ),
                ),
            ]),
      bottomNavigationBar: Container(
        decoration: const BoxDecoration(color: KeelColors.card, border: Border(top: BorderSide(color: KeelColors.line))),
        child: SafeArea(
          top: false,
          child: Padding(
            padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
            child: FilledButton(key: const Key('address.new'), onPressed: () => _edit(null), child: const Text('新建收货地址')),
          ),
        ),
      ),
    );
  }
}
