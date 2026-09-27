import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/api/schema.g.dart';
import 'package:keel_buyer/api/view.dart';

void main() {
  test('yuan', () {
    expect(yuan(4990), '¥49.90');
    expect(yuan(5), '¥0.05');
    expect(yuan(-800), '-¥8.00');
    expect(yuan(null), '¥0.00');
  });

  test('shortTime：UTC 转本地；带微秒；解析不了原样', () {
    final iso = '2026-09-26T22:55:37.188864Z';
    final d = DateTime.parse(iso).toLocal();
    String p(int n) => n.toString().padLeft(2, '0');
    expect(shortTime(iso), '${p(d.month)}-${p(d.day)} ${p(d.hour)}:${p(d.minute)}');
    expect(shortTime('昨天'), '昨天');
  });

  test('productRow：多规格价格区间、特价标签、图片补 origin', () {
    const p = ProductSummary(id: 19, title: '挂耳咖啡 10 包', minPriceCents: 6900, maxPriceCents: 8900, status: 1,
        imageUrl: '/api/v1/uploads/3', promotionTags: [
          PromotionTag(promotionId: 2, promotionType: 3, label: '限时特价 ¥49.9'),
          PromotionTag(promotionId: 1, promotionType: 1, label: '满199减20'),
          PromotionTag(promotionId: 9, promotionType: 1, label: '第三个'),
        ]);
    final r = productRow(p, (u) => 'http://h$u');
    expect(r.priceText, '¥69.00 ~ ¥89.00');
    expect(r.minPriceText, '¥69.00');
    expect(r.hasRange, isTrue);
    expect(r.promoTags, ['限时特价 ¥49.9', '满199减20']);
    expect(r.cover.imageUrl, 'http://h/api/v1/uploads/3');
    expect(r.cover.glyph, '挂');
  });

  test('productRow：in_stock 缺席时不说它没货；下架标出来', () {
    const p = ProductSummary(id: 1, title: 'A', minPriceCents: 100, status: 2);
    final r = productRow(p, (u) => u);
    expect(r.soldOut, isFalse);
    expect(r.offShelf, isTrue);
    expect(r.cover.imageUrl, '');
  });
}
