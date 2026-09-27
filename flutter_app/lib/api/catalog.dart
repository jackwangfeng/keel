import 'client.dart';
import 'schema.g.dart';
import 'view.dart';

/// 浏览：商品列表、分类、详情、搜索（含效果回传的归因）、加购。
/// 行为照 uni-app x 的 app/src/api/view.uts、search-trace.uts 与各页面移植。

class ProductPage {
  final List<ProductRow> rows;
  final int total;
  const ProductPage(this.rows, this.total);
}

/// categoryId 为 null = 「全部」，不带这个参数。
Future<ProductPage> fetchProducts(ApiClient c, {required int? storeId, int? categoryId, int page = 1, int pageSize = 20}) async {
  final q = <String, String>{'page': '$page', 'page_size': '$pageSize'};
  if (storeId != null) q['store_id'] = '$storeId';
  if (categoryId != null) q['category_id'] = '$categoryId';
  final res = await c.send('GET', '/products', query: q,
      decode: (j) => ListProductsResponse.fromJson(j as Map<String, dynamic>));
  return ProductPage(res.data.items.map((p) => productRow(p, c.assetUrl)).toList(), res.data.total);
}

class CategoryChip {
  final int id;
  final String name;
  const CategoryChip(this.id, this.name);
}

/// 首页那一排分类：只取根分类。拉不到（包括服务端还没有这条路由）就返回空表 ——
/// 分类是浏览的捷径，不是浏览的前提。
Future<List<CategoryChip>> fetchCategories(ApiClient c) async {
  try {
    final res = await c.send('GET', '/categories',
        decode: (j) => (j as List).map((e) => Category.fromJson(e as Map<String, dynamic>)).toList());
    return res.data.map((t) => CategoryChip(t.id, t.name)).toList();
  } on ApiFailure {
    return const [];
  }
}

class SkuRow {
  final int id;
  /// 规格值拼成的短标签（「中度」「900ml」）；没有规格值时退回 sku_code。
  final String label;
  /// 规格名（「烘焙」）；页面拿它当规格区的标题。
  final String specName;
  /// 实际成交单价：有限时特价就是特价。
  final String priceText;
  /// 有特价时的门店价（划线用）；没有是空串。
  final String listPriceText;
  final int availableQty;
  final bool soldOut;
  const SkuRow({required this.id, required this.label, required this.specName, required this.priceText,
      required this.listPriceText, required this.availableQty, required this.soldOut});
}

SkuRow skuRow(Sku s) {
  final sv = s.specValues ?? const <String, String>{};
  final promo = s.promoPriceCents != null && s.promoPriceCents! < s.priceCents;
  return SkuRow(
    id: s.id,
    label: sv.isEmpty ? s.skuCode : sv.values.join(' / '),
    specName: sv.isEmpty ? '规格' : sv.keys.join(' / '),
    priceText: promo ? yuan(s.promoPriceCents) : yuan(s.priceCents),
    listPriceText: promo ? yuan(s.priceCents) : '',
    availableQty: s.availableQty,
    soldOut: s.availableQty <= 0,
  );
}

class ProductDetailView {
  final int id;
  final String title;
  final String subtitle;
  final String description;
  final String priceText;
  final bool offShelf;
  final List<SkuRow> skus;
  final Cover cover;
  final List<String> images;
  final List<String> promoTags;
  const ProductDetailView({required this.id, required this.title, required this.subtitle, required this.description,
      required this.priceText, required this.offShelf, required this.skus, required this.cover, required this.images,
      required this.promoTags});

  SkuRow? sku(int id) {
    for (final s in skus) {
      if (s.id == id) return s;
    }
    return null;
  }

  /// 默认选中的规格：第一个有货的；全都没货是 null。
  SkuRow? get firstBuyable {
    for (final s in skus) {
      if (!s.soldOut) return s;
    }
    return null;
  }
}

/// 有货的规格在前、无货的排后，各自保持服务端的顺序（稳定分区）：无货的仍然列着、标「无货」，
/// 不藏 —— 藏了就分不清「本来没有 XL」和「XL 卖完了」。
ProductDetailView productDetailView(ProductDetail p, String Function(String) asset) {
  final rows = p.skus.map(skuRow).toList();
  final cover = coverOf(p.id, p.title, p.imageUrl == null ? '' : asset(p.imageUrl!));
  final images = (p.images ?? const <String>[]).map(asset).where((u) => u.isNotEmpty).toList();
  return ProductDetailView(
    id: p.id,
    title: p.title,
    subtitle: p.subtitle ?? '',
    description: p.description ?? '',
    priceText: yuan(p.minPriceCents),
    offShelf: p.status == 2,
    skus: [...rows.where((r) => !r.soldOut), ...rows.where((r) => r.soldOut)],
    cover: cover,
    images: images.isNotEmpty ? images : (cover.imageUrl.isEmpty ? const [] : [cover.imageUrl]),
    promoTags: promoTagLabels(p.promotionTags),
  );
}

/// 详情的价格要和列表、结算同一家店算：storeId 与列表页用的同一个；null 服务端回落默认店。
Future<ProductDetailView> fetchProduct(ApiClient c, int productId, int? storeId) async {
  final res = await c.send('GET', '/products/$productId',
      query: storeId == null ? null : {'store_id': '$storeId'},
      decode: (j) => ProductDetail.fromJson(j as Map<String, dynamic>));
  return productDetailView(res.data, c.assetUrl);
}

class SearchResult {
  final List<ProductRow> rows;
  /// 这批结果的 trace_id（效果回传用）；服务端写日志失败时缺席，是空串 —— 这批的回传全部跳过。
  final String traceId;
  const SearchResult(this.rows, this.traceId);
}

/// POST /search（公开）。一期不翻页，结果上限就是 size。query 超过 200 字截断（契约 1~200）。
Future<SearchResult> searchProducts(ApiClient c, String query, int? storeId, {int size = 20}) async {
  final q = query.length > 200 ? query.substring(0, 200) : query;
  final res = await c.send('POST', '/search', body: SearchRequest(query: q, size: size, storeId: storeId).toJson(),
      decode: (j) => SearchResponse.fromJson(j as Map<String, dynamic>));
  return SearchResult(res.data.items.map((h) => searchHitRow(h, c.assetUrl)).toList(), res.data.traceId ?? '');
}

/// 搜索效果回传的归因：只有从搜索结果点进去的（或在结果里原地加购的）商品才算「来自这次搜索」。
/// 同一件商品多次被搜索点进去以最近一次为准；只记在内存里（重启丢掉只是少几条统计）。
class SearchTrace {
  SearchTrace(this._c);
  final ApiClient _c;
  static const _max = 50;
  final _traces = <int, String>{};

  void _remember(int productId, String traceId) {
    _traces.remove(productId);
    _traces[productId] = traceId;
    while (_traces.length > _max) {
      _traces.remove(_traces.keys.first);
    }
  }

  Future<void> _report(String traceId, String event, int productId) => _c.fireAndForget('/search/events',
      ReportSearchEventRequest(traceId: traceId, event: event, productId: productId).toJson());

  /// 从搜索结果点进商品：记下归因并回传 click。
  Future<void> clicked(String traceId, int productId) async {
    if (traceId.isEmpty || productId <= 0) return;
    _remember(productId, traceId);
    await _report(traceId, 'click', productId);
  }

  /// 在结果列表里原地加购（没点进详情）：只报 add_cart，不补 click（那会把点击率算高）。
  Future<void> addedFromList(String traceId, int productId) async {
    if (traceId.isEmpty || productId <= 0) return;
    _remember(productId, traceId);
    await _report(traceId, 'add_cart', productId);
  }

  /// 加购 / 下单：这件商品是从某次搜索来的就回传对应事件，否则什么都不做。
  Future<void> converted(String event, int productId) async {
    final t = _traces[productId];
    if (t == null) return;
    await _report(t, event, productId);
  }
}

int cartQuantity(Cart c) => c.items.fold(0, (n, it) => n + it.quantity);

/// POST /cart/items（必带幂等键）。每次点击一个新键：再点一次就是想再加一件（服务端累加）。
/// 返回车里的总件数（角标用）。三种常见问题翻成页面上的话。
Future<int> addToCart(ApiClient c, {required int skuId, required int quantity, required int? storeId}) async {
  try {
    final res = await c.send('POST', '/cart/items',
        query: storeId == null ? null : {'store_id': '$storeId'},
        body: AddCartItemRequest(skuId: skuId, quantity: quantity).toJson(),
        idempotencyKey: newIdempotencyKey(),
        decode: (j) => Cart.fromJson(j as Map<String, dynamic>));
    return cartQuantity(res.data);
  } on ApiFailure catch (f) {
    if (f.isType('sku-not-sold-in-store')) throw f.withMessage('当前门店不卖这个规格');
    if (f.isType('cart-quantity-exceeded')) throw f.withMessage('购物车里这个规格已经到上限了');
    if (f.isType('insufficient-stock')) throw f.withMessage('库存不够了');
    rethrow;
  }
}
