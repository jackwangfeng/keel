import 'dart:math' as math;

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:mp_flutter_wechat/mp_flutter_wechat.dart';

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

/// 解码用的像素比。小程序里封顶 2x：iPhone 上图片在 wasm 里解，3x 屏的网格图会取到 640 档，
/// 实测一张 60–100ms、正好卡在新图进屏那一帧；2x 取 480 档，像素少近一半，商品卡这个尺寸看不出差别。
/// 原生（手机 App / 浏览器）解码快，照设备像素比。
double decodePixelRatio(double dpr, {required bool miniProgram}) => miniProgram ? math.min(dpr, 2) : dpr;

/// 传给 Image 的 cacheWidth。Web（含小程序）引擎带 cacheWidth 时要先按原尺寸解一遍拿宽高、再按目标尺寸解一遍
/// （flutter_web_sdk lib/ui/painting.dart instantiateImageCodecWithSize），iPhone 上实测每张图 decode-slow 成对出现；
/// 缩略图已经是服务端按档位缩好的，就不再传。外链原图可能很大，照旧按显示尺寸解；原生只解一遍，照旧传、省内存。
int? decodeWidth(int px, {required bool thumb, required bool web}) => px <= 0 || (thumb && web) ? null : px;

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
      final px = (w * decodePixelRatio(MediaQuery.devicePixelRatioOf(context), miniProgram: MpWechat.isAvailable)).round();
      final src = thumbUrl(url, px);
      return Image.network(src, width: width, height: height, fit: BoxFit.cover,
          cacheWidth: decodeWidth(px, thumb: src != url, web: kIsWeb),
          errorBuilder: (_, _, _) => fallback ?? const SizedBox());
    }

    if (width != null) return image(width!);
    return LayoutBuilder(builder: (context, c) => image(c.hasBoundedWidth ? c.maxWidth : 400));
  }
}
