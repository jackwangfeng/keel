import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/config.dart';

void main() {
  test('编译期注入了就用注入的，去掉末尾斜杠', () {
    expect(resolveApiBase(defined: 'http://x:1/api/v1/', isWeb: false, base: Uri.parse('file:///')),
        'http://x:1/api/v1');
  });
  test('Web 没注入：同源相对路径', () {
    expect(resolveApiBase(defined: '', isWeb: true, base: Uri.parse('http://localhost:5000/')), '/api/v1');
  });
  test('小程序（Uri.base 是 https://mp.local/）没注入：报清楚的错，不打到 mp.local', () {
    expect(() => resolveApiBase(defined: '', isWeb: true, base: Uri.parse('https://mp.local/')),
        throwsA(isA<StateError>().having((e) => e.message, 'message', contains('KEEL_API_BASE'))));
  });
  test('原生没注入：报错', () {
    expect(() => resolveApiBase(defined: '', isWeb: false, base: Uri.parse('file:///')), throwsStateError);
  });
}
