import 'catalog.dart';
import 'client.dart';
import 'schema.g.dart';
import 'view.dart';

/// 购物车。照 uni-app x 的 view.uts（cartRow / cartView / freightNote / promotionNotes）与 pages/cart 移植。
/// 每个写接口都返回整辆车：页面拿到就整体替换，不在本地增减。

class CartRow {
  final int id;
  final int skuId;
  final int productId;
  final String title;
  final String specText;
  /// 失效 / 本店不售的行价格是 null：显示「—」。是活动价（限时特价后）。
  final String priceText;
  /// 门店价比活动价高时的门店价（划线）；否则空串。
  final String listPriceText;
  final int quantity;
  /// 页面上显示的勾选：只有能买的行才可能是勾选的。
  final bool selected;
  /// 能按这个数量买下（status == available）。只有这种行能勾选、能结算。
  final bool available;
  final String statusText;
  /// 送不到当前收货地址的原因；送得到或没有地址是空串。
  final String undeliverableText;
  final Cover cover;
  const CartRow({required this.id, required this.skuId, required this.productId, required this.title,
      required this.specText, required this.priceText, required this.listPriceText, required this.quantity,
      required this.selected, required this.available, required this.statusText, required this.undeliverableText,
      required this.cover});
}

String cartStatusText(String status) => switch (status) {
      'off_shelf' => '已失效',
      'not_sold_in_store' => '当前门店不售',
      'out_of_stock' => '暂时无货',
      'insufficient_stock' => '库存不足',
      _ => '',
    };

CartRow cartRow(CartItem it, String Function(String) asset) {
  final title = it.title ?? '规格 #${it.skuId}';
  final pid = it.productId ?? 0;
  final available = it.status == 'available';
  final price = it.priceCents;
  final list = it.listPriceCents;
  return CartRow(
    id: it.id,
    skuId: it.skuId,
    productId: pid,
    title: title,
    specText: (it.specValues ?? const {}).values.join(' / '),
    priceText: price == null ? '—' : yuan(price),
    listPriceText: price != null && list != null && list > price ? yuan(list) : '',
    quantity: it.quantity,
    // 服务端的 selected 可能还是 true（加购时勾上的），但不能买的行不进合计、不去结算。
    selected: it.selected && available,
    available: available,
    statusText: cartStatusText(it.status),
    undeliverableText: it.undeliverable?.reason ?? '',
    cover: coverOf(pid > 0 ? pid : it.skuId, title, it.imageUrl == null ? '' : asset(it.imageUrl!)),
  );
}

class CartView {
  final List<CartRow> rows;
  final String selectedText;
  final int selectedCount;
  final bool allSelected;
  final int availableCount;
  final int storeId;
  /// 预估运费。没有地址时服务端不给 freight：空串（算不了，不是包邮）。
  final String freightText;
  final String freightNote;
  /// 「运费」（快递，按模板）或「配送费」（同城，按距离）。
  final String freightLabel;
  /// 同城配送没到起送价：「还差 ¥z 起送」，此时去结算置灰。空 = 没有起送价或已够。
  final String shortfallText;
  /// 同城配送、地址没有坐标：配送费按最远一档计，提示给地址选点。
  final bool needsPin;
  final String promotionDiscountText;
  final List<String> promotionNotes;
  /// 总件数（角标）。
  final int quantity;
  const CartView({required this.rows, required this.selectedText, required this.selectedCount, required this.allSelected,
      required this.availableCount, required this.storeId, required this.freightText, required this.freightNote,
      this.freightLabel = '运费', this.shortfallText = '', this.needsPin = false, required this.promotionDiscountText, required this.promotionNotes, required this.quantity});

  bool get belowMinimum => shortfallText.isNotEmpty;

  /// 去结算带哪些行：能买且勾选的（与 selected_total_cents 同一口径）。
  List<({int skuId, int quantity})> get checkoutLines =>
      [for (final r in rows) if (r.available && r.selected) (skuId: r.skuId, quantity: r.quantity)];
}

CartView cartView(Cart c, String Function(String) asset) {
  final rows = c.items.map((it) => cartRow(it, asset)).toList();
  final avail = rows.where((r) => r.available).length;
  final selected = rows.where((r) => r.selected).length;
  final f = c.freight;
  return CartView(
    rows: rows,
    selectedText: yuan(c.selectedTotalCents),
    selectedCount: selected,
    allSelected: avail > 0 && selected == avail,
    availableCount: avail,
    storeId: c.store.storeId ?? 0,
    freightText: f == null ? '' : yuan(f.freightCents),
    freightNote: f == null ? '' : freightNote(f),
    freightLabel: freightLabel(f),
    shortfallText: shortfallText(f),
    needsPin: needsPin(f),
    promotionDiscountText: c.promotionDiscountCents > 0 ? '-${yuan(c.promotionDiscountCents)}' : '',
    promotionNotes: promotionNotes(c.promotions),
    quantity: cartQuantity(c),
  );
}

bool _local(FreightBreakdown? b) => b?.mode == 'local' && b?.local != null;

/// 同城配送（有围栏的门店）叫配送费，快递（默认店，按模板）叫运费。
String freightLabel(FreightBreakdown? b) => _local(b) ? '配送费' : '运费';

/// 同城配送没到起送价（比的是活动后、用券前的商品金额，与购物车显示的同一个数）。
String shortfallText(FreightBreakdown? b) {
  final q = _local(b) ? b!.local! : null;
  return q != null && q.shortfallCents > 0 ? '还差 ${yuan(q.shortfallCents)} 起送' : '';
}

/// 同城配送但地址没有坐标：服务端按最远一档计。
bool needsPin(FreightBreakdown? b) => _local(b) && b!.local!.distanceM == null;

/// 运费一句话说明。快递多组（多个模板）时只说第一组；同城说距离 / 免配送费 / 按最远一档。
String freightNote(FreightBreakdown b) {
  if (_local(b)) {
    final q = b.local!;
    if ((q.freeReason ?? '').isNotEmpty) return '已免配送费';
    final d = q.distanceM;
    if (d == null) return '按最远一档计';
    final km = '距离 ${(d / 1000).toStringAsFixed(1)} 公里';
    return q.freeOverCents > 0 ? '$km，满${faceYuan(q.freeOverCents)} 免配送费' : km;
  }
  if (b.groups.isEmpty) return '';
  final g = b.groups.first;
  final reason = g.freeReason ?? '';
  final rule = g.rule;
  if (reason == 'threshold' && rule != null) return '已满${faceYuan(rule.freeThresholdCents)} 包邮';
  if (reason == 'quantity' && rule != null) return '已满 ${rule.freeQuantity} 件包邮';
  if (reason.isNotEmpty || rule == null) return '';
  if (rule.freeThresholdCents > 0) return '满${faceYuan(rule.freeThresholdCents)} 包邮';
  return '首件${faceYuan(rule.firstFeeCents)}，续件${faceYuan(rule.additionalFeeCents)}';
}

/// 命中 / 差一点命中的活动说明：服务端的 message 原样，不在客户端拼「再买多少减多少」。
List<String> promotionNotes(List<PromotionHit> hits) => [for (final h in hits) if (h.message.isNotEmpty) h.message];

Map<String, String>? _q(int? storeId, int? addressId) {
  final q = <String, String>{if (storeId != null) 'store_id': '$storeId', if (addressId != null) 'address_id': '$addressId'};
  return q.isEmpty ? null : q;
}

Future<CartView> _cart(ApiClient c, String method, String path, int? storeId,
    {Object? body, String? key, int? addressId}) async {
  final res = await c.send(method, path, query: _q(storeId, addressId), body: body, idempotencyKey: key,
      decode: (j) => Cart.fromJson(j as Map<String, dynamic>));
  return cartView(res.data, c.assetUrl);
}

/// 与商品页、结算页同一家门店：购物车的价才和试算对得上。
Future<CartView> fetchCart(ApiClient c, {required int? storeId, int? addressId}) =>
    _cart(c, 'GET', '/cart', storeId, addressId: addressId);

Future<CartView> setCartChecked(ApiClient c, int itemId, bool selected, {required int? storeId}) =>
    _cart(c, 'PATCH', '/cart/items/$itemId', storeId, body: UpdateCartItemRequest(selected: selected).toJson());

/// 调大超过可售量是 409（detail 写了原因）；调小永远放行。
Future<CartView> setCartQuantity(ApiClient c, int itemId, int quantity, {required int? storeId}) =>
    _cart(c, 'PATCH', '/cart/items/$itemId', storeId, body: UpdateCartItemRequest(quantity: quantity).toJson());

/// 全选 / 全不选：item_ids 省略即全车。
Future<CartView> selectAllCart(ApiClient c, bool selected, {required int? storeId}) =>
    _cart(c, 'PUT', '/cart/selection', storeId, body: SelectCartItemsRequest(selected: selected).toJson());

/// 批量删除（必带幂等键）。重试时传同一个键：第一次其实删成了、只是响应丢了的话，第二次是重放。
Future<CartView> deleteCartItems(ApiClient c, List<int> itemIds, {required int? storeId, String? idempotencyKey}) =>
    _cart(c, 'POST', '/cart/items/batch-delete', storeId,
        body: BatchDeleteCartItemsRequest(itemIds: itemIds).toJson(), key: idempotencyKey ?? newIdempotencyKey());
