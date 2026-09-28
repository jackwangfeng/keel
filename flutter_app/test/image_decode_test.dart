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

  test('小程序里解码像素比封顶 2x（wasm 解码，640 图一张 60–100ms）；原生照设备像素比', () {
    expect(decodePixelRatio(3, miniProgram: true), 2);
    expect(decodePixelRatio(1.5, miniProgram: true), 1.5);
    expect(decodePixelRatio(3, miniProgram: false), 3);
  });

  group('缩略图档位（GET /uploads/{id}?w=，服务端只认 160 / 320 / 480 / 640）', () {
    test('按显示像素向上取档，超过 640 按 640', () {
      expect(thumbUrl('http://h/api/v1/uploads/23', 100), 'http://h/api/v1/uploads/23?w=160');
      expect(thumbUrl('http://h/api/v1/uploads/23', 160), 'http://h/api/v1/uploads/23?w=160');
      expect(thumbUrl('http://h/api/v1/uploads/23', 216), 'http://h/api/v1/uploads/23?w=320');
      expect(thumbUrl('http://h/api/v1/uploads/23', 519), 'http://h/api/v1/uploads/23?w=640');
      expect(thumbUrl('/api/v1/uploads/7', 2000), '/api/v1/uploads/7?w=640');
    });
    test('不是我们自己的上传文件（外链、已带参数的）不动', () {
      expect(thumbUrl('https://cdn.example.com/a.jpg', 300), 'https://cdn.example.com/a.jpg');
      expect(thumbUrl('http://h/api/v1/uploads/23?w=160', 300), 'http://h/api/v1/uploads/23?w=160');
      expect(thumbUrl('http://h/api/v1/uploads/23/blob?sig=x', 300), 'http://h/api/v1/uploads/23/blob?sig=x');
    });
    testWidgets('NetImage 请求的是缩略图', (t) async {
      t.view.devicePixelRatio = 3;
      addTearDown(t.view.reset);
      await t.pumpWidget(const MaterialApp(home: Center(child: NetImage(url: 'http://h/api/v1/uploads/23', width: 72, height: 72))));
      final r = t.widget<Image>(find.byType(Image)).image as ResizeImage;
      expect((r.imageProvider as NetworkImage).url, 'http://h/api/v1/uploads/23?w=320');
    });
  });
}
