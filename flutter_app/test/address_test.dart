import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/address.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/schema.g.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:shared_preferences/shared_preferences.dart';

Map<String, dynamic> addr({int id = 5, bool def = false, String? street, String? region, int? tag, double? lat, double? lng}) => {
      'id': id, 'receiver_name': '张三', 'phone': '13900000000', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区',
      'street': ?street, 'detail': '文三路 1 号', 'region_code': ?region, 'tag': ?tag, 'is_default': def, 'lat': ?lat, 'lng': ?lng,
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

  test('坐标（POI）：PUT 整条替换，表单原样带回，不然一保存就把坐标清掉；行上也带着（首页换地址用）', () async {
    final (c, seen) = await client((r) => j(addr(lat: 30.27, lng: 120.15)));
    final a = Address.fromJson(addr(lat: 30.27, lng: 120.15));
    await updateAddress(c, 5, AddressForm.of(a));
    final b = jsonDecode(seen.single.body) as Map;
    expect((b['lat'], b['lng']), (30.27, 120.15));
    expect(addressRow(a).at, (lat: 30.27, lng: 120.15));
    expect(addressRow(Address.fromJson(addr())).at, isNull);
  });

  test('选中一个地点：自动填省市区、街道、区划码、坐标与详细地址', () {
    final f = AddressForm()..applyPlace(const GeoPlace(name: '黄龙时代广场', address: '杭大路 15 号', province: '浙江省', city: '杭州市',
        district: '西湖区', adcode: '330106', street: '北山街道', lat: 30.27, lng: 120.15));
    expect([f.province, f.city, f.district, f.street, f.regionCode], ['浙江省', '杭州市', '西湖区', '北山街道', '330106']);
    expect(f.detail, '杭大路 15 号 黄龙时代广场');
    expect((f.lat, f.lng), (30.27, 120.15));
    final b = f.input(false).toJson();
    expect((b['lat'], b['lng']), (30.27, 120.15));
  });

  test('详细地址：高德的 address 常带着省市区、也已含地点名 —— 去掉省市区前缀，已含地点名就不再拼', () {
    GeoPlace g(String name, String address, {String province = '浙江省', String city = '杭州市', String district = '拱墅区'}) =>
        GeoPlace(name: name, address: address, province: province, city: city, district: district, adcode: '330105', street: '',
            lat: 30.27, lng: 120.15);
    String detail(GeoPlace p) => (AddressForm()..applyPlace(p)).detail;
    expect(detail(g('天巢花苑', '浙江省杭州市拱墅区天水街道天巢花苑')), '天水街道天巢花苑');
    expect(detail(g('黄龙时代广场', '杭大路 15 号')), '杭大路 15 号 黄龙时代广场');
    expect(detail(g('人民广场', '上海市黄浦区人民大道', province: '上海市', city: '上海市', district: '黄浦区')), '人民大道 人民广场');
    expect(detail(g('某地', '')), '某地');
  });
}
