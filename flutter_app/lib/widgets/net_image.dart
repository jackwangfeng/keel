import 'package:flutter/material.dart';

/// 网络图：按显示尺寸解码（cacheWidth = 逻辑宽 × 像素比），不把原图整张解进内存 ——
/// 列表里一张 1200 宽的商品图只显示 173 宽，小程序（mp-flutter 支持 cacheWidth）和手机上都省内存。
/// 只限宽，高按原图比例，BoxFit.cover 不变形。width 不给就按布局约束的宽算。读不出来显示 fallback。
class NetImage extends StatelessWidget {
  const NetImage({super.key, required this.url, this.width, this.height, this.fallback});
  final String url;
  final double? width;
  final double? height;
  final Widget? fallback;

  @override
  Widget build(BuildContext context) {
    Widget image(double w) {
      final px = (w * MediaQuery.devicePixelRatioOf(context)).round();
      return Image.network(url, width: width, height: height, fit: BoxFit.cover, cacheWidth: px > 0 ? px : null,
          errorBuilder: (_, _, _) => fallback ?? const SizedBox());
    }

    if (width != null) return image(width!);
    return LayoutBuilder(builder: (context, c) => image(c.hasBoundedWidth ? c.maxWidth : 400));
  }
}
