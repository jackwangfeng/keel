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

// 用 mp-flutter 自己的驱动（冷启动、launch 重试、吞开发者工具的自动化超时）：tools/e2e/drive.js。
// v0.3.0 起 keel 从 pub.dev 拉 flutter_miniprogram，pub 包里不带仓库根目录的 tools/，所以按这个顺序找：
//   1. 环境变量 MP_FLUTTER_REPO 指向的 mp-flutter 仓库克隆（推荐：git clone https://github.com/jackwangfeng/mp-flutter.git）；
//   2. pub 的 git 缓存里留下的 mp-flutter-* 目录（以前用 git 依赖时拉过的），取最新的一份并提示版本可能不一致。
function findDrive() {
  const repo = process.env.MP_FLUTTER_REPO;
  if (repo) {
    const f = path.join(repo, 'tools', 'e2e', 'drive.js');
    if (fs.existsSync(f)) return f;
    throw new Error(`MP_FLUTTER_REPO=${repo} 下没有 tools/e2e/drive.js`);
  }
  const cache = path.join(process.env.HOME, '.pub-cache', 'git');
  const dirs = fs.existsSync(cache) ? fs.readdirSync(cache).filter((x) => x.startsWith('mp-flutter-')) : [];
  const hits = dirs.map((d) => path.join(cache, d, 'tools', 'e2e', 'drive.js')).filter((f) => fs.existsSync(f))
    .sort((a, b) => fs.statSync(b).mtimeMs - fs.statSync(a).mtimeMs);
  if (hits.length) {
    console.warn(`[mp_walk] 用 pub git 缓存里的驱动：${hits[0]}（可能比当前依赖的 flutter_miniprogram 旧；要对齐版本就设 MP_FLUTTER_REPO）`);
    return hits[0];
  }
  throw new Error('找不到 mp-flutter 的 tools/e2e/drive.js：git clone https://github.com/jackwangfeng/mp-flutter.git，再设 MP_FLUTTER_REPO=<克隆目录>');
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
