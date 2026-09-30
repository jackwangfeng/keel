import 'package:shared_preferences/shared_preferences.dart';

import 'client.dart';
import 'schema.g.dart';
import 'session.dart';
import 'store.dart';
import 'view.dart';

/// 个人资料与服务地址。最初从 uni-app x 版（view.uts 的 meView / identityRow 与 pages/me/profile、pages/settings/base，见 tag uniapp-final）移植。

class MeView {
  final String nickname;
  /// 脱敏的手机号；没绑是空串。
  final String phone;
  final int gender;
  final String genderText;
  final bool hasPassword;
  const MeView({required this.nickname, required this.phone, required this.gender, required this.genderText, required this.hasPassword});
}

String genderText(int g) => switch (g) { 1 => '男', 2 => '女', _ => '未设置' };

MeView meView(User u) => MeView(
      nickname: u.nickname,
      phone: u.phone ?? '',
      gender: u.gender ?? 0,
      genderText: genderText(u.gender ?? 0),
      hasPassword: u.hasPassword == true,
    );

class IdentityRow {
  final int provider;
  final String name;
  final String boundAt;
  const IdentityRow(this.provider, this.name, this.boundAt);
}

String identityName(int p) => switch (p) {
      1 => '微信小程序',
      2 => '微信公众号',
      3 => '微信开放平台',
      4 => '支付宝',
      5 => 'Apple',
      _ => '第三方账号 #$p',
    };

Future<MeView> fetchMe(ApiClient c) async =>
    meView((await c.send('GET', '/me', decode: (j) => User.fromJson(j as Map<String, dynamic>))).data);

/// 只带改了的字段（契约 minProperties: 1）。「我的」页读的是本机会话里的昵称：保存后跟着改。
Future<MeView> updateMe(ApiClient c, Session s, {required String? nickname, required int? gender}) async {
  final res = await c.send('PATCH', '/me', body: UpdateMeRequest(nickname: nickname, gender: gender).toJson(),
      decode: (j) => User.fromJson(j as Map<String, dynamic>));
  final v = meView(res.data);
  await s.setNickname(v.nickname);
  return v;
}

Future<List<IdentityRow>> fetchIdentities(ApiClient c) async => (await c.send('GET', '/me/identities',
        decode: (j) => (j as List).map((e) => UserIdentity.fromJson(e as Map<String, dynamic>)).toList()))
    .data
    .map((i) => IdentityRow(i.provider, identityName(i.provider), shortTime(i.createdAt)))
    .toList();

/// 解绑。最后一个能登录的方式（last-credential）翻成人话。
Future<void> unbindIdentity(ApiClient c, int provider) async {
  try {
    await c.send('DELETE', '/me/identities/$provider', decode: (_) => null);
  } on ApiFailure catch (f) {
    if (f.isType('last-credential')) throw f.withMessage('这是账号最后一个能登录的方式，先设置登录密码再解绑');
    rethrow;
  }
}

// ---- 服务地址（= 租户） ----

const _kBase = 'keel.base';

String normalizeBase(String v) {
  var s = v.trim();
  while (s.endsWith('/')) {
    s = s.substring(0, s.length - 1);
  }
  return s;
}

/// 本机设置过的服务地址；没设过是 null（用编译时注入的）。
Future<String?> savedBase() async => (await SharedPreferences.getInstance()).getString(_kBase);

/// 换服务地址。换了就是换了一家店（租户）：上一家的登录与门店都作废。返回是否退出了登录。
/// value 为空串 = 回到编译时的默认 [fallback]。
Future<bool> switchBase(ApiClient c, Session s, StoreService store, String value, {required String fallback}) async {
  final next = normalizeBase(value).isEmpty ? fallback : normalizeBase(value);
  final p = await SharedPreferences.getInstance();
  if (normalizeBase(value).isEmpty) {
    await p.remove(_kBase);
  } else {
    await p.setString(_kBase, next);
  }
  if (next == c.base) return false;
  c.base = next;
  store.reset();
  final was = s.loggedIn;
  await s.clear();
  return was;
}
