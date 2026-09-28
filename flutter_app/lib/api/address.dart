import 'client.dart';
import 'geo.dart';
import 'schema.g.dart';

/// 地址簿。照 uni-app x 的 view.uts（addressRow / addressForm / addressRequest / fieldErrors）与 pages/address 移植。

class AddressRow {
  final int id;
  final String name;
  final String phone;
  final String regionText;
  final String detail;
  /// 一行写完：地区 + 街道 + 门牌。结算页与列表的副标题用它。
  final String fullText;
  final bool isDefault;
  final String tagText;
  /// 坐标（WGS-84）。老地址、手填的没有。首页「换地址」按它重新解析门店。
  final LatLng? at;
  const AddressRow({required this.id, required this.name, required this.phone, required this.regionText,
      required this.detail, required this.fullText, required this.isDefault, required this.tagText, this.at});
}

String _tag(int? t) => switch (t) { 1 => '家', 2 => '公司', 3 => '学校', _ => '' };

AddressRow addressRow(Address a) {
  final region = '${a.province} ${a.city} ${a.district}';
  final street = a.street ?? '';
  return AddressRow(
    id: a.id,
    name: a.receiverName,
    phone: a.phone,
    regionText: region,
    detail: a.detail,
    fullText: '$region ${street.isNotEmpty ? '$street ' : ''}${a.detail}',
    isDefault: a.isDefault,
    tagText: _tag(a.tag),
    at: a.lat != null && a.lng != null ? (lat: a.lat!, lng: a.lng!) : null,
  );
}

/// 编辑页的表单。表单上没有的字段（街道、区划码、邮编、标签、坐标）也存着，PUT 时原样带回 ——
/// PUT 是整条替换，不带就被清掉（uni-app x 那版漏了这几项）。
class AddressForm {
  String receiverName = '';
  String phone = '';
  String province = '';
  String city = '';
  String district = '';
  String detail = '';
  /// 服务端当前的默认状态（修改时 PUT 只能带这个值）。
  bool isDefault = false;
  String? street;
  String? regionCode;
  String? postalCode;
  int? tag;
  /// WGS-84，要么都有要么都没有（只给一个服务端回 422）。
  double? lat;
  double? lng;

  AddressForm();

  AddressForm.of(Address a)
      : receiverName = a.receiverName,
        phone = a.phone,
        province = a.province,
        city = a.city,
        district = a.district,
        detail = a.detail,
        isDefault = a.isDefault,
        street = a.street,
        regionCode = a.regionCode,
        postalCode = a.postalCode,
        tag = a.tag,
        lat = a.lat,
        lng = a.lng;

  /// 搜索地点 / 地图选点选中了一处：省市区、街道、区划码（运费按它算）与坐标照填，详细地址先写上地址与地点名，门牌号用户自己补。
  void applyPlace(GeoPlace p) {
    province = p.province;
    city = p.city;
    district = p.district;
    street = p.street.isNotEmpty ? p.street : null;
    regionCode = p.adcode.isNotEmpty ? p.adcode : null;
    detail = [p.address, p.name].where((s) => s.isNotEmpty).join(' ');
    lat = p.lat;
    lng = p.lng;
  }

  AddressInput input(bool isDefault) => AddressInput(
        receiverName: receiverName,
        phone: phone,
        province: province,
        city: city,
        district: district,
        detail: detail,
        street: street,
        regionCode: regionCode,
        postalCode: postalCode,
        tag: tag,
        lat: lat,
        lng: lng,
        isDefault: isDefault,
      );
}

/// 422 的 errors[].field 是契约名 -> 表单字段名 -> 那一格下面写的话。
Map<String, String> formErrors(ApiFailure f) => {
      for (final e in f.fieldErrors)
        switch (e.field ?? '') { 'receiver_name' => 'receiverName', 'is_default' => 'isDefault', final x => x }:
            (e.message ?? '').isNotEmpty ? e.message! : '这一项不正确',
    };

Future<List<Address>> _list(ApiClient c) async => (await c.send('GET', '/addresses',
        decode: (j) => (j as List).map((e) => Address.fromJson(e as Map<String, dynamic>)).toList()))
    .data;

/// 服务端已按默认在前排好。
Future<List<AddressRow>> fetchAddresses(ApiClient c) async => (await _list(c)).map(addressRow).toList();

/// 编辑页用：没有 GET /addresses/{id}，从列表里取。已经不存在是 null。
Future<AddressForm?> fetchAddressForm(ApiClient c, int id) async {
  for (final a in await _list(c)) {
    if (a.id == id) return AddressForm.of(a);
  }
  return null;
}

/// 新建（幂等键由表单持有：超时后再点保存是重放，不会建出两条）。true = 设为默认（服务端同事务清旧默认）。
Future<AddressRow> createAddress(ApiClient c, AddressForm f, {required bool wantDefault, required String idempotencyKey}) async =>
    addressRow((await c.send('POST', '/addresses', body: f.input(wantDefault).toJson(), idempotencyKey: idempotencyKey,
            decode: (j) => Address.fromJson(j as Map<String, dynamic>)))
        .data);

/// 修改：只能带这条地址当前的 is_default（PUT 不负责切默认，要切走 setDefaultAddress）。
Future<AddressRow> updateAddress(ApiClient c, int id, AddressForm f) async =>
    addressRow((await c.send('PUT', '/addresses/$id', body: f.input(f.isDefault).toJson(),
            decode: (j) => Address.fromJson(j as Map<String, dynamic>)))
        .data);

Future<void> setDefaultAddress(ApiClient c, int id) =>
    c.send('PUT', '/addresses/$id/default', decode: (_) => null);

Future<void> deleteAddress(ApiClient c, int id) => c.send('DELETE', '/addresses/$id', decode: (_) => null);
