import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/catalog.dart';
import '../api/client.dart';
import '../api/services.dart';
import '../theme.dart';

void toast(BuildContext context, String text) {
  ScaffoldMessenger.of(context)
    ..hideCurrentSnackBar()
    ..showSnackBar(
      SnackBar(content: Text(text), duration: const Duration(seconds: 2)),
    );
}

/// 列表页（首页 / 搜索结果）的「原地加购」：只有一个规格时直接加一件、弹一句轻提示；
/// 多个规格时从底部弹出规格浮层，在浮层里选完、加完，全程不离开列表页。
/// onAdded 在真的加进去之后调（搜索页拿它回传 add_cart）。
Future<void> quickAdd(
  BuildContext context,
  int productId, {
  VoidCallback? onAdded,
}) async {
  final s = Services.of(context);
  if (!s.session.loggedIn) {
    context.push('/login');
    return;
  }
  try {
    final store = await s.store.ensure();
    final d = await fetchProduct(s.client, productId, store.storeId);
    if (!context.mounted) return;
    if (d.offShelf) return toast(context, '这件商品已下架');
    final first = d.firstBuyable;
    if (first == null) return toast(context, '暂时无货');
    // 只有一个规格：不弹浮层，直接加 —— 多一步选择就是多一次点击。
    if (d.skus.length == 1) {
      final n = await addToCart(
        s.client,
        skuId: first.id,
        quantity: 1,
        storeId: store.storeId,
      );
      s.cart.set(n);
      onAdded?.call();
      if (context.mounted) toast(context, '已加入购物车');
      return;
    }
    // 上一次加购的轻提示还在的话会盖住浮层底部的「加入购物车」：先收掉。
    ScaffoldMessenger.of(context).hideCurrentSnackBar();
    final added = await showModalBottomSheet<bool>(
      context: context,
      isScrollControlled: true,
      backgroundColor: KeelColors.card,
      shape: const RoundedRectangleBorder(
        borderRadius: BorderRadius.vertical(top: Radius.circular(20)),
      ),
      builder: (_) => Services(
        client: s.client,
        session: s.session,
        store: s.store,
        cart: s.cart,
        trace: s.trace,
        unread: s.unread,
        child: QuickCartSheet(detail: d, storeId: store.storeId),
      ),
    );
    if (added == true) {
      onAdded?.call();
      if (context.mounted) toast(context, '已加入购物车');
    }
  } on ApiFailure catch (f) {
    if (context.mounted) toast(context, f.message);
  }
}

class QuickCartSheet extends StatefulWidget {
  const QuickCartSheet({
    super.key,
    required this.detail,
    required this.storeId,
  });
  final ProductDetailView detail;
  final int? storeId;
  @override
  State<QuickCartSheet> createState() => _QuickCartSheetState();
}

class _QuickCartSheetState extends State<QuickCartSheet> {
  late int _skuId = widget.detail.firstBuyable!.id;
  int _qty = 1;
  bool _adding = false;
  String _hint = '';

  SkuRow get _picked => widget.detail.sku(_skuId)!;
  bool get _canPlus => _qty < _picked.availableQty && _qty < 999;

  void _pick(SkuRow r) {
    if (r.soldOut) return;
    setState(() {
      _skuId = r.id;
      _hint = '';
      if (_qty > r.availableQty) _qty = r.availableQty < 1 ? 1 : r.availableQty;
    });
  }

  Future<void> _confirm() async {
    if (_adding || _picked.soldOut) return;
    final s = Services.of(context);
    setState(() {
      _adding = true;
      _hint = '';
    });
    try {
      final n = await addToCart(
        s.client,
        skuId: _skuId,
        quantity: _qty,
        storeId: widget.storeId,
      );
      s.cart.set(n);
      if (mounted) Navigator.of(context).pop(true);
    } on ApiFailure catch (f) {
      // 浮层开着：错误留在浮层里，让人换个规格或改数量再试。
      if (mounted) {
        setState(() {
          _adding = false;
          _hint = f.message;
        });
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final d = widget.detail;
    final p = _picked;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(20, 18, 20, 16),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Text(
                        d.title,
                        style: KeelText.body.copyWith(
                          fontWeight: FontWeight.w700,
                        ),
                      ),
                      const SizedBox(height: 4),
                      Row(
                        children: [
                          Text(
                            p.priceText,
                            key: const Key('quick.price'),
                            style: KeelText.price,
                          ),
                          if (p.listPriceText.isNotEmpty) ...[
                            const SizedBox(width: 6),
                            Text(
                              p.listPriceText,
                              style: KeelText.hint.copyWith(
                                decoration: TextDecoration.lineThrough,
                              ),
                            ),
                          ],
                        ],
                      ),
                    ],
                  ),
                ),
                IconButton(
                  key: const Key('quick.close'),
                  onPressed: _adding
                      ? null
                      : () => Navigator.of(context).pop(false),
                  icon: const Icon(Icons.close, size: 20),
                ),
              ],
            ),
            const SizedBox(height: 12),
            Text(d.skus.first.specName, style: KeelText.sub),
            const SizedBox(height: 8),
            SkuChips(
              skus: d.skus,
              selected: _skuId,
              onPick: _pick,
              keyPrefix: 'quick.sku',
            ),
            const SizedBox(height: 12),
            Row(
              children: [
                const Text('数量', style: KeelText.sub),
                const Spacer(),
                QtyStepper(
                  value: _qty,
                  canMinus: _qty > 1,
                  canPlus: _canPlus,
                  keyPrefix: 'quick',
                  onMinus: () => setState(() => _qty--),
                  onPlus: () => setState(() => _qty++),
                ),
              ],
            ),
            if (_hint.isNotEmpty)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Text(
                  _hint,
                  key: const Key('quick.hint'),
                  style: KeelText.err,
                ),
              ),
            const SizedBox(height: 16),
            SizedBox(
              width: double.infinity,
              child: FilledButton(
                key: const Key('quick.confirm'),
                onPressed: _adding || p.soldOut ? null : _confirm,
                child: Text(_adding ? '加入中…' : '加入购物车'),
              ),
            ),
          ],
        ),
      ),
    );
  }
}

/// 规格选择：选中的深色、无货的压暗并标「无货」（仍然列着）。
class SkuChips extends StatelessWidget {
  const SkuChips({
    super.key,
    required this.skus,
    required this.selected,
    required this.onPick,
    required this.keyPrefix,
  });
  final List<SkuRow> skus;
  final int selected;
  final void Function(SkuRow) onPick;
  final String keyPrefix;

  @override
  Widget build(BuildContext context) => Wrap(
    spacing: 10,
    runSpacing: 10,
    children: [
      for (final s in skus)
        GestureDetector(
          key: Key('$keyPrefix.${s.id}'),
          onTap: () => onPick(s),
          child: Container(
            padding: const EdgeInsets.symmetric(horizontal: 18, vertical: 8),
            decoration: BoxDecoration(
              color: s.id == selected
                  ? KeelColors.primary
                  : (s.soldOut ? const Color(0xFFF3EEE8) : KeelColors.card),
              border: Border.all(
                color: s.id == selected
                    ? KeelColors.primary
                    : (s.soldOut
                          ? const Color(0xFFD6CCC0)
                          : KeelColors.chipBorder),
              ),
              borderRadius: BorderRadius.circular(18),
            ),
            // 无货：灰底、划线字，再挂一个「无货」小标 —— 一眼看得出来，而不只是淡一点。
            child: Row(
              mainAxisSize: MainAxisSize.min,
              children: [
                Text(
                  s.label,
                  style: TextStyle(
                    fontSize: 14,
                    color: s.id == selected
                        ? KeelColors.card
                        : (s.soldOut
                              ? const Color(0xFFA89B8C)
                              : KeelColors.text),
                    decoration: s.soldOut ? TextDecoration.lineThrough : null,
                  ),
                ),
                if (s.soldOut)
                  Container(
                    margin: const EdgeInsets.only(left: 6),
                    padding: const EdgeInsets.symmetric(
                      horizontal: 5,
                      vertical: 1,
                    ),
                    decoration: BoxDecoration(
                      color: const Color(0xFFB5A898),
                      borderRadius: BorderRadius.circular(4),
                    ),
                    child: const Text(
                      '无货',
                      style: TextStyle(fontSize: 11, color: KeelColors.card),
                    ),
                  ),
              ],
            ),
          ),
        ),
    ],
  );
}

class QtyStepper extends StatelessWidget {
  const QtyStepper({
    super.key,
    required this.value,
    required this.canMinus,
    required this.canPlus,
    required this.onMinus,
    required this.onPlus,
    required this.keyPrefix,
  });
  final int value;
  final bool canMinus;
  final bool canPlus;
  final VoidCallback onMinus;
  final VoidCallback onPlus;
  final String keyPrefix;

  // 灰色的胶囊（uni-app x 的 .stepper）：− 数字 ＋，点不了的一侧变淡。
  @override
  Widget build(BuildContext context) => Container(
    decoration: BoxDecoration(
      color: const Color(0xFFF3ECE3),
      borderRadius: BorderRadius.circular(16),
    ),
    child: Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        _btn('$keyPrefix.minus', '−', canMinus ? onMinus : null),
        ConstrainedBox(
          constraints: const BoxConstraints(minWidth: 24),
          child: Text(
            '$value',
            key: Key('$keyPrefix.qty'),
            textAlign: TextAlign.center,
            style: const TextStyle(
              fontSize: 14,
              fontWeight: FontWeight.w700,
              color: KeelColors.text,
            ),
          ),
        ),
        _btn('$keyPrefix.plus', '+', canPlus ? onPlus : null),
      ],
    ),
  );

  Widget _btn(String key, String t, VoidCallback? on) => InkWell(
    key: Key(key),
    onTap: on,
    borderRadius: BorderRadius.circular(15),
    child: SizedBox(
      width: 30,
      height: 30,
      child: Center(
        child: Text(
          t,
          style: TextStyle(
            fontSize: 18,
            color: KeelColors.primary.withValues(alpha: on == null ? 0.3 : 1),
          ),
        ),
      ),
    ),
  );
}
