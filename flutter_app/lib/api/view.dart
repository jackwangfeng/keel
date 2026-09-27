import 'package:flutter/painting.dart';

import 'client.dart';
import 'schema.g.dart';
import 'session.dart';

/// 契约类型 -> 页面要显示的东西。页面只碰这里的行模型，契约字段一个都不写（页面闸门强制）。

String yuan(int? cents) {
  final c = cents ?? 0;
  final neg = c < 0;
  final a = c.abs();
  return '${neg ? '-' : ''}¥${a ~/ 100}.${(a % 100).toString().padLeft(2, '0')}';
}

/// 「2026-09-26T22:55:37.188864Z」-> 本地时区「09-27 06:55」。解析不了就原样返回。
String shortTime(String iso) {
  final d = DateTime.tryParse(iso);
  if (d == null) return iso;
  final l = d.toLocal();
  String p(int n) => n.toString().padLeft(2, '0');
  return '${p(l.month)}-${p(l.day)} ${p(l.hour)}:${p(l.minute)}';
}

class Cover {
  final String imageUrl;
  final Color color;
  final String glyph;
  const Cover(this.imageUrl, this.color, this.glyph);
}

const _tones = [Color(0xFFC9A27E), Color(0xFFA7B49A), Color(0xFFD4B896), Color(0xFFB98A6E), Color(0xFF9FA8A3), Color(0xFFC7A9A0)];

/// 有图显示图；没有图用按 id 取色的色块 + 标题首字（同一件商品各处颜色一致）。
Cover coverOf(int key, String title, String imageUrl) =>
    Cover(imageUrl, _tones[key.abs() % _tones.length], title.isEmpty ? '·' : title.substring(0, 1));

class ProductRow {
  final int id;
  final String title;
  final String subtitle;
  final String priceText;
  final bool hasRange;
  final bool offShelf;
  final bool soldOut;
  final Cover cover;
  final List<String> promoTags;
  const ProductRow({required this.id, required this.title, required this.subtitle, required this.priceText,
      required this.hasRange, required this.offShelf, required this.soldOut, required this.cover, required this.promoTags});
}

/// 首页卡片：多规格只显示最低价 + 「起」（hasRange），搜索页另显示完整区间（第 2 步）。
ProductRow productRow(ProductSummary p, String Function(String) asset) {
  final max = p.maxPriceCents;
  return ProductRow(
    id: p.id,
    title: p.title,
    subtitle: p.subtitle ?? '',
    priceText: yuan(p.minPriceCents),
    hasRange: max != null && max > p.minPriceCents,
    offShelf: p.status == 2,
    soldOut: p.inStock == false,
    cover: coverOf(p.id, p.title, p.imageUrl == null ? '' : asset(p.imageUrl!)),
    promoTags: (p.promotionTags ?? const []).take(2).map((t) => t.label).toList(),
  );
}

Future<List<ProductRow>> fetchProducts(ApiClient c, {required int? storeId, int page = 1, int pageSize = 20}) async {
  final q = <String, String>{'page': '$page', 'page_size': '$pageSize'};
  if (storeId != null) q['store_id'] = '$storeId';
  final res = await c.send('GET', '/products', query: q,
      decode: (j) => ListProductsResponse.fromJson(j as Map<String, dynamic>));
  return res.data.items.map((p) => productRow(p, c.assetUrl)).toList();
}

Future<void> login(ApiClient c, Session s, String phone, String password) async {
  final res = await c.send('POST', '/auth/login', body: LoginRequest(phone: phone, password: password).toJson(),
      decode: (j) => LoginResponse.fromJson(j as Map<String, dynamic>));
  await s.save(res.data);
}

Future<void> logout(ApiClient c, Session s) async {
  try {
    await c.send('POST', '/auth/logout', decode: (_) => null);
  } catch (_) {
    // 服务端吊销失败也要把本机清掉。
  }
  await s.clear();
}
