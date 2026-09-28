import 'dart:async';

import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/catalog.dart';
import '../api/client.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/net_image.dart';
import '../widgets/quick_cart.dart';
import '../widgets/states.dart';

/// 商品详情：图片、价格（限时特价显示特价并划线门店价）、规格、加入购物车、立即购买、底栏购物车角标。
class ProductPage extends StatefulWidget {
  const ProductPage({super.key, required this.productId});
  final int productId;
  @override
  State<ProductPage> createState() => _ProductPageState();
}

class _ProductPageState extends State<ProductPage> {
  ProductDetailView? _d;
  int _skuId = 0;
  int? _storeId;
  bool _loading = true;
  String _error = '';
  String _hint = '';
  bool _added = false;
  bool _adding = false;
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _load();
      Services.of(context).cart.refresh();
    }
  }

  Future<void> _load() async {
    final s = Services.of(context);
    if (widget.productId <= 0) {
      setState(() {
        _loading = false;
        _error = '没有指定商品';
      });
      return;
    }
    try {
      // 与列表、结算同一家店算价：从列表点进来门店已经解析过；从分享链接直接打开才真的去解析。
      final store = await s.store.ensure();
      final d = await fetchProduct(s.client, widget.productId, store.storeId);
      if (!mounted) return;
      await _afterTransition();
      if (!mounted) return;
      setState(() {
        _storeId = store.storeId;
        _d = d;
        _skuId = d.firstBuyable?.id ?? 0;
        _loading = false;
      });
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _loading = false;
        _error = f.message;
      });
    }
  }

  /// 滑入动画走完再换上内容：整页第一次排版在小程序 iOS 上要两三百毫秒（没有 JIT，一段中文十几毫秒），
  /// 落在转场中间就是一下明显的卡；等动画停了再排，动画是顺的，内容晚出现不到 0.3 秒。
  Future<void> _afterTransition() {
    final a = ModalRoute.of(context)?.animation;
    if (a == null || a.isCompleted) return Future.value();
    final done = Completer<void>();
    void listen(AnimationStatus st) {
      if (st == AnimationStatus.completed || st == AnimationStatus.dismissed) {
        a.removeStatusListener(listen);
        done.complete();
      }
    }

    a.addStatusListener(listen);
    return done.future;
  }

  SkuRow? get _picked => _d?.sku(_skuId);
  bool get _canCart =>
      _d != null && !_d!.offShelf && _skuId > 0 && _storeId != null;
  bool get _canBuy => _d != null && !_d!.offShelf && _skuId > 0;

  void _pick(SkuRow r) {
    setState(() {
      if (r.soldOut) {
        _hint = '这个规格暂时无货';
        return;
      }
      _hint = '';
      _added = false;
      _skuId = r.id;
    });
  }

  Future<void> _addToCart() async {
    final s = Services.of(context);
    if (!s.session.loggedIn) {
      context.push('/login');
      return;
    }
    if (!_canCart || _adding) return;
    setState(() {
      _adding = true;
      _hint = '';
      _added = false;
    });
    try {
      // 每次点击一个新的幂等键（addToCart 里）：再点一次就是想再加一件。手抖连点由 _adding 挡住。
      final n = await addToCart(
        s.client,
        skuId: _skuId,
        quantity: 1,
        storeId: _storeId,
      );
      s.cart.set(n);
      // 从搜索结果点进来的商品回传 add_cart（不是就什么都不做）。
      s.trace.converted('add_cart', widget.productId);
      if (!mounted) return;
      setState(() {
        _adding = false;
        _added = true;
      });
      toast(context, '已加入购物车');
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _adding = false;
        _hint = f.message;
      });
    }
  }

  void _buy() {
    final d = _d;
    if (d == null) return;
    if (d.offShelf) return setState(() => _hint = '这件商品已下架');
    if (_skuId <= 0) return setState(() => _hint = '请先选择规格');
    context.push('/checkout?sku_id=$_skuId&product_id=${widget.productId}');
  }

  @override
  Widget build(BuildContext context) {
    final d = _d;
    final p = _picked;
    return Scaffold(
      appBar: AppBar(
        backgroundColor: KeelColors.bg,
        surfaceTintColor: KeelColors.bg,
      ),
      body: d == null
          ? (_loading
                ? const EmptyState(text: '正在加载…')
                : ErrorCard(message: _error, onRetry: _load))
          : ListView(
              children: [
                // 白色的信息面板往上压住图片 24（圆角 24），与 uni-app x 的 .sheet 一样。
                Stack(
                  children: [
                    _Gallery(detail: d),
                    Container(
                      margin: const EdgeInsets.only(top: _Gallery.height - 24),
                      padding: const EdgeInsets.fromLTRB(20, 24, 20, 28),
                      decoration: const BoxDecoration(
                        color: KeelColors.card,
                        borderRadius: BorderRadius.vertical(
                          top: Radius.circular(24),
                        ),
                      ),
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Text(
                            d.offShelf ? '已下架' : 'KEEL 精选',
                            key: const Key('detail.overline'),
                            style: KeelText.overline,
                          ),
                          const SizedBox(height: 8),
                          Text(
                            d.title,
                            key: const Key('detail.title'),
                            style: const TextStyle(
                              fontSize: 22,
                              fontWeight: FontWeight.w700,
                              color: KeelColors.text,
                            ),
                          ),
                          const SizedBox(height: 4),
                          if (d.subtitle.isNotEmpty)
                            Text(d.subtitle, style: KeelText.sub),
                          if (d.promoTags.isNotEmpty)
                            Padding(
                              padding: const EdgeInsets.only(top: 8),
                              child: Wrap(
                                spacing: 6,
                                children: [
                                  for (final t in d.promoTags)
                                    PromoTag(text: t),
                                ],
                              ),
                            ),
                          const SizedBox(height: 12),
                          Row(
                            crossAxisAlignment: CrossAxisAlignment.end,
                            children: [
                              Text(
                                p?.priceText ?? d.priceText,
                                key: const Key('detail.price'),
                                style: KeelText.priceL,
                              ),
                              if (p != null && p.listPriceText.isNotEmpty) ...[
                                const SizedBox(width: 6),
                                Text(
                                  p.listPriceText,
                                  key: const Key('detail.listPrice'),
                                  style: KeelText.hint.copyWith(
                                    decoration: TextDecoration.lineThrough,
                                  ),
                                ),
                              ],
                              if (p != null)
                                Padding(
                                  padding: const EdgeInsets.only(left: 12),
                                  child: Text(
                                    '库存 ${p.availableQty}',
                                    style: KeelText.hint,
                                  ),
                                ),
                            ],
                          ),
                          const Divider(height: 29, color: KeelColors.line),
                          if (d.skus.isNotEmpty) ...[
                            Text(
                              d.skus.first.specName,
                              style: KeelText.section,
                            ),
                            const SizedBox(height: 12),
                            SkuChips(
                              skus: d.skus,
                              selected: _skuId,
                              onPick: _pick,
                              keyPrefix: 'detail.sku',
                            ),
                          ],
                          if (d.description.isNotEmpty) ...[
                            const Divider(height: 32, color: KeelColors.line),
                            const Text('商品介绍', style: KeelText.section),
                            const SizedBox(height: 8),
                            Text(
                              d.description,
                              style: KeelText.body.copyWith(height: 1.6),
                            ),
                          ],
                        ],
                      ),
                    ),
                  ],
                ),
              ],
            ),
      bottomNavigationBar: d == null ? null : _bar(p),
    );
  }

  Widget _bar(SkuRow? p) {
    final count = Services.of(context).cart;
    return SafeArea(
      child: Container(
        decoration: const BoxDecoration(
          color: KeelColors.card,
          border: Border(top: BorderSide(color: KeelColors.line)),
        ),
        padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Align(
              alignment: Alignment.centerLeft,
              child: _hint.isNotEmpty
                  ? Text(
                      _hint,
                      key: const Key('detail.hint'),
                      style: KeelText.err,
                    )
                  : _added
                  ? GestureDetector(
                      key: const Key('detail.added'),
                      onTap: () => context.go('/cart'),
                      child: const Text('已加入购物车，去结算 ›', style: KeelText.ok),
                    )
                  : Text(
                      '已选 ${p?.label ?? '—'}',
                      key: const Key('detail.selected'),
                      style: KeelText.hint,
                    ),
            ),
            const SizedBox(height: 8),
            Row(
              children: [
                GestureDetector(
                  key: const Key('detail.cart'),
                  onTap: () => context.go('/cart'),
                  child: ListenableBuilder(
                    listenable: count,
                    builder: (_, _) => Badge(
                      isLabelVisible: count.n > 0,
                      label: Text(
                        count.n > 99 ? '99+' : '${count.n}',
                        key: const Key('detail.cartBadge'),
                      ),
                      child: const Column(
                        mainAxisSize: MainAxisSize.min,
                        children: [
                          Icon(
                            Icons.shopping_cart_outlined,
                            color: KeelColors.primary,
                          ),
                          Text(
                            '购物车',
                            style: TextStyle(
                              fontSize: 10,
                              color: KeelColors.textSub,
                            ),
                          ),
                        ],
                      ),
                    ),
                  ),
                ),
                const SizedBox(width: 8),
                Expanded(
                  child: OutlinedButton(
                    key: const Key('detail.add'),
                    style: OutlinedButton.styleFrom(
                      minimumSize: const Size(0, 48),
                      shape: const StadiumBorder(),
                    ),
                    onPressed: _canCart && !_adding ? _addToCart : null,
                    child: Text(_adding ? '加入中…' : '加入购物车'),
                  ),
                ),
                const SizedBox(width: 10),
                Expanded(
                  child: FilledButton(
                    key: const Key('detail.buy'),
                    onPressed: _canBuy ? _buy : null,
                    child: const Text('立即购买'),
                  ),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }
}

class _Gallery extends StatelessWidget {
  const _Gallery({required this.detail});

  /// 与 uni-app x 的 .hero 同高（固定 340，宽屏也不会一张图占满一屏）。
  static const height = 340.0;
  final ProductDetailView detail;
  @override
  Widget build(BuildContext context) {
    final c = detail.cover;
    final glyph = ColoredBox(
      color: c.color,
      child: Center(
        child: Text(
          c.glyph,
          style: const TextStyle(
            fontSize: 72,
            fontWeight: FontWeight.w700,
            color: KeelColors.card,
          ),
        ),
      ),
    );
    return SizedBox(
      height: height,
      child: detail.images.isEmpty
          ? glyph
          : PageView(
              key: const Key('detail.gallery'),
              children: [
                for (final u in detail.images)
                  NetImage(url: u, fallback: glyph),
              ],
            ),
    );
  }
}

class PromoTag extends StatelessWidget {
  const PromoTag({super.key, required this.text});
  final String text;
  @override
  Widget build(BuildContext context) => Container(
    padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 1),
    decoration: BoxDecoration(
      border: Border.all(color: const Color(0xFFE8B4A6)),
      borderRadius: BorderRadius.circular(4),
    ),
    child: Text(
      text,
      style: const TextStyle(fontSize: 11, color: KeelColors.err),
    ),
  );
}
