'use strict';
// 小程序冒烟：用微信开发者工具打开 build/weapp，截 App 自己的画面（mp.screenshot，不截屏幕），
// 按底部 tab 的位置点一遍，收集 console 报错。用前先跟 mp-flutter 会话打招呼（开发者工具是共用的）。
//   node tool/mp_walk.js [产物目录] [截图目录]
const path = require('path');
const fs = require('fs');

const dir = path.resolve(process.argv[2] || path.join(__dirname, '..', 'build', 'weapp'));
const shots = path.resolve(process.argv[3] || path.join(__dirname, '..', 'build', 'mp-shots'));
fs.mkdirSync(shots, { recursive: true });
const T = (x, y) => ({ identifier: 0, x, y, pageX: x, pageY: y, clientX: x, clientY: y });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 用 mp-flutter 自己的驱动（冷启动、launch 重试、吞开发者工具的自动化超时）：它就在 pub 缓存里的依赖源码中。
function findDrive() {
  const cache = path.join(process.env.HOME, '.pub-cache', 'git');
  for (const d of fs.readdirSync(cache).filter((x) => x.startsWith('mp-flutter-'))) {
    const f = path.join(cache, d, 'tools', 'e2e', 'drive.js');
    if (fs.existsSync(f)) return f;
  }
  throw new Error('pub 缓存里没有 mp-flutter 的 tools/e2e/drive.js（先 make flutter-get）');
}
const { runE2E } = require(findDrive());

runE2E({
  projectPath: dir,
  bootMs: 25000,
  settleMs: 3000,
  async interact(mp, page, log) {
    const shot = async (name) => {
      try {
        await Promise.race([mp.screenshot({ path: path.join(shots, name) }), sleep(20000).then(() => { throw new Error('screenshot 超时'); })]);
        log('[shot] ' + name);
      } catch (e) {
        log('[shot] ' + name + ' 失败：' + e.message);
      }
    };
    await shot('1-home.png');
    const canvas = await page.$('#flutter-canvas');
    const size = await canvas.size();
    log('[size] ' + JSON.stringify(size));
    // 首页往下拖一屏（回到顶部），看顶部的问候、搜索框、分类是否在。
    const dx = size.width / 2;
    await canvas.touchstart({ touches: [T(dx, 200)], changedTouches: [T(dx, 200)] });
    for (let k = 1; k <= 12; k++) await canvas.touchmove({ touches: [T(dx, 200 + k * 50)], changedTouches: [T(dx, 200 + k * 50)] });
    await canvas.touchend({ touches: [], changedTouches: [T(dx, 800)] });
    await sleep(3000);
    await shot('0-home-top.png');
    const names = ['1-home', '2-cart', '3-orders', '4-me'];
    // 首页的搜索框（390 宽时大约在 y=200）：进搜索页，看顶栏右侧的「搜索」有没有被胶囊盖住。
    const tapAt = async (x, y) => {
      await canvas.touchstart({ touches: [T(x, y)], changedTouches: [T(x, y)] });
      await canvas.touchend({ touches: [], changedTouches: [T(x, y)] });
      await sleep(4000);
    };
    await tapAt(size.width / 2, 200);
    await shot('5-search.png');
    for (const i of [1, 2, 3, 0]) {
      const x = size.width * (i + 0.5) / 4;
      const y = size.height - 40;
      await canvas.touchstart({ touches: [T(x, y)], changedTouches: [T(x, y)] });
      await canvas.touchend({ touches: [], changedTouches: [T(x, y)] });
      await sleep(5000);
      await shot(names[i] + (i === 0 ? '-again' : '') + '.png');
    }
  },
}).then((r) => {
  const lines = (r && r.lines) || [];
  fs.writeFileSync(path.join(shots, 'console.txt'), lines.join('\n'));
  const bad = lines.filter((l) => /^\[(error|EXCEPTION)\]/.test(l));
  console.log('行数 ' + lines.length + '，error/exception ' + bad.length);
  lines.filter((l) => /^\[(shot|size|AUTOMATOR)\]/.test(l)).forEach((l) => console.log(l));
  bad.slice(0, 20).forEach((l) => console.log(l.slice(0, 300)));
}).catch((e) => { console.error('mp_walk 失败：', e && e.message); process.exit(1); });
