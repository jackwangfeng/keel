import 'package:flutter/foundation.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:geolocator/geolocator.dart';
import 'package:keel_buyer/api/locate.dart';

void main() {
  test('Android 绕开 Google 融合定位、直接走系统 LocationManager（国内机器上融合定位挂着不回）', () {
    final s = locationSettingsFor(TargetPlatform.android);
    expect(s, isA<AndroidSettings>());
    expect((s as AndroidSettings).forceLocationManager, isTrue);
    expect(s.accuracy, LocationAccuracy.medium);
  });

  test('其它平台照旧', () {
    final s = locationSettingsFor(TargetPlatform.iOS);
    expect(s, isNot(isA<AndroidSettings>()));
    expect(s.accuracy, LocationAccuracy.medium);
  });
}
