import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/capsule.dart';
import 'package:keel_buyer/theme.dart';
import 'package:mp_flutter_wechat/mp_flutter_wechat.dart';

class FakeMp implements MpWechatChannel {
  FakeMp(this.rect);
  final Map<String, num>? rect;
  @override
  bool get isAvailable => true;
  @override
  Future<String> call(String api, String paramsJson) async => '{}';
  @override
  void setShareInfo(String json) {}
  @override
  Future<String?> menuButtonRect() async => rect == null ? null : jsonEncode(rect);
}

void main() {
  tearDown(() => MpWechat.debugSetChannel(null));

  test('小程序里：顶栏右侧让出胶囊（屏宽 − 胶囊左边 + 8）', () async {
    MpWechat.debugSetChannel(FakeMp({'left': 281, 'top': 51, 'right': 368, 'bottom': 83, 'width': 87, 'height': 32}));
    expect(await capsuleInset(390), 117);
  });

  test('不在小程序里 / 拿不到胶囊：不让位', () async {
    expect(await capsuleInset(390), 0);
    MpWechat.debugSetChannel(FakeMp(null));
    expect(await capsuleInset(390), 0);
  });

  test('主题把让位用在顶栏右侧按钮上', () {
    expect(keelTheme(actionsRight: 117).appBarTheme.actionsPadding, const EdgeInsets.only(right: 117));
    expect(keelTheme().appBarTheme.actionsPadding, isNull);
  });
}
