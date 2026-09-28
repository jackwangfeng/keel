import 'package:flutter/material.dart';

import '../api/view.dart';
import '../theme.dart';
import 'net_image.dart';

/// 首页网格的商品卡：封面（真图 / 首字色块）、标题两行省略、活动标签、价格（多规格加「起」）。
class ProductCard extends StatelessWidget {
  const ProductCard({
    super.key,
    required this.row,
    required this.onTap,
    this.onAdd,
  });
  final ProductRow row;
  final VoidCallback onTap;

  /// 「＋」原地加购；下架 / 缺货的不显示。
  final VoidCallback? onAdd;

  /// 网格里的固定行高：封面 156 + 内边距 12/14 + 两行标题 40 + 2 + 副标题 17 + 标签 4+19 + 余量 4 + 4 + 价格行 28 + 2。
  static const gridExtent = 302.0;

  bool get _off => row.offShelf || row.soldOut;
  String get _offText => row.offShelf ? '已下架' : '无货';

  @override
  Widget build(BuildContext context) {
    final c = row.cover;
    return GestureDetector(
      key: Key('product.card.${row.id}'),
      onTap: onTap,
      child: Container(
        decoration: BoxDecoration(
          color: KeelColors.card,
          borderRadius: BorderRadius.circular(16),
        ),
        clipBehavior: Clip.antiAlias,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            SizedBox(
              height: 156,
              child: Stack(
                fit: StackFit.expand,
                children: [
                  c.imageUrl.isNotEmpty
                      ? NetImage(url: c.imageUrl, fallback: _Glyph(cover: c))
                      : _Glyph(cover: c),
                  // 无货 / 下架：整张封面压一层灰，正中写字 —— 角落一个小标签在两列网格里太容易看漏。
                  if (_off)
                    ColoredBox(
                      key: Key('product.mask.${row.id}'),
                      color: const Color(0x8CFFFDF9),
                      child: Center(
                        child: Container(
                          padding: const EdgeInsets.symmetric(
                            horizontal: 14,
                            vertical: 4,
                          ),
                          decoration: BoxDecoration(
                            color: const Color(0xB82A211B),
                            borderRadius: BorderRadius.circular(14),
                          ),
                          child: Text(
                            _offText,
                            style: const TextStyle(
                              fontSize: 15,
                              fontWeight: FontWeight.w700,
                              color: KeelColors.card,
                            ),
                          ),
                        ),
                      ),
                    ),
                ],
              ),
            ),
            Expanded(
              child: Padding(
                padding: const EdgeInsets.fromLTRB(12, 12, 12, 14),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    // 标题 / 副标题 / 标签这一块占剩下的高度并裁掉超出部分（行高固定，内容少时价格照样贴底），
                    // 字体还在加载时测量差一两像素，不裁就是「溢出 1–2 像素」；价格行永远贴底。
                    Expanded(
                      child: ClipRect(
                        child: OverflowBox(
                          alignment: Alignment.topLeft,
                          minHeight: 0,
                          maxHeight: double.infinity,
                          child: Column(
                            mainAxisSize: MainAxisSize.min,
                            crossAxisAlignment: CrossAxisAlignment.start,
                            children: [
                              Text(
                                row.title,
                                maxLines: 2,
                                overflow: TextOverflow.ellipsis,
                                style: KeelText.body.copyWith(
                                  fontWeight: FontWeight.w700,
                                  height: 20 / 14,
                                ),
                              ),
                              const SizedBox(height: 2),
                              if (row.subtitle.isNotEmpty)
                                Text(
                                  row.subtitle,
                                  maxLines: 1,
                                  overflow: TextOverflow.ellipsis,
                                  style: KeelText.hint.copyWith(
                                    height: 17 / 12,
                                  ),
                                ),
                              if (row.promoTags.isNotEmpty)
                                Padding(
                                  padding: const EdgeInsets.only(top: 4),
                                  child: Wrap(
                                    spacing: 4,
                                    runSpacing: 2,
                                    children: [
                                      for (final tag in row.promoTags)
                                        Container(
                                          padding: const EdgeInsets.symmetric(
                                            horizontal: 5,
                                            vertical: 1,
                                          ),
                                          decoration: BoxDecoration(
                                            border: Border.all(
                                              color: const Color(0xFFE8B4A6),
                                            ),
                                            borderRadius: BorderRadius.circular(
                                              4,
                                            ),
                                          ),
                                          child: Text(
                                            tag,
                                            style: const TextStyle(
                                              fontSize: 11,
                                              color: KeelColors.err,
                                            ),
                                          ),
                                        ),
                                    ],
                                  ),
                                ),
                              // 4 像素余量：字体测量差一两像素（标签、行高）时，
                              // 余量在裁剪区里，被裁的是它而不是标签的下边框。
                              const SizedBox(height: 4),
                            ],
                          ),
                        ),
                      ),
                    ),
                    const SizedBox(height: 4),
                    Row(
                      crossAxisAlignment: CrossAxisAlignment.center,
                      children: [
                        // 价格占剩下的宽度（单行，超长省略），「＋」固定在右边永远不被挤。
                        Expanded(
                          child: Row(
                            children: [
                              Flexible(
                                child: Text(
                                  row.minPriceText,
                                  maxLines: 1,
                                  overflow: TextOverflow.ellipsis,
                                  style: _off
                                      ? KeelText.price.copyWith(
                                          color: _offPrice,
                                        )
                                      : KeelText.price,
                                ),
                              ),
                              if (row.hasRange)
                                const Padding(
                                  padding: EdgeInsets.only(left: 2),
                                  child: Text(
                                    '起',
                                    style: TextStyle(
                                      fontSize: 11,
                                      color: KeelColors.textHint,
                                    ),
                                  ),
                                ),
                            ],
                          ),
                        ),
                        // 买不了时「＋」的位置换成一个灰标签：不给一个点了才告诉你无货的按钮。
                        if (_off)
                          SoldTag(
                            key: Key('product.sold.${row.id}'),
                            text: _offText,
                          )
                        else if (onAdd != null)
                          AddButton(id: row.id, onTap: onAdd!),
                      ],
                    ),
                  ],
                ),
              ),
            ),
          ],
        ),
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
    child: Center(
      child: Text(
        cover.glyph,
        style: const TextStyle(
          fontSize: 44,
          fontWeight: FontWeight.w700,
          color: KeelColors.card,
        ),
      ),
    ),
  );
}

/// 「＋」：原地加购的入口。
class AddButton extends StatelessWidget {
  const AddButton({super.key, required this.id, required this.onTap});
  final int id;
  final VoidCallback onTap;
  @override
  Widget build(BuildContext context) => GestureDetector(
    key: Key('product.add.$id'),
    onTap: onTap,
    child: Container(
      width: 28,
      height: 28,
      alignment: Alignment.center,
      margin: const EdgeInsets.only(left: 8),
      decoration: const BoxDecoration(
        color: KeelColors.primary,
        shape: BoxShape.circle,
      ),
      child: const Icon(Icons.add, size: 18, color: KeelColors.card),
    ),
  );
}

/// 搜索结果的一行：封面、标题、完整价格区间；缺货 / 下架标出来并压暗（不藏）。
class ProductTile extends StatelessWidget {
  const ProductTile({super.key, required this.row, required this.onTap, this.onAdd});
  final ProductRow row;
  final VoidCallback onTap;
  final VoidCallback? onAdd;

  // 左图右文（uni-app x 的 .pc-row）：内边距 12、72 的图、标题 15 最多两行；
  // 价格行里右边是「＋」或灰标签，和价格齐平。缺货 / 下架整行压暗 0.6（不藏）。
  @override
  Widget build(BuildContext context) {
    final c = row.cover;
    final dim = row.soldOut || row.offShelf;
    return GestureDetector(
      key: Key('search.row.${row.id}'),
      onTap: onTap,
      behavior: HitTestBehavior.opaque,
      child: Opacity(
        opacity: dim ? 0.6 : 1,
        child: Container(
          margin: const EdgeInsets.only(top: 12),
          padding: const EdgeInsets.all(12),
          decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
          child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
            ClipRRect(
              borderRadius: BorderRadius.circular(12),
              child: SizedBox(
                width: 72, height: 72,
                child: c.imageUrl.isNotEmpty
                    ? NetImage(url: c.imageUrl, fallback: _Glyph(cover: c))
                    : _Glyph(cover: c),
              ),
            ),
            const SizedBox(width: 12),
            Expanded(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Text(row.title, maxLines: 2, overflow: TextOverflow.ellipsis,
                    style: const TextStyle(fontSize: 15, height: 21 / 15, fontWeight: FontWeight.w700, color: KeelColors.text)),
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
                Row(children: [
                  Expanded(child: Text(row.priceText, maxLines: 1, overflow: TextOverflow.ellipsis, style: KeelText.price)),
                  if (dim) SoldTag(key: Key('search.sold.${row.id}'), text: row.offShelf ? '已下架' : '无货'),
                  if (onAdd != null && !dim) AddButton(id: row.id, onTap: onAdd!),
                ]),
              ]),
            ),
          ]),
        ),
      ),
    );
  }
}

const _offPrice = Color(0xFFA89B8C);

/// 买不了时的灰标签（「无货」/「已下架」）。
class SoldTag extends StatelessWidget {
  const SoldTag({super.key, required this.text});
  final String text;
  @override
  Widget build(BuildContext context) => Container(
    height: 24,
    padding: const EdgeInsets.symmetric(horizontal: 8),
    alignment: Alignment.center,
    decoration: BoxDecoration(
      color: const Color(0xFFEFE9E2),
      borderRadius: BorderRadius.circular(12),
    ),
    child: Text(
      text,
      style: const TextStyle(fontSize: 12, color: Color(0xFF9A8D7E)),
    ),
  );
}
