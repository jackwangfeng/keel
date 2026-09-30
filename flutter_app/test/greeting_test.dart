import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/pages/home_page.dart';

void main() {
  test('首页问候按时段：夜深了 / 早上好 / 上午好 / 中午好 / 下午好 / 晚上好（上午 11 点不再显示「下午好」）', () {
    const want = {
      0: '夜深了', 4: '夜深了',
      5: '早上好', 8: '早上好',
      9: '上午好', 11: '上午好',
      12: '中午好', 13: '中午好',
      14: '下午好', 17: '下午好',
      18: '晚上好', 23: '晚上好',
    };
    for (final e in want.entries) {
      expect(greetingFor(e.key), e.value, reason: '${e.key} 点');
    }
  });
}
