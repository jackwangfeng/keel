import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/widgets/net_image.dart';

void main() {
  testWidgets('列表图按显示尺寸解码（cacheWidth = 逻辑宽 × 像素比），不整张解原图', (t) async {
    t.view.devicePixelRatio = 3;
    addTearDown(t.view.reset);
    await t.pumpWidget(const MaterialApp(home: Center(child: NetImage(url: 'http://h/a.png', width: 72, height: 72))));
    final img = t.widget<Image>(find.byType(Image));
    expect(img.image, isA<ResizeImage>());
    final r = img.image as ResizeImage;
    expect(r.width, 216);
    expect(r.height, isNull, reason: '只限宽，高按原图比例，BoxFit.cover 不变形');
  });
}
