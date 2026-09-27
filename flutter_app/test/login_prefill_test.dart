import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/pages/login_page.dart';

void main() {
  testWidgets('登录页预填演示买家，不用手输', (t) async {
    await t.pumpWidget(const MaterialApp(home: LoginPage(from: '')));
    expect(t.widget<TextField>(find.byKey(const Key('login.phone'))).controller!.text, '13800000000');
    expect(t.widget<TextField>(find.byKey(const Key('login.password'))).controller!.text, 'keel-demo-2026');
  });
}
