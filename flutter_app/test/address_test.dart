import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/address.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/schema.g.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:shared_preferences/shared_preferences.dart';

Map<String, dynamic> addr({int id = 5, bool def = false, String? street, String? region, int? tag}) => {
      'id': id, 'receiver_name': '张三', 'phone': '13900000000', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区',
      'street': ?street, 'detail': '文三路 1 号', 'region_code': ?region, 'tag': ?tag, 'is_default': def,
    };

Future<(ApiClient, List<http.Request>)> client(http.Response Function(http.Request) h) async {
  SharedPreferences.setMockInitialValues({});
  final s = Session();
  await s.load();
  final seen = <http.Request>[];
  return (ApiClient(base: 'http://h/api/v1', session: s, http: MockClient((r) async {
    seen.add(r);
    return h(r);
  })), seen);
}

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

void main() {
  test('行：地区 + 街道 + 门牌拼一行；标签', () {
    final r = addressRow(Address.fromJson(addr(street: '古荡街道', tag: 2, def: true)));
    expect(r.fullText, '浙江省 杭州市 西湖区 古荡街道 文三路 1 号');
    expect(r.tagText, '公司');
    expect(r.isDefault, isTrue);
  });

  test('修改：PUT 带这条地址当前的 is_default，并原样带回表单里没有的字段（街道、区划码、标签）', () async {
    final (c, seen) = await client((r) => j(addr(region: '330106')));
    final f = AddressForm.of(Address.fromJson(addr(street: '古荡街道', region: '330106', tag: 1)));
    f.detail = '文三路 2 号';
    await updateAddress(c, 5, f);
    final b = jsonDecode(seen.single.body) as Map;
    expect(seen.single.method, 'PUT');
    expect(b['is_default'], false);
    expect(b['detail'], '文三路 2 号');
    expect(b['street'], '古荡街道');
    expect(b['region_code'], '330106');
    expect(b['tag'], 1);
  });

  test('新建：带幂等键；想要默认就 is_default: true', () async {
    final (c, seen) = await client((r) => j(addr(def: true), 201));
    final f = AddressForm()
      ..receiverName = '李四'
      ..phone = '13900000001'
      ..province = '上海市'
      ..city = '上海市'
      ..district = '徐汇区'
      ..detail = '1 号';
    await createAddress(c, f, wantDefault: true, idempotencyKey: 'k1');
    expect(seen.single.headers['Idempotency-Key'], 'k1');
    expect((jsonDecode(seen.single.body) as Map)['is_default'], true);
  });

  test('422 errors[].field 是契约名：映射回表单字段', () async {
    final (c, _) = await client((r) => j({'type': 'https://keel.dev/problems/invalid-request', 'title': '参数不合法', 'status': 422,
      'errors': [{'field': 'receiver_name', 'message': '不能为空'}, {'field': 'phone', 'message': '手机号格式不对'}]}, 422));
    try {
      await createAddress(c, AddressForm(), wantDefault: false, idempotencyKey: 'k');
      fail('应当失败');
    } on ApiFailure catch (f) {
      final errs = formErrors(f);
      expect(errs, {'receiverName': '不能为空', 'phone': '手机号格式不对'});
    }
  });
}
