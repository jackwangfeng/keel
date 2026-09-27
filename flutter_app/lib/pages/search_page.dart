import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/catalog.dart';
import '../api/client.dart';
import '../api/services.dart';
import '../api/view.dart';
import '../theme.dart';
import '../widgets/product_card.dart';
import '../widgets/quick_cart.dart';
import '../widgets/states.dart';

enum _State { idle, loading, done, error }

/// 搜索：按相关度排好；缺货的也返回（排在最后），标出来、压暗，不藏。
/// 效果回传：点进去报 click，结果里原地加购报 add_cart；trace_id 缺席时这批都不报。
class SearchPage extends StatefulWidget {
  const SearchPage({super.key});
  @override
  State<SearchPage> createState() => _SearchPageState();
}

class _SearchPageState extends State<SearchPage> {
  final _input = TextEditingController();
  _State _state = _State.idle;
  String _searched = '';
  List<ProductRow> _rows = const [];
  String _traceId = '';
  String _error = '';
  // 连着搜两次时只认最后一次发出去的请求。
  int _seq = 0;

  @override
  void dispose() {
    _input.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    final q = _input.text.trim();
    if (q.isEmpty) return;
    final mine = ++_seq;
    final s = Services.of(context);
    setState(() {
      _state = _State.loading;
      _searched = q.length > 200 ? q.substring(0, 200) : q;
    });
    try {
      final store = await s.store.ensure();
      final res = await searchProducts(s.client, q, store.storeId);
      if (!mounted || mine != _seq) return;
      setState(() {
        _rows = res.rows;
        _traceId = res.traceId;
        _state = _State.done;
      });
    } on ApiFailure catch (f) {
      if (!mounted || mine != _seq) return;
      setState(() {
        _error = f.message;
        _state = _State.error;
      });
    }
  }

  void _open(int id) {
    // 回传发了就不管，不等它、不因为它拦跳转。
    Services.of(context).trace.clicked(_traceId, id);
    context.push('/product/$id');
  }

  void _quickAdd(int id) {
    final trace = Services.of(context).trace;
    final traceId = _traceId;
    quickAdd(context, id, onAdded: () => trace.addedFromList(traceId, id));
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        backgroundColor: KeelColors.bg,
        surfaceTintColor: KeelColors.bg,
        titleSpacing: 0,
        title: Container(
          height: 38,
          decoration: BoxDecoration(color: KeelColors.searchBox, borderRadius: BorderRadius.circular(19)),
          child: TextField(
            key: const Key('search.input'),
            controller: _input,
            autofocus: true,
            maxLength: 200,
            textInputAction: TextInputAction.search,
            onSubmitted: (_) => _submit(),
            style: KeelText.body,
            decoration: const InputDecoration(
              counterText: '',
              border: InputBorder.none,
              prefixIcon: Icon(Icons.search, size: 18, color: KeelColors.textHint),
              hintText: '搜索商品，例如「连衣裙」「咖啡」',
              hintStyle: KeelText.hint,
              contentPadding: EdgeInsets.symmetric(vertical: 9),
            ),
          ),
        ),
        actions: [TextButton(key: const Key('search.submit'), onPressed: _submit, child: const Text('搜索'))],
      ),
      body: switch (_state) {
        _State.idle => const EmptyState(text: '输入关键词，按相关度为你排好'),
        _State.loading => const EmptyState(text: '正在搜索…'),
        _State.error => ErrorCard(message: _error, onRetry: _submit),
        _State.done when _rows.isEmpty => EmptyState(key: const Key('search.empty'), text: '没有找到「$_searched」相关的商品'),
        _State.done => ListView(padding: const EdgeInsets.fromLTRB(16, 8, 16, 24), children: [
            Padding(
              padding: const EdgeInsets.only(bottom: 10),
              child: Text('「$_searched」共 ${_rows.length} 件，按相关度排序', key: const Key('search.count'), style: KeelText.hint),
            ),
            for (final r in _rows) ProductTile(row: r, onTap: () => _open(r.id), onAdd: () => _quickAdd(r.id)),
          ]),
      },
    );
  }
}
