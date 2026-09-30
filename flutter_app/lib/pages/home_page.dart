import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/catalog.dart';

import '../api/client.dart';
import '../api/services.dart';
import 'place_picker_page.dart';
import '../api/store.dart';
import '../api/view.dart';
import '../theme.dart';
import '../widgets/product_card.dart';
import '../widgets/quick_cart.dart';
import '../widgets/states.dart';

/// 首页顶上的问候，按本地时间的小时分段（原来 11–18 点都算「下午好」，上午 11 点也显示下午好）。
String greetingFor(int hour) => switch (hour) {
      < 5 => '夜深了',
      < 9 => '早上好',
      < 12 => '上午好',
      < 14 => '中午好',
      < 18 => '下午好',
      _ => '晚上好',
    };

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
  int _total = 0;
  List<CategoryChip> _categories = const [];
  /// 0 = 全部。
  int _categoryId = 0;
  int? _storeId;

  StoreService? _store;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (_store == null) {
      _store = Services.of(context).store..addListener(_onStore);
      _load();
    }
  }

  @override
  void dispose() {
    _store?.removeListener(_onStore);
    super.dispose();
  }

  /// 门店作废了（换了服务地址 / 换了送货地址 = 可能换了一家店）：重新解析、重拉 —— 不然首页还摆着上一家店的商品。
  /// 地址名（逆地理编码）晚一步到，到了刷一下顶上那行。
  void _onStore() {
    if (!mounted) return;
    if (_store!.current != null) {
      setState(() {});
      return;
    }
    setState(() {
      _rows = const [];
      _categories = const [];
      _categoryId = 0;
      _storeLine = '';
    });
    _load();
  }

  Future<void> _load() async {
    final s = Services.of(context);
    setState(() {
      _loading = true;
      _error = '';
    });
    _loadCategories();
    try {
      // 先定门店再拉列表：价格与在售范围按门店算。
      final store = await s.store.ensure();
      if (!mounted) return;
      if (store.storeId == null) {
        // match_type = none：这家商家不送这里。是成功的结果，不是错误。
        setState(() {
          _outOfRange = true;
          _rows = const [];
          _total = 0;
          _loading = false;
        });
        return;
      }
      setState(() {
        _outOfRange = false;
        _storeId = store.storeId;
        _storeLine = '由「${store.name}」为你配送';
      });
      await _fetch();
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _error = f.message;
        _loading = false;
      });
    }
  }

  Future<void> _loadCategories() async {
    final chips = await fetchCategories(Services.of(context).client);
    if (!mounted) return;
    setState(() {
      _categories = chips;
      // 选中的分类被停用 / 删掉了就回到「全部」，不然标题和列表对不上。
      if (!chips.any((c) => c.id == _categoryId)) _categoryId = 0;
    });
  }

  Future<void> _fetch() async {
    // 连着点两个分类时只认最后一次：返回时分类已经变了就丢掉结果。
    final asked = _categoryId;
    try {
      final page = await fetchProducts(Services.of(context).client, storeId: _storeId, categoryId: asked == 0 ? null : asked);
      if (!mounted || asked != _categoryId) return;
      setState(() {
        _rows = page.rows;
        _total = page.total;
        _loading = false;
      });
    } on ApiFailure catch (f) {
      if (!mounted || asked != _categoryId) return;
      setState(() {
        _error = f.message;
        _loading = false;
      });
    }
  }

  void _pickCategory(int id) {
    if (id == _categoryId || _storeId == null) return;
    setState(() {
      _categoryId = id;
      _loading = true;
      _error = '';
    });
    _fetch();
  }

  String get _sectionTitle {
    for (final c in _categories) {
      if (c.id == _categoryId) return c.name;
    }
    return '全部商品';
  }

  /// 「送至 …」：定位到的地址名 / 换过的地址；还不知道（没配地图服务商、没定位）就退回门店。
  String get _placeLine {
    final label = _store?.placeLabel ?? '';
    return label.isNotEmpty ? '送至 $label' : _storeLine;
  }

  /// 换送货地址：搜索地点 / 地图选点 / 收货地址 / 当前定位。选完按新坐标重新解析门店（StoreService 通知 → _onStore 重拉）。
  Future<void> _changePlace() async {
    final store = Services.of(context).store;
    final messenger = ScaffoldMessenger.of(context);
    final p = await context.push<PickedPlace>('/place');
    if (p == null) return;
    if (p.device) {
      store.useDeviceLocation();
      return;
    }
    store.deliverTo(at: p.at, label: p.label);
    if (p.at == null) messenger.showSnackBar(const SnackBar(content: Text('这条地址没有位置信息，暂按默认门店')));
  }

  String get _greeting => greetingFor(DateTime.now().hour);

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
                  const SizedBox(height: 4),
                  const Text('慢一点，好好喝一杯', style: KeelText.sub),
                  if (_placeLine.isNotEmpty)
                    GestureDetector(
                      key: const Key('home.store'),
                      behavior: HitTestBehavior.opaque,
                      onTap: _changePlace,
                      child: Padding(
                        padding: const EdgeInsets.only(top: 6),
                        child: Text('$_placeLine ›', maxLines: 1, overflow: TextOverflow.ellipsis, style: KeelText.hint),
                      ),
                    ),
                  const SizedBox(height: 20),
                  // 首页只放一个入口，真正的输入框在搜索页（首页一聚焦就弹键盘挡住商品）。
                  GestureDetector(
                    key: const Key('home.search'),
                    onTap: () => context.push('/search'),
                    child: Container(
                      height: 44,
                      padding: const EdgeInsets.symmetric(horizontal: 18),
                      decoration: BoxDecoration(color: KeelColors.searchBox, borderRadius: BorderRadius.circular(22)),
                      child: const Row(children: [
                        Icon(Icons.search, size: 18, color: KeelColors.textHint),
                        SizedBox(width: 8),
                        Text('搜索商品', style: KeelText.hint),
                      ]),
                    ),
                  ),
                ]),
              ),
            ),
            // 分类拉不到就整排不显示，首页照常。
            if (_categories.isNotEmpty)
              SliverToBoxAdapter(
                child: SizedBox(
                  height: 50,
                  child: ListView(
                    scrollDirection: Axis.horizontal,
                    padding: const EdgeInsets.fromLTRB(20, 16, 20, 0),
                    children: [
                      _chip('home.cat.all', '全部', 0),
                      for (final c in _categories) _chip('home.cat.${c.id}', c.name, c.id),
                    ],
                  ),
                ),
              ),
            SliverToBoxAdapter(
              child: Padding(
                padding: const EdgeInsets.fromLTRB(20, 24, 20, 4),
                child: Row(children: [
                  Text(_sectionTitle, key: const Key('home.section'), style: KeelText.section),
                  const Spacer(),
                  if (_total > 0) Text('共 $_total 件', key: const Key('home.total'), style: KeelText.hint),
                ]),
              ),
            ),
            if (_error.isNotEmpty) SliverToBoxAdapter(child: ErrorCard(message: _error, onRetry: _load)),
            if (_outOfRange) const SliverToBoxAdapter(child: EmptyState(text: '当前位置暂不在配送范围内')),
            if (!_loading && !_outOfRange && _error.isEmpty && _rows.isEmpty)
              const SliverToBoxAdapter(child: EmptyState(text: '店里还没有上架商品')),
            if (_loading && _rows.isEmpty) const SliverToBoxAdapter(child: EmptyState(text: '正在加载…')),
            // 两列网格：固定行高（按最高的卡片：两行标题 + 副标题 + 标签），同一行两张卡片等高、价格行贴底。
            // 不用 IntrinsicHeight：它每行要排两遍版，滑动时每进来一行都付一次，小程序里（canvas + wasm）明显卡。
            // 边距 16、卡片间距 12（与 uni-app x 一样）。
            SliverPadding(
              key: const Key('home.grid'),
              padding: const EdgeInsets.fromLTRB(16, 6, 16, 0),
              sliver: SliverGrid(
                gridDelegate: const SliverGridDelegateWithFixedCrossAxisCount(
                    crossAxisCount: 2, mainAxisSpacing: 12, crossAxisSpacing: 12, mainAxisExtent: ProductCard.gridExtent),
                delegate: SliverChildBuilderDelegate(
                  (_, i) => ProductCard(
                    row: _rows[i],
                    onTap: () => context.push('/product/${_rows[i].id}'),
                    onAdd: () => quickAdd(context, _rows[i].id),
                  ),
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

extension on _HomePageState {
  Widget _chip(String key, String name, int id) {
    final on = id == _categoryId;
    return Padding(
      padding: const EdgeInsets.only(right: 8),
      child: GestureDetector(
        key: Key(key),
        onTap: () => _pickCategory(id),
        child: Container(
          padding: const EdgeInsets.symmetric(horizontal: 16),
          alignment: Alignment.center,
          decoration: BoxDecoration(
            color: on ? KeelColors.primary : KeelColors.card,
            border: Border.all(color: on ? KeelColors.primary : KeelColors.chipBorder),
            borderRadius: BorderRadius.circular(17),
          ),
          child: Text(name, style: TextStyle(fontSize: 13, color: on ? KeelColors.card : KeelColors.text)),
        ),
      ),
    );
  }
}
