import 'package:flutter/material.dart';

/// 服务端缩略图档位（GET /uploads/{id}?w=，契约只认这四档，其它值向上取、超过 640 按 640）。
const thumbWidths = [160, 320, 480, 640];

/// 我们自己的上传文件（…/uploads/{id}，还没带参数）拼上缩略图宽度：按显示像素向上取档，与服务端的归档一致，
/// 不打散它的缓存。外链、已经带参数的地址（比如 302 之后的限时地址）原样返回。
/// 服务端还没上线这个参数时多出来的 ?w= 被忽略、给原图，行为不变。
String thumbUrl(String url, int px) {
  if (!RegExp(r'/uploads/\d+$').hasMatch(url)) return url;
  final w = thumbWidths.firstWhere((t) => t >= px, orElse: () => thumbWidths.last);
  return '$url?w=$w';
}

/// 网络图：向服务端要按显示尺寸缩好的图（thumbUrl），并按显示尺寸解码（cacheWidth = 逻辑宽 × 像素比）——
/// iPhone 小程序里图片在 wasm 里解码（没有 JIT），解 800×800 比解 320 宽慢好几倍；
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
      return Image.network(thumbUrl(url, px), width: width, height: height, fit: BoxFit.cover, cacheWidth: px > 0 ? px : null,
          errorBuilder: (_, _, _) => fallback ?? const SizedBox());
    }

    if (width != null) return image(width!);
    return LayoutBuilder(builder: (context, c) => image(c.hasBoundedWidth ? c.maxWidth : 400));
  }
}
