import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/cart.dart';
import '../api/client.dart';
import '../api/services.dart';
import '../tabs.dart';
import '../theme.dart';
import '../widgets/quick_cart.dart';
import '../widgets/states.dart';

/// 购物车：勾选 / 全选、改数量、不能买的行写原因且不能勾、合计用服务端的数、预估运费、凑单提示、删除、去结算。
class CartPage extends StatefulWidget {
  const CartPage({super.key});
  @override
  State<CartPage> createState() => _CartPageState();
}

class _CartPageState extends State<CartPage> {
  CartView? _v;
  int? _storeId;
  String _storeLine = '';
  String _error = '';
  String _message = '';
  bool _loaded = false;
  bool _editing = false;
  // 管理模式下「删除」勾的行：和结算的勾选分开 —— 不能买的行也要能删。
  final _removeIds = <int>{};
  // 有请求在飞时不接下一次点击：每个请求都返回整辆车，两个交错回来会互相覆盖。
  bool _busy = false;
  bool _started = false;
  late final _session = Services.of(context).session;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      Tabs.current.addListener(_onTab);
      _session.addListener(_onSession);
      _load();
    }
  }

  @override
  void dispose() {
    Tabs.current.removeListener(_onTab);
    _session.removeListener(_onSession);
    super.dispose();
  }

  void _onTab() {
    if (Tabs.current.value == Tabs.cart && mounted) _load();
  }

  void _onSession() {
    if (!mounted) return;
    if (!Services.of(context).session.loggedIn) {
      setState(() {
        _v = null;
        _loaded = false;
      });
    } else {
      _load();
    }
  }

  Future<void> _load() async {
    final s = Services.of(context);
    if (!s.session.loggedIn) return setState(() {});
    setState(() {
      _error = '';
      _message = '';
    });
    try {
      final store = await s.store.ensure();
      _storeId = store.storeId;
      _storeLine = store.storeId != null
          ? '按「${store.name}」的价格与库存'
          : '当前位置暂不在配送范围内';
      _apply(await fetchCart(s.client, storeId: _storeId));
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _error = f.message;
          _loaded = true;
        });
      }
    }
  }

  void _apply(CartView v) {
    if (!mounted) return;
    Services.of(context).cart.set(v.quantity);
    setState(() {
      _v = v;
      _loaded = true;
      _busy = false;
      _removeIds.removeWhere((id) => !v.rows.any((r) => r.id == id));
    });
  }

  Future<void> _run(Future<CartView> Function() op) async {
    if (_busy) return;
    setState(() {
      _busy = true;
      _message = '';
    });
    try {
      _apply(await op());
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _busy = false;
        _message = f.message;
      });
      // 失败时服务端那辆车没变，但页面可能已经过时（别处改过）：重读一次对齐，数量回到服务端的值。
      final keep = _message;
      await _load();
      if (mounted) setState(() => _message = keep);
    }
  }

  /// 从购物车压出去的页（商品、结算）回来时重读：那边可能加购了、下单删掉了几行。
  /// tab 下标没变，Tabs.current 不会通知。
  Future<void> _pushThenReload(String route) async {
    await context.push(route);
    if (mounted) _load();
  }

  void _toggle(CartRow r) {
    final c = Services.of(context).client;
    if (_editing && !r.available) {
      setState(
        () => _removeIds.contains(r.id)
            ? _removeIds.remove(r.id)
            : _removeIds.add(r.id),
      );
      return;
    }
    if (!r.available) return;
    _run(() => setCartChecked(c, r.id, !r.selected, storeId: _storeId));
  }

  void _qty(CartRow r, int next) {
    if (next < 1 || next > 999) return;
    _run(
      () => setCartQuantity(
        Services.of(context).client,
        r.id,
        next,
        storeId: _storeId,
      ),
    );
  }

  void _toggleAll() {
    final v = _v;
    if (v == null || v.availableCount == 0) return;
    _run(
      () => selectAllCart(
        Services.of(context).client,
        !v.allSelected,
        storeId: _storeId,
      ),
    );
  }

  List<int> get _deletable => [
    for (final r in _v?.rows ?? const <CartRow>[])
      if (r.selected || _removeIds.contains(r.id)) r.id,
  ];

  Future<void> _remove() async {
    final ids = _deletable;
    if (ids.isEmpty || _busy) return;
    final ok = await showDialog<bool>(
      context: context,
      builder: (d) => AlertDialog(
        title: Text('删除 ${ids.length} 件商品？'),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(d, false),
            child: const Text('取消'),
          ),
          TextButton(
            key: const Key('cart.deleteConfirm'),
            onPressed: () => Navigator.pop(d, true),
            child: const Text('删除'),
          ),
        ],
      ),
    );
    if (ok != true || !mounted) return;
    final c = Services.of(context).client;
    await _run(() => deleteCartItems(c, ids, storeId: _storeId));
    if (mounted) setState(() => _editing = false);
  }

  @override
  Widget build(BuildContext context) {
    final s = Services.of(context);
    final v = _v;
    return Scaffold(
      appBar: AppBar(
        title: const Text('购物车'),
        backgroundColor: KeelColors.bg,
        surfaceTintColor: KeelColors.bg,
      ),
      body: !s.session.loggedIn
          ? Center(
              child: Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  const Text('登录后查看购物车', style: KeelText.sub),
                  const SizedBox(height: 16),
                  FilledButton(
                    key: const Key('cart.login'),
                    onPressed: () => context.push('/login'),
                    child: const Text('登录'),
                  ),
                ],
              ),
            )
          : RefreshIndicator(
              onRefresh: _load,
              child: ListView(
                padding: const EdgeInsets.fromLTRB(16, 4, 16, 24),
                children: [
                  if (_error.isNotEmpty)
                    ErrorCard(message: _error, onRetry: _load),
                  if (_message.isNotEmpty)
                    Container(
                      margin: const EdgeInsets.only(bottom: 10),
                      padding: const EdgeInsets.all(12),
                      decoration: BoxDecoration(
                        color: const Color(0xFFF8EAE5),
                        borderRadius: BorderRadius.circular(12),
                      ),
                      child: Text(
                        _message,
                        key: const Key('cart.message'),
                        style: KeelText.err,
                      ),
                    ),
                  if (v != null && v.rows.isNotEmpty)
                    Padding(
                      padding: const EdgeInsets.fromLTRB(4, 12, 4, 0),
                      child: Row(
                        children: [
                          Expanded(
                            child: Text(_storeLine, style: KeelText.hint),
                          ),
                          GestureDetector(
                            key: const Key('cart.manage'),
                            onTap: () => setState(() => _editing = !_editing),
                            child: Text(
                              _editing ? '完成' : '管理',
                              style: KeelText.link,
                            ),
                          ),
                        ],
                      ),
                    ),
                  // 不能买的行照样列出来、写明原因，但不能勾选、不进合计、不去结算。服务端不替用户删，客户端也不。
                  for (final r in v?.rows ?? const <CartRow>[]) _row(r),
                  if (v != null && v.promotionNotes.isNotEmpty)
                    Container(
                      margin: const EdgeInsets.only(top: 12),
                      padding: const EdgeInsets.all(18),
                      decoration: BoxDecoration(
                        color: KeelColors.card,
                        borderRadius: BorderRadius.circular(16),
                      ),
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          for (final n in v.promotionNotes)
                            Text(n, style: KeelText.promo),
                        ],
                      ),
                    ),
                  if (_loaded &&
                      _error.isEmpty &&
                      (v == null || v.rows.isEmpty))
                    Padding(
                      padding: const EdgeInsets.only(top: 80),
                      child: Column(
                        children: [
                          const Text(
                            '购物车是空的',
                            key: Key('cart.empty'),
                            style: KeelText.sub,
                          ),
                          const SizedBox(height: 12),
                          OutlinedButton(
                            onPressed: () => context.go('/'),
                            child: const Text('去逛逛'),
                          ),
                        ],
                      ),
                    ),
                ],
              ),
            ),
      bottomNavigationBar: v == null || v.rows.isEmpty || !s.session.loggedIn
          ? null
          : _bar(v),
    );
  }

  Widget _check(bool on, bool enabled) => Container(
    width: 22,
    height: 22,
    decoration: BoxDecoration(
      shape: BoxShape.circle,
      color: on
          ? KeelColors.primary
          : (enabled ? null : const Color(0xFFEEE9E3)),
      border: Border.all(
        color: on
            ? KeelColors.primary
            : (enabled ? const Color(0xFFCFC3B5) : KeelColors.chipBorder),
      ),
    ),
    child: on
        ? const Icon(Icons.check, size: 14, color: KeelColors.card)
        : null,
  );

  Widget _glyph(CartRow r) => ColoredBox(
    color: r.cover.color,
    child: Center(
      child: Text(
        r.cover.glyph,
        style: const TextStyle(
          fontSize: 30,
          color: KeelColors.card,
          fontWeight: FontWeight.w700,
        ),
      ),
    ),
  );

  Widget _row(CartRow r) {
    final marked = r.selected || (_editing && _removeIds.contains(r.id));
    return Opacity(
      key: Key('cart.row.${r.id}'),
      opacity: r.available ? 1 : 0.7,
      child: Container(
        margin: const EdgeInsets.only(top: 12),
        padding: const EdgeInsets.all(18),
        decoration: BoxDecoration(
          color: KeelColors.card,
          borderRadius: BorderRadius.circular(16),
        ),
        child: Row(
          children: [
            GestureDetector(
              key: Key('cart.check.${r.id}'),
              onTap: () => _toggle(r),
              behavior: HitTestBehavior.opaque,
              child: Padding(
                padding: const EdgeInsets.only(right: 12),
                child: _check(marked, r.available || _editing),
              ),
            ),
            GestureDetector(
              key: Key('cart.cover.${r.id}'),
              onTap: r.productId > 0
                  ? () => _pushThenReload('/product/${r.productId}')
                  : null,
              child: ClipRRect(
                borderRadius: BorderRadius.circular(12),
                child: SizedBox(
                  width: 76,
                  height: 76,
                  child: r.cover.imageUrl.isNotEmpty
                      ? Image.network(
                          r.cover.imageUrl,
                          fit: BoxFit.cover,
                          errorBuilder: (_, _, _) => _glyph(r),
                        )
                      : _glyph(r),
                ),
              ),
            ),
            const SizedBox(width: 12),
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    r.title,
                    maxLines: 2,
                    overflow: TextOverflow.ellipsis,
                    style: KeelText.section,
                  ),
                  if (r.specText.isNotEmpty)
                    Text(r.specText, style: KeelText.hint),
                  if (r.statusText.isNotEmpty)
                    Text(
                      r.statusText,
                      key: Key('cart.status.${r.id}'),
                      style: KeelText.err,
                    ),
                  if (r.undeliverableText.isNotEmpty)
                    Text(
                      r.undeliverableText,
                      key: Key('cart.undeliverable.${r.id}'),
                      style: KeelText.err,
                    ),
                  const SizedBox(height: 6),
                  Row(
                    children: [
                      Text(r.priceText, style: KeelText.price),
                      if (r.listPriceText.isNotEmpty) ...[
                        const SizedBox(width: 6),
                        Text(
                          r.listPriceText,
                          style: KeelText.hint.copyWith(
                            decoration: TextDecoration.lineThrough,
                          ),
                        ),
                      ],
                      const Spacer(),
                      QtyStepper(
                        value: r.quantity,
                        canMinus: r.quantity > 1 && !_busy,
                        canPlus: !_busy,
                        keyPrefix: 'cart.${r.id}',
                        onMinus: () => _qty(r, r.quantity - 1),
                        onPlus: () => _qty(r, r.quantity + 1),
                      ),
                    ],
                  ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _bar(CartView v) => Container(
    decoration: const BoxDecoration(
      color: KeelColors.card,
      border: Border(top: BorderSide(color: KeelColors.line)),
    ),
    child: SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
        child: Row(
          children: [
            GestureDetector(
              key: const Key('cart.all'),
              onTap: _toggleAll,
              child: Row(
                children: [
                  _check(v.allSelected, v.availableCount > 0),
                  const SizedBox(width: 6),
                  const Text('全选', style: KeelText.sub),
                ],
              ),
            ),
            const SizedBox(width: 12),
            Expanded(
              child: _editing
                  ? const SizedBox()
                  : Column(
                      crossAxisAlignment: CrossAxisAlignment.end,
                      mainAxisSize: MainAxisSize.min,
                      children: [
                        Row(
                          mainAxisAlignment: MainAxisAlignment.end,
                          children: [
                            const Text('合计 ', style: KeelText.hint),
                            // 金额很长（或字体很宽）时缩小而不是溢出。
                            Flexible(
                              child: FittedBox(
                                fit: BoxFit.scaleDown,
                                child: Text(v.selectedText, key: const Key('cart.total'), style: KeelText.priceL),
                              ),
                            ),
                          ],
                        ),
                        if (v.promotionDiscountText.isNotEmpty)
                          Text(
                            '活动已减 ${v.promotionDiscountText.substring(1)}',
                            key: const Key('cart.discount'),
                            style: KeelText.promo,
                          ),
                        // 预估运费按默认地址算；没有地址时服务端不给 freight：不显示，不能写成「包邮」。
                        if (v.freightText.isNotEmpty)
                          Text(
                            '运费 ${v.freightText}${v.freightNote.isEmpty ? '' : '（${v.freightNote}）'}',
                            key: const Key('cart.freight'),
                            style: KeelText.hint,
                          ),
                      ],
                    ),
            ),
            const SizedBox(width: 12),
            _editing
                ? FilledButton(
                    key: const Key('cart.delete'),
                    style: FilledButton.styleFrom(
                      backgroundColor: KeelColors.err,
                    ),
                    onPressed: _deletable.isEmpty ? null : _remove,
                    child: Text('删除(${_deletable.length})'),
                  )
                : FilledButton(
                    key: const Key('cart.checkout'),
                    style: FilledButton.styleFrom(
                      minimumSize: const Size(132, 48),
                    ),
                    onPressed: v.selectedCount == 0
                        ? null
                        : () => _pushThenReload('/checkout?from=cart'),
                    child: Text('去结算(${v.selectedCount})'),
                  ),
          ],
        ),
      ),
    ),
  );
}
