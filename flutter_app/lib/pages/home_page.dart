import 'package:flutter/material.dart';

import '../api/client.dart';
import '../api/services.dart';
import '../api/view.dart';
import '../theme.dart';
import '../widgets/product_card.dart';
import '../widgets/states.dart';

class HomePage extends StatefulWidget {
  const HomePage({super.key});
  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  List<ProductRow> _rows = const [];
  String _storeLine = '';
  String _error = '';
  bool _loading = true;
  bool _outOfRange = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (_loading && _rows.isEmpty && _error.isEmpty) _load();
  }

  Future<void> _load() async {
    final s = Services.of(context);
    setState(() {
      _loading = true;
      _error = '';
    });
    try {
      // 先定门店再拉列表：价格与在售范围按门店算。
      final store = await s.store.ensure();
      if (store.storeId == null) {
        setState(() {
          _outOfRange = true;
          _loading = false;
        });
        return;
      }
      final rows = await fetchProducts(s.client, storeId: store.storeId);
      if (!mounted) return;
      setState(() {
        _storeLine = '由「${store.name}」为你配送';
        _rows = rows;
        _loading = false;
      });
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _error = f.message;
        _loading = false;
      });
    }
  }

  String get _greeting {
    final h = DateTime.now().hour;
    return h < 11 ? '早上好' : (h < 18 ? '下午好' : '晚上好');
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: CustomScrollView(slivers: [
            SliverToBoxAdapter(
              child: Padding(
                padding: const EdgeInsets.fromLTRB(20, 20, 20, 8),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('KEEL · 精选好物', style: KeelText.overline),
                  const SizedBox(height: 8),
                  Text(_greeting, style: KeelText.display),
                  const Text('慢一点，好好喝一杯', style: KeelText.sub),
                  if (_storeLine.isNotEmpty)
                    Padding(
                      padding: const EdgeInsets.only(top: 6),
                      child: Text(_storeLine, key: const Key('home.store'), style: KeelText.hint),
                    ),
                ]),
              ),
            ),
            if (_error.isNotEmpty) SliverToBoxAdapter(child: ErrorCard(message: _error, onRetry: _load)),
            if (_outOfRange) const SliverToBoxAdapter(child: EmptyState(text: '当前位置暂不在配送范围内')),
            if (_loading && _rows.isEmpty) const SliverToBoxAdapter(child: EmptyState(text: '正在加载…')),
            SliverPadding(
              padding: const EdgeInsets.symmetric(horizontal: 10),
              sliver: SliverGrid(
                key: const Key('home.grid'),
                gridDelegate: const SliverGridDelegateWithFixedCrossAxisCount(
                    crossAxisCount: 2, mainAxisSpacing: 12, crossAxisSpacing: 12, childAspectRatio: 0.62),
                delegate: SliverChildBuilderDelegate(
                  (_, i) => ProductCard(row: _rows[i], onTap: () {}),
                  childCount: _rows.length,
                ),
              ),
            ),
            const SliverToBoxAdapter(child: SizedBox(height: 28)),
          ]),
        ),
      ),
    );
  }
}
