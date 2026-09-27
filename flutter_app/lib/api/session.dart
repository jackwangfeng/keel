import 'package:flutter/foundation.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'schema.g.dart';

/// 本机会话。放在 shared_preferences（小程序里由 mp-flutter 换成 wx.*StorageSync）。
class Session extends ChangeNotifier {
  static const _kAccess = 'keel.access';
  static const _kRefresh = 'keel.refresh';
  static const _kNick = 'keel.nickname';

  String? accessToken;
  String? refreshToken;
  String nickname = '';

  bool get loggedIn => accessToken != null && accessToken!.isNotEmpty;

  Future<void> load() async {
    final p = await SharedPreferences.getInstance();
    accessToken = p.getString(_kAccess);
    refreshToken = p.getString(_kRefresh);
    nickname = p.getString(_kNick) ?? '';
    notifyListeners();
  }

  /// 改了昵称（个人资料页保存后）。
  Future<void> setNickname(String v) async {
    nickname = v;
    await (await SharedPreferences.getInstance()).setString(_kNick, v);
    notifyListeners();
  }

  Future<void> save(LoginResponse r) =>
      saveTokens(access: r.accessToken, refresh: r.refreshToken ?? refreshToken, nickname: r.user.nickname);

  Future<void> saveTokens({required String access, String? refresh, required String nickname}) async {
    accessToken = access;
    refreshToken = refresh;
    this.nickname = nickname;
    final p = await SharedPreferences.getInstance();
    await p.setString(_kAccess, access);
    if (refresh != null) await p.setString(_kRefresh, refresh);
    await p.setString(_kNick, nickname);
    notifyListeners();
  }

  Future<void> clear() async {
    accessToken = null;
    refreshToken = null;
    nickname = '';
    final p = await SharedPreferences.getInstance();
    await p.remove(_kAccess);
    await p.remove(_kRefresh);
    await p.remove(_kNick);
    notifyListeners();
  }
}
