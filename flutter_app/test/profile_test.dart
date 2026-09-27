import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/profile.dart';
import 'package:keel_buyer/api/schema.g.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:shared_preferences/shared_preferences.dart';

void main() {
  test('资料：性别文案；没绑手机号是空串；第三方名字', () {
    final v = meView(User.fromJson({'id': 1, 'nickname': '小明', 'gender': 2, 'has_password': true}));
    expect(v.genderText, '女');
    expect(v.phone, '');
    expect(v.hasPassword, isTrue);
    expect(meView(User.fromJson({'id': 1, 'nickname': 'x'})).genderText, '未设置');
    expect(identityName(1), '微信小程序');
    expect(identityName(9), '第三方账号 #9');
  });

  test('PATCH /me 只带改了的字段；保存后本机会话里的昵称跟着改', () async {
    SharedPreferences.setMockInitialValues({'keel.access': 'a', 'keel.nickname': '旧名'});
    final s = Session();
    await s.load();
    Map? sent;
    final c = ApiClient(base: 'http://h/api/v1', session: s, http: MockClient((r) async {
      sent = jsonDecode(r.body) as Map;
      return http.Response(jsonEncode({'id': 1, 'nickname': '新名', 'gender': 1}), 200,
          headers: {'content-type': 'application/json; charset=utf-8'});
    }));
    final v = await updateMe(c, s, nickname: '新名', gender: null);
    expect(sent, {'nickname': '新名'});
    expect(v.nickname, '新名');
    expect(s.nickname, '新名');
  });

  test('服务地址：去掉末尾斜杠与空白；空串回到编译时的默认', () {
    expect(normalizeBase(' https://a.example/api/v1/ '), 'https://a.example/api/v1');
    expect(normalizeBase(''), '');
  });
}
