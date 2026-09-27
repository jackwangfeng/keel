import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/address.dart';
import '../api/client.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/quick_cart.dart';
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
      toast(context, '已设为默认地址');
      _load();
    } on ApiFailure catch (f) {
      if (mounted) toast(context, f.message);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: Text(widget.select ? '选择收货地址' : '收货地址', style: KeelText.title),
          backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: _error.isNotEmpty
          ? ErrorCard(message: _error, onRetry: _load)
          : ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
              if (_loaded && _rows.isEmpty) const EmptyState(text: '还没有收货地址'),
              for (final r in _rows)
                GestureDetector(
                  key: Key('address.row.${r.id}'),
                  onTap: () => widget.select ? context.pop(r.id) : _edit(r.id),
                  child: Container(
                    margin: const EdgeInsets.only(bottom: 10),
                    padding: const EdgeInsets.all(14),
                    decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(14)),
                    child: Row(children: [
                      Expanded(
                        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                          Row(children: [
                            Text(r.name, style: KeelText.body.copyWith(fontWeight: FontWeight.w700)),
                            const SizedBox(width: 8),
                            Text(r.phone, style: KeelText.sub),
                            if (r.isDefault) ...[
                              const SizedBox(width: 8),
                              Container(
                                key: Key('address.default.${r.id}'),
                                padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 1),
                                decoration: BoxDecoration(color: KeelColors.primary, borderRadius: BorderRadius.circular(4)),
                                child: const Text('默认', style: TextStyle(fontSize: 11, color: KeelColors.card)),
                              ),
                            ],
                            if (r.tagText.isNotEmpty) ...[const SizedBox(width: 6), Text(r.tagText, style: KeelText.hint)],
                          ]),
                          const SizedBox(height: 4),
                          Text(r.fullText, style: KeelText.sub),
                        ]),
                      ),
                      if (!widget.select && !r.isDefault)
                        TextButton(key: Key('address.makeDefault.${r.id}'), onPressed: () => _makeDefault(r.id), child: const Text('设为默认')),
                      if (!widget.select)
                        IconButton(key: Key('address.edit.${r.id}'), onPressed: () => _edit(r.id), icon: const Icon(Icons.edit_outlined, size: 18)),
                    ]),
                  ),
                ),
            ]),
      bottomNavigationBar: SafeArea(
        child: Padding(
          padding: const EdgeInsets.fromLTRB(16, 8, 16, 10),
          child: FilledButton(key: const Key('address.new'), onPressed: () => _edit(null), child: const Text('新建收货地址')),
        ),
      ),
    );
  }
}
