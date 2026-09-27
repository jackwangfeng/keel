import 'package:integration_test/integration_test.dart';

import 'browse.dart';
import 'smoke.dart';

/// 一个入口跑全部：flutter drive 每个 target 都要重新编一次 App，分文件跑慢得多。
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  smokeTests();
  browseTests();
}
