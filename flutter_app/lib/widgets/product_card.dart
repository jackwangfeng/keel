import 'package:flutter/material.dart';

import '../api/view.dart';
import '../theme.dart';

/// 首页网格的商品卡：封面（真图 / 首字色块）、标题两行省略、活动标签、价格（多规格加「起」）。
class ProductCard extends StatelessWidget {
  const ProductCard({super.key, required this.row, required this.onTap});
  final ProductRow row;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    final c = row.cover;
    return GestureDetector(
      key: Key('product.card.${row.id}'),
      onTap: onTap,
      child: Container(
        decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
        clipBehavior: Clip.antiAlias,
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          AspectRatio(
            aspectRatio: 1,
            child: c.imageUrl.isNotEmpty
                ? Image.network(c.imageUrl, fit: BoxFit.cover,
                    errorBuilder: (_, _, _) => _Glyph(cover: c))
                : _Glyph(cover: c),
          ),
          Padding(
            padding: const EdgeInsets.fromLTRB(12, 10, 12, 12),
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              Text(row.title, maxLines: 2, overflow: TextOverflow.ellipsis,
                  style: KeelText.body.copyWith(fontWeight: FontWeight.w700)),
              if (row.subtitle.isNotEmpty) Text(row.subtitle, maxLines: 1, overflow: TextOverflow.ellipsis, style: KeelText.hint),
              if (row.promoTags.isNotEmpty)
                Padding(
                  padding: const EdgeInsets.only(top: 4),
                  child: Wrap(spacing: 4, runSpacing: 2, children: [
                    for (final tag in row.promoTags)
                      Container(
                        padding: const EdgeInsets.symmetric(horizontal: 5, vertical: 1),
                        decoration: BoxDecoration(border: Border.all(color: const Color(0xFFE8B4A6)), borderRadius: BorderRadius.circular(4)),
                        child: Text(tag, style: const TextStyle(fontSize: 11, color: KeelColors.err)),
                      ),
                  ]),
                ),
              const SizedBox(height: 6),
              Row(crossAxisAlignment: CrossAxisAlignment.end, children: [
                Text(row.priceText, style: KeelText.price),
                if (row.hasRange) const Text(' 起', style: KeelText.hint),
              ]),
            ]),
          ),
        ]),
      ),
    );
  }
}

class _Glyph extends StatelessWidget {
  const _Glyph({required this.cover});
  final Cover cover;
  @override
  Widget build(BuildContext context) => ColoredBox(
        color: cover.color,
        child: Center(child: Text(cover.glyph, style: const TextStyle(fontSize: 44, fontWeight: FontWeight.w700, color: KeelColors.card))),
      );
}
