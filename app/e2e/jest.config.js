// 真机自动化测试。用法见 app/e2e/README.md，入口是 `make app-e2e`。
module.exports = {
  rootDir: __dirname,
  testMatch: ['<rootDir>/**/*.test.js'],
  testEnvironment: '@dcloudio/uni-automator/dist/environment.js',
  // 断开与 App 的连接。即便如此 jest 仍不会自己退出：adbkit 的 adb 连接与 ws 服务是
  // 在 uni-automator 的测试环境里建的，用例代码碰不到（--detectOpenHandles 也看不见），
  // 所以 package.json 的脚本带 --forceExit。
  globalTeardown: '@dcloudio/uni-automator/dist/teardown.js',
  testTimeout: 60000,
  // 一台手机只有一个 App 实例：用例必须串行（package.json 的脚本带 -i）。
  maxWorkers: 1,
}
