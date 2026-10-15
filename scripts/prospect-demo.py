#!/usr/bin/env python3
"""按客户的商品清单，在多商家形态的栈上开一家「像他自己店」的演示店。

见客户之前跑一次：拿他的商品清单（导入模板格式的 xlsx / csv，也就是后台「批量导入」认的那份），
开店 → 按清单建类目 → 批量导入 → 上架 → 配图 → 补 N 天模拟经营（下单、付款、搜索）→ 发 AI 员工密钥。
跑完 AI 员工就有东西可管：卖得快的货见底（补货 / 渠道分配提案）、几乎没人买的货压着
（滞销清仓提案）、顾客搜了店里没有的东西（搜索缺口）。最后打印一页「能讲的数字」。

  prospect-demo.py create  --catalog 清单.csv --code xinhui --name "鑫惠超市" [--days 14]
  prospect-demo.py simulate --code xinhui [--orders 5]     现在再下几单（演示当天让数字动起来）
  prospect-demo.py report   --code xinhui                  只重新打印那一页数字

默认打本机的多商家栈（`make multi-up`，项目名 keelmulti）；换栈用 --project 或下面的环境变量。
**不碰单商家演示站**：演示站是单商家形态，开不了第二家店。

清单里可以多一列「演示热度」（热 / 平 / 冷），决定模拟经营里谁好卖、谁滞销；导入会把这列当
不认识的列忽略（预检有提示，不影响导入）。没有这一列（或 xlsx）时按 --seed 随机分档。
只有下单与付款走真实接口（SAGA、沙箱支付）；把一批单挪到过去某一天、建模拟买家是直接改库，
理由与 ~/.local/share/keel-eshop/bin/simulate.py 相同 —— 接口没有「补历史」这件事。

凭据：平台会话读 multi-verify 缓存的那份（tmp/multi-verify-platform-token.<项目名>）；
新店管理员的一次性登录 token 打到 stdout（与 issue-login 同一个约定，不写日志、不落盘）；
AI 员工密钥写 ~/.config/keel/prospect-<code>-agent-key（0600），只打印路径。
"""
import argparse, csv, io, json, os, random, secrets, subprocess, sys, time, urllib.error, urllib.request, uuid

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STATE_DIR = os.path.join(ROOT, "tmp")
CONF_DIR = os.path.expanduser("~/.config/keel")
# 店里没有、顾客却会搜的词：搜索缺口手册要用到。挑超市常见、清单里大概率没有的。
MISSING_TERMS = ["螺蛳粉", "自热火锅", "燕麦奶", "冻干咖啡", "宠物粮", "电池", "猫砂", "榴莲"]
HEAT_WEIGHT = {"热": 10.0, "平": 3.0, "冷": 0.08}
PALETTE = [(0xE8, 0xF5, 0xE9), (0xFF, 0xF3, 0xE0), (0xE3, 0xF2, 0xFD), (0xFC, 0xE4, 0xEC),
           (0xF3, 0xE5, 0xF5), (0xFF, 0xFD, 0xE7), (0xE0, 0xF7, 0xFA), (0xEF, 0xEB, 0xE9)]
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # 本机 curl 走代理，这里显式不走


def die(msg):
    print("✗ " + msg, file=sys.stderr)
    sys.exit(1)


class Api:
    def __init__(self, base, host):
        self.base, self.host = base.rstrip("/"), host

    def call(self, method, path, body=None, token=None, idem=False, files=None, fields=None, ok=(200, 201, 204)):
        headers = {"Host": self.host}
        data = None
        if files is not None:
            boundary = "----keel" + secrets.token_hex(8)
            buf = io.BytesIO()
            for name, (fname, content, ctype) in files.items():
                buf.write(("--%s\r\nContent-Disposition: form-data; name=\"%s\"; filename=\"%s\"\r\n"
                           "Content-Type: %s\r\n\r\n" % (boundary, name, fname, ctype)).encode())
                buf.write(content + b"\r\n")
            for name, (value, ctype) in (fields or {}).items():
                buf.write(("--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\nContent-Type: %s\r\n\r\n"
                           % (boundary, name, ctype)).encode())
                buf.write(value.encode() + b"\r\n")
            buf.write(("--%s--\r\n" % boundary).encode())
            data = buf.getvalue()
            headers["Content-Type"] = "multipart/form-data; boundary=" + boundary
        elif body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if token:
            headers["Authorization"] = "Bearer " + token
        if idem or method in ("POST", "PUT", "PATCH", "DELETE"):  # 写接口多数要求它，一律带上
            headers["Idempotency-Key"] = str(uuid.uuid4())
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=headers)
        try:
            with opener.open(req, timeout=120) as r:
                st, raw = r.status, r.read()
        except urllib.error.HTTPError as e:
            st, raw = e.code, e.read()
        try:
            out = json.loads(raw) if raw else None
        except ValueError:
            out = raw.decode(errors="replace")
        if ok and st not in ok:
            die("%s %s → %s %s" % (method, path, st, json.dumps(out, ensure_ascii=False)[:400]))
        return st, out


class Db:
    """直连 postgres 容器（docker exec psql）。单体形态 core 与库存是同一个库。"""

    def __init__(self, core, core_db, inv, inv_db):
        self.t = {"core": (core, core_db), "inv": (inv, inv_db)}

    def q(self, which, sql, **vars):
        c, d = self.t[which]
        cmd = ["docker", "exec", "-i", c, "psql", "-U", "keel", "-d", d, "-v", "ON_ERROR_STOP=1", "-Atq", "-F", "\t"]
        for k, v in vars.items():
            cmd += ["-v", "%s=%s" % (k, v)]
        p = subprocess.run(cmd, input=sql, capture_output=True, text=True)
        if p.returncode != 0:
            die("psql(%s): %s" % (which, p.stderr.strip()[:400]))
        return p.stdout.strip()


# ---------- 公共：配置与状态 ----------

def setup(a):
    proj = a.project
    api = Api(a.api or "http://127.0.0.1:%s/api/v1" % os.environ.get("KEEL_HTTP_PORT", "28185"),
              "%s.%s" % (a.code, a.base_domain))
    core = a.core_pg or proj + "-postgres-1"
    db = Db(core, a.core_db, a.inv_pg or core, a.inv_db or a.core_db)
    return api, db


def state_path(a):
    return os.path.join(STATE_DIR, "prospect-%s.%s.json" % (a.project, a.code))


def load_state(a):
    try:
        return json.load(open(state_path(a)))
    except FileNotFoundError:
        die("没有 %s 的状态文件（%s）：先跑 create" % (a.code, state_path(a)))


def save_state(a, st):
    os.makedirs(STATE_DIR, exist_ok=True)
    json.dump(st, open(state_path(a), "w"), ensure_ascii=False, indent=1)


def heat_from_csv(path):
    """读 CSV 的「演示热度」列：{sku_code: 热/平/冷}。xlsx 或没有这列 → {}。"""
    raw = open(path, "rb").read()
    if raw[:2] == b"PK":
        return {}
    try:
        text = raw.decode("utf-8-sig")
    except UnicodeDecodeError:
        text = raw.decode("gb18030")
    out = {}
    for row in csv.DictReader(io.StringIO(text)):
        row = {(k or "").strip(): (v or "").strip() for k, v in row.items()}
        code, heat = row.get("SKU 编码") or row.get("SKU编码"), row.get("演示热度")
        if code and heat in HEAT_WEIGHT:
            out[code] = heat
    return out


# ---------- create ----------

def ensure_categories(api, tok, paths):
    """按「一级>二级」路径建类目（已有的复用），返回 {原文路径: 末级 id}。"""
    st, existing = api.call("GET", "/admin/categories", token=tok)
    items = existing if isinstance(existing, list) else existing.get("items", [])
    by_key = {(c.get("parent_id"), c["name"]): c["id"] for c in items}
    out = {}
    for raw in sorted(paths):
        parent = None
        for name in [p.strip() for p in raw.replace("/", ">").split(">") if p.strip()]:
            key = (parent, name)
            if key not in by_key:
                body = {"name": name}
                if parent:
                    body["parent_id"] = parent
                _, c = api.call("POST", "/admin/categories", body, token=tok, idem=True)
                by_key[key] = c["id"]
            parent = by_key[key]
        out[raw] = parent
    return out


def make_image(title, idx):
    """没有商品图时画一张文字图：浅底色 + 商品名。比空白的灰框像样，也一眼看得出是示意图。"""
    try:
        from PIL import Image, ImageDraw, ImageFont
    except ImportError:
        return None
    font_path = next((p for p in ["/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc",
                                  "/usr/share/fonts/opentype/noto/NotoSerifCJK-Bold.ttc",
                                  "/System/Library/Fonts/PingFang.ttc"] if os.path.exists(p)), None)
    if not font_path:
        return None
    W = 800
    img = Image.new("RGB", (W, W), PALETTE[idx % len(PALETTE)])
    d = ImageDraw.Draw(img)
    text = title if len(title) <= 6 else title[:6] + "\n" + title[6:12]
    size = 120 if len(title) <= 4 else 96
    f = ImageFont.truetype(font_path, size)
    box = d.multiline_textbbox((0, 0), text, font=f, align="center", spacing=24)
    d.multiline_text(((W - (box[2] - box[0])) / 2, (W - (box[3] - box[1])) / 2 - 30), text,
                     font=f, fill=(0x42, 0x42, 0x42), align="center", spacing=24)
    small = ImageFont.truetype(font_path, 32)
    d.text((W / 2, W - 70), "示意图", font=small, fill=(0x9E, 0x9E, 0x9E), anchor="mm")
    buf = io.BytesIO()
    img.save(buf, "JPEG", quality=85)
    return buf.getvalue()


SIM_BUYERS_SQL = """
INSERT INTO users (merchant_id, phone, password_hash, nickname, status)
SELECT :mid, '139000001' || lpad(g::text, 2, '0'),
       (SELECT password_hash FROM users WHERE phone = :'ref' AND password_hash IS NOT NULL ORDER BY id LIMIT 1),
       '模拟顾客 ' || g, 1
  FROM generate_series(1, :n) g
 WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.merchant_id = :mid AND u.phone = '139000001' || lpad(g::text, 2, '0'));
INSERT INTO user_addresses (merchant_id, user_id, receiver_name, phone, province, city, district, street, detail, region_code, is_default)
SELECT u.merchant_id, u.id, '模拟收件人', u.phone, '浙江省', '杭州市', '西湖区', '文三路', '模拟 ' || right(u.phone, 2) || ' 号', '330106', TRUE
  FROM users u
 WHERE u.merchant_id = :mid AND u.phone LIKE '139000001__'
   AND NOT EXISTS (SELECT 1 FROM user_addresses a WHERE a.user_id = u.id);
SELECT count(*) FROM users WHERE merchant_id = :mid AND phone LIKE '139000001__';
"""


def create(a):
    api, db = setup(a)
    if os.path.exists(state_path(a)) and not a.force:
        die("%s 已经建过（%s）。换个 --code，或加 --force 覆盖状态文件（店本身不会被删）" % (a.code, state_path(a)))
    plat_file = a.platform_token_file or os.path.join(STATE_DIR, "multi-verify-platform-token." + a.project)
    if not os.path.exists(plat_file):
        die("没有平台会话缓存 %s：先跑一次 make multi-verify（它会用引导 token 换一份并缓存）" % plat_file)
    plat = open(plat_file).read().strip()
    catalog = open(a.catalog, "rb").read()
    fname = os.path.basename(a.catalog)
    ctype = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" if catalog[:2] == b"PK" else "text/csv"
    rnd = random.Random(a.seed)

    print("① 开店 %s（%s）" % (a.name, a.code))
    # 多商家形态下每个请求都先按 Host 认店，连平台级的开店也一样 —— 拿一家已有的店当入口。
    plat_api = Api(api.base, a.platform_host or "shop-a." + a.base_domain)
    _, m = plat_api.call("POST", "/admin/merchants",
                    {"code": a.code, "name": a.name, "admin_email": "admin@%s.keel.invalid" % a.code},
                    token=plat, idem=True)
    mid, admin_staff = m["id"], m.get("admin_staff_id")
    # 再登记一个解析到本机的域名（nip.io：<任意>.127.0.0.1.nip.io → 127.0.0.1）。多商家形态按 Host 认店，
    # 而 AI 员工的 MCP 客户端（Node fetch）会丢掉自定义的 Host 头 —— 只有一个真能解析的域名它才进得了这家店。
    # 浏览器里打开后台 / 买家端也靠它。
    domain = a.domain or "%s.127.0.0.1.nip.io" % a.code
    plat_api.call("PATCH", "/admin/merchants/%d" % m["id"], {"domain": domain}, token=plat)
    _, sess = api.call("POST", "/admin/auth/session", {"token": m["admin_login_token"]})
    tok = sess["token"]

    print("② 门店")
    _, stores = api.call("GET", "/admin/stores", token=tok)
    stores = stores if isinstance(stores, list) else stores.get("items", [])
    if not stores:
        _, regions = api.call("GET", "/admin/regions", token=tok)
        regions = regions if isinstance(regions, list) else regions.get("items", [])
        if regions:
            region_id = regions[0]["id"]
        else:  # 新开的店什么都没有：区域 → 门店 → 设为默认（导入的库存只写默认门店）
            _, r = api.call("POST", "/admin/regions", {"code": "local", "name": "本地"}, token=tok, idem=True)
            region_id = r["id"]
        prov, city, dist, addr = (a.store_address.split(",") + ["", "", "", ""])[:4]
        _, s = api.call("POST", "/admin/stores", {
            "region_id": region_id, "code": "main", "name": a.name + "（总店）", "is_default": True,
            "province": prov, "city": city, "district": dist, "address": addr, "lat": a.lat, "lng": a.lng},
            token=tok, idem=True)
        stores = [s]
    store_id = stores[0]["id"]

    print("③ 预检清单（%s）" % fname)
    files = {"file": (fname, catalog, ctype)}
    _, pv = api.call("POST", "/admin/product-imports/preview", token=tok, files=files)
    bad = [p for p in pv["products"] if not p["importable"]]
    if bad:
        print("  有 %d 件商品有错、不会导入：%s" % (len(bad), "、".join(p["title"] for p in bad[:8])))
    for n in pv.get("notices", []):
        print("  提示：" + n)
    raw_cat = {r["first_row"]: (r.get("category") or "").strip() for r in pv["rows"] if r.get("category")}
    need = {raw_cat[p["first_row"]] for p in pv["products"]
            if p["importable"] and p["category"]["status"] != "matched" and raw_cat.get(p["first_row"])}
    if need:
        ensure_categories(api, tok, need)
        print("  按清单建了 %d 条类目路径" % len(need))
        _, pv = api.call("POST", "/admin/product-imports/preview", token=tok, files=files)

    print("④ 导入（类目引擎：%s）" % pv["category_engine"])
    choices, fallback = [], None
    for p in pv["products"]:
        if not p["importable"]:
            continue
        c = p["category"]
        if c["status"] == "matched":
            continue
        if c["status"] == "recommended" or (c["status"] == "needs_review" and c.get("candidates")):
            cid = c.get("category_id") or c["candidates"][0]["category_id"]
        else:
            if fallback is None:
                fallback = ensure_categories(api, tok, {"未分类"})["未分类"]
            cid = fallback
        choices.append({"first_row": p["first_row"], "category_id": cid})
    _, imp = api.call("POST", "/admin/product-imports", token=tok, idem=True, files=files,
                      fields={"categories": (json.dumps(choices), "application/json")})
    created = [p for p in imp["products"] if p.get("status") != "failed" and p.get("product_id")]
    print("  导入 %d 件商品" % len(created))

    print("⑤ 上架%s" % ("" if a.no_images else " + 配示意图"))
    for i, p in enumerate(created):
        pid = p["product_id"]
        if not a.no_images:
            jpg = make_image(p["title"], i)
            if jpg:
                _, up = api.call("POST", "/admin/uploads", token=tok, idem=True,
                                 files={"file": ("p%d.jpg" % pid, jpg, "image/jpeg")})
                api.call("PUT", "/admin/products/%d/images" % pid, {"images": [{"upload_id": up["id"]}]}, token=tok)
        api.call("POST", "/admin/products/%d/publication" % pid, {"action": "publish"}, token=tok)

    print("⑥ 模拟顾客 + 沙箱支付密钥")
    # 沙箱支付要「本店该渠道的回调密钥」才造得出可回推的回调（种子只给 shop-a / shop-b 配了）。
    # 随机一把，只给这家演示店的 wechat 渠道用；不是真实渠道的密钥。
    db.q("core", """INSERT INTO shop_settings (merchant_id, extra)
                    VALUES (:mid, jsonb_build_object('payment_channels', jsonb_build_object('wechat',
                            jsonb_build_object('notify_secret', :'sec'))))
                    ON CONFLICT (merchant_id) DO UPDATE SET extra = jsonb_set(shop_settings.extra, '{payment_channels}',
                      coalesce(shop_settings.extra->'payment_channels', '{}') || (EXCLUDED.extra->'payment_channels'), true),
                      updated_at = now()""",
         mid=mid, sec="prospect-" + secrets.token_hex(16))
    n = db.q("core", SIM_BUYERS_SQL, mid=mid, ref=a.ref_buyer, n=a.buyers)
    if not n or int(n.splitlines()[-1]) == 0:
        die("没建出模拟顾客：参考买家 %s 不存在或没有口令（--ref-buyer 换一个）" % a.ref_buyer)

    print("⑦ 发 AI 员工密钥")
    _, ag = api.call("POST", "/admin/agents", {"name": "AI 店长", "role": 2}, token=tok)
    _, key = api.call("POST", "/admin/agents/%d/keys" % ag["id"], {"name": "prospect-demo"}, token=tok, idem=True)
    os.makedirs(CONF_DIR, mode=0o700, exist_ok=True)
    key_file = os.path.join(CONF_DIR, "prospect-%s-agent-key" % a.code)
    with os.fdopen(os.open(key_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as f:
        f.write(key["secret"])

    searchable = wait_searchable(db, mid)
    if not searchable:
        print("  ⚠ 商品的检索数据没有生成（栈上没配推理引擎 KEEL_EMBED_ENDPOINT，或 infero 没在跑），"
              "新店搜不到任何商品 —— 这次不模拟搜索，免得「搜了没有」的数字全是假的")
    heat = heat_from_csv(a.catalog)
    st = {"merchant_id": mid, "code": a.code, "name": a.name, "store_id": store_id, "admin_staff_id": admin_staff,
          "agent_staff_id": ag["id"], "domain": domain, "port": api.base.split(":")[2].split("/")[0], "heat": heat, "searchable": searchable, "seed": a.seed, "created_at": time.strftime("%F %T")}
    save_state(a, st)

    print("⑧ 预警线 + 把商品挪到 %d 天前上架" % (a.days + 1))
    set_warnings(api, tok, st, rnd)
    backdate_catalog(db, st, a.days)

    print("⑨ 补 %d 天经营" % a.days)
    backfill(a, api, db, st, rnd)

    _, lt = api.call("POST", "/admin/staff/%d/login-token" % admin_staff, {}, token=tok) if admin_staff else (0, {})
    report(a, api, db, st)
    print("\n后台登录（一次性，只显示这一次）：%s" % (lt or {}).get("token", "（重签失败，用 make multi-login STAFF=%s）" % admin_staff))
    print("AI 员工密钥：%s" % key_file)
    base = "http://%s:%s" % (domain, st["port"])
    print("买家端 API：%s/api/v1/products    后台 API 同域（后台页面：make multi-up 输出里的 console 端口）" % base)


def wait_searchable(db, mid, timeout=90):
    """检索派生数据（search_text）由异步索引任务写；有引擎时导入后几十秒内就会出现。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if db.q("core", "SELECT count(*) FROM products WHERE merchant_id = :mid AND search_text IS NOT NULL", mid=mid) != "0":
            return True
        time.sleep(5)
    return False


# ---------- 模拟经营 ----------

def buyers(db, st):
    return db.q("core", "SELECT phone FROM users WHERE merchant_id = :mid AND phone LIKE '139000001__' ORDER BY phone",
                mid=st["merchant_id"]).split()


def login(api, phone, pw):
    _, r = api.call("POST", "/auth/login", {"phone": phone, "password": pw})
    return r["access_token"]


def catalog(api, st, rnd):
    """[(sku_id, sku_code, 可售, 权重)]。热度：清单里给了用清单的，否则按 seed 稳定分档。"""
    _, r = api.call("GET", "/products?page_size=100&store_id=%d" % st["store_id"])
    out = []
    for p in r["items"]:
        _, d = api.call("GET", "/products/%d?store_id=%d" % (p["id"], st["store_id"]))
        for s in d.get("skus", []):
            h = st["heat"].get(s.get("sku_code") or s.get("code"))
            if h is None:
                h = random.Random("%s/%s" % (st["seed"], s["id"])).choices(["热", "平", "冷"], [2, 5, 3])[0]
            out.append((s["id"], s.get("available_qty", 0), HEAT_WEIGHT[h], d.get("title") or p.get("title", "")))
    return out


def place(api, tok, st, skus, rnd):
    _, addrs = api.call("GET", "/addresses", token=tok)
    items = addrs.get("items", addrs) if isinstance(addrs, dict) else addrs
    if not items:
        return None
    pool = [s for s in skus if s[1] >= 3]
    if not pool:
        return None
    k = rnd.choice([1, 1, 2, 2, 3])
    lines, seen = [], set()
    for _ in range(k):
        s = rnd.choices(pool, [x[2] for x in pool])[0]
        if s[0] in seen:
            continue
        seen.add(s[0])
        lines.append({"sku_id": s[0], "quantity": min(s[1] - 1, rnd.choice([1, 1, 1, 2, 2, 3]))})
    st_code, o = api.call("POST", "/orders", {"items": lines, "address_id": items[0]["id"], "store_id": st["store_id"]},
                          token=tok, idem=True, ok=None)
    if st_code != 201:
        return None
    sc, intent = api.call("POST", "/orders/%s/payments" % o["order_no"], {"channel": "wechat"}, token=tok, idem=True, ok=None)
    if sc in (200, 201):
        settle = intent["payload"]["settle"]
        req = urllib.request.Request(api.base.rsplit("/api/v1", 1)[0] + settle["url"], method="POST",
                                     data=settle["body"].encode(), headers=dict(settle["headers"], Host=api.host))
        try:
            opener.open(req, timeout=30).read()
        except urllib.error.HTTPError:
            pass
    for ln in lines:  # 本地扣掉，免得同一天反复挑到已经见底的货
        for i, s in enumerate(skus):
            if s[0] == ln["sku_id"]:
                skus[i] = (s[0], s[1] - ln["quantity"], s[2], s[3])
    return o["order_no"]


def search_some(api, skus, rnd, n):
    titles = list({s[3] for s in skus if s[3]})
    for _ in range(n):
        q = rnd.choice(MISSING_TERMS) if rnd.random() < 0.18 else rnd.choice(titles)[:4]
        api.call("POST", "/search", {"query": q, "size": 10}, ok=None)


def run_day(a, api, db, st, rnd, n_orders, pw, phones):
    skus = catalog(api, st, rnd)
    toks, placed = {}, []
    for _ in range(n_orders):
        ph = rnd.choice(phones)
        if ph not in toks:
            toks[ph] = login(api, ph, pw)
        no = place(api, toks[ph], st, skus, rnd)
        if no:
            placed.append(no)
    if st.get("searchable"):
        search_some(api, skus, rnd, rnd.randint(n_orders, n_orders * 2 + 3))
    return placed


TZ = "Asia/Shanghai"  # 经营日按北京时间切；容器的会话时区是 UTC


def shift(db, st, nos, since, days, rnd):
    """把这一天的单挪到「北京时间今天往前 days 天」的 8–22 点，并保持下单的先后顺序。

    按「目标日期」算，不按「现在减 N 天」算：后者在零点前后跑会整体错一天，AI 员工第二天巡店
    就看到「昨天一单都没有」（真踩过）。保持先后顺序是因为库存流水的 before / after 是按真实
    先后扣出来的，断货天数按每天最后一条流水判 —— 打乱了收盘数就是错的。
    """
    if nos:
        mins = sorted(rnd.sample(range(8 * 60, 22 * 60), len(nos)))
        vals = ",".join("('%s', %d)" % (no, m) for no, m in zip(nos, mins))
        target = "((now() AT TIME ZONE '%s')::date - %d + make_interval(mins => v.m)) AT TIME ZONE '%s'" % (TZ, days, TZ)
        out = db.q("core", """
          WITH v(no, m) AS (VALUES %(v)s),
               d AS (SELECT o.id, o.order_no, %(t)s - o.created_at AS delta
                       FROM orders o JOIN v ON v.no = o.order_no),
               u AS (UPDATE orders o SET created_at = o.created_at + d.delta, paid_at = o.paid_at + d.delta,
                                         placed_at = o.placed_at + d.delta, expire_at = o.expire_at + d.delta
                       FROM d WHERE d.id = o.id RETURNING o.id),
               p AS (UPDATE payments p SET paid_at = p.paid_at + d.delta, created_at = p.created_at + d.delta
                       FROM d WHERE d.id = p.order_id RETURNING p.id)
          SELECT d.order_no, extract(epoch FROM d.delta)::bigint FROM d;""" % {"v": vals, "t": target})
        # 库存库可能是另一个库（拆分形态），所以把每单的位移带过去，而不是在那边重算
        deltas = ",".join("('%s', %s)" % tuple(r.split("\t")) for r in out.splitlines() if r)
        db.q("inv", """UPDATE inventory_logs l SET created_at = l.created_at + make_interval(secs => v.s)
                         FROM (VALUES %s) AS v(no, s) WHERE l.biz_id = v.no;""" % deltas)
    sh = "((now() AT TIME ZONE '%s')::date - %d + time '12:00') AT TIME ZONE '%s'" % (TZ, days, TZ)
    db.q("core", "UPDATE search_logs SET created_at = %s + (created_at - '%s') WHERE merchant_id = :mid AND created_at >= '%s';"
         % (sh, since, since), mid=st["merchant_id"])


def backdate_catalog(db, st, days):
    """商品、SKU、开店时的入库流水挪到模拟期开始之前。

    补货的「有效天数」= min(回看天数, SKU 上架至今的天数) − 断货天数；SKU 是今天建的，
    回看 14 天也只算 1 天，置信度永远是 low，手册规定 low 只进简报不提案（AI 员工真这么拒过）。
    入库流水也要挪：断货天数按流水推每天的收盘数，入库那条若晚于卖出的流水，前几天的收盘就是负的。
    """
    start = "((now() AT TIME ZONE '%s')::date - %d + time '07:00') AT TIME ZONE '%s'" % (TZ, days + 1, TZ)
    db.q("core", """UPDATE products SET created_at = %(s)s WHERE merchant_id = :mid;
                    UPDATE skus SET created_at = %(s)s WHERE merchant_id = :mid;
                    UPDATE stores SET created_at = %(s)s WHERE merchant_id = :mid;""" % {"s": start}, mid=st["merchant_id"])
    db.q("inv", "UPDATE inventory_logs SET created_at = %s WHERE merchant_id = :mid;" % start, mid=st["merchant_id"])


def set_warnings(api, tok, st, rnd):
    """预警线：没配的话库存见底也不会有 stock_low 事件，AI 员工会在简报里问「预警线配了吗」。"""
    for sku_id, avail, _, _ in catalog(api, st, rnd):
        api.call("PUT", "/admin/stores/%d/skus/%d/inventory" % (st["store_id"], sku_id),
                 {"expected_available_qty": avail, "available_qty": avail, "warning_qty": max(3, avail // 8)}, token=tok)


def backfill(a, api, db, st, rnd):
    pw, phones = a.buyer_password, buyers(db, st)
    for d in range(a.days, 0, -1):
        since = db.q("core", "SELECT now()")
        lo, hi = a.orders_per_day
        nos = run_day(a, api, db, st, rnd, rnd.randint(lo, hi), pw, phones)
        shift(db, st, nos, since, d, rnd)
        print("  第 -%2d 天：%d 单" % (d, len(nos)))


def simulate(a):
    api, db = setup(a)
    st = load_state(a)
    rnd = random.Random()
    nos = run_day(a, api, db, st, rnd, a.orders, a.buyer_password, buyers(db, st))
    print("现在下了 %d 单" % len(nos))


# ---------- 能讲的数字 ----------

REPORT_SQL = """
SELECT 'orders', count(*), coalesce(sum(paid_cents), 0) FROM orders WHERE merchant_id = :mid AND paid_at IS NOT NULL;
SELECT 'days', count(DISTINCT created_at::date), 0 FROM orders WHERE merchant_id = :mid AND paid_at IS NOT NULL;
SELECT 'products', count(*), 0 FROM products WHERE merchant_id = :mid AND status = 1;
SELECT 'nores', count(*), count(DISTINCT query) FROM search_logs WHERE merchant_id = :mid AND coalesce(cardinality(ranked_ids), 0) = 0;
"""


def report(a, api=None, db=None, st=None):
    if db is None:
        api, db = setup(a)
        st = load_state(a)
    mid = st["merchant_id"]
    rows = {r.split("\t")[0]: r.split("\t")[1:] for r in db.q("core", REPORT_SQL, mid=mid).splitlines()}
    sold = db.q("core", """
      SELECT s.id, p.title || coalesce(' ' || (SELECT string_agg(v, '/') FROM jsonb_each_text(s.spec_values) AS e(k, v)), ''), coalesce(sum(oi.quantity), 0)
        FROM skus s JOIN products p ON p.id = s.product_id
        LEFT JOIN order_items oi ON oi.sku_id = s.id
        LEFT JOIN orders o ON o.id = oi.order_id AND o.paid_at IS NOT NULL
       WHERE s.merchant_id = :mid GROUP BY 1, 2""", mid=mid)
    stock = dict(r.split("\t") for r in db.q("inv", "SELECT sku_id, available_qty FROM inventories WHERE store_id = :sid",
                                             sid=st["store_id"]).splitlines() if r)
    skus = [(name, int(q), int(stock.get(sid, 0))) for sid, name, q in (r.split("\t") for r in sold.splitlines() if r)]
    days = max(int(rows["days"][0]), 1)
    low = sorted([s for s in skus if s[1] > 0 and s[2] < s[1] / days * 3], key=lambda s: s[2])
    slow = sorted([s for s in skus if s[1] <= 1 and s[2] >= 20], key=lambda s: -s[2])
    terms = db.q("core", """SELECT query, count(*) FROM search_logs WHERE merchant_id = :mid AND coalesce(cardinality(ranked_ids), 0) = 0
                             GROUP BY 1 ORDER BY 2 DESC LIMIT 5""", mid=mid)
    n, gmv = int(rows["orders"][0]), int(rows["orders"][1])
    print("\n==== %s（%s）演示店：能讲的数字 ====" % (st["name"], st["code"]))
    print("在售商品 %s 件；近 %d 天 %d 单、成交 ¥%.2f，客单价 ¥%.2f"
          % (rows["products"][0], days, n, gmv / 100, gmv / 100 / max(n, 1)))
    print("快卖断的货（可售不到 3 天）%d 个：%s" % (len(low), "、".join("%s 剩 %d" % (s[0], s[2]) for s in low[:6]) or "无"))
    print("压着不动的货（%d 天卖 ≤1 件、库存 ≥20）%d 个：%s"
          % (days, len(slow), "、".join("%s 压 %d" % (s[0], s[2]) for s in slow[:6]) or "无"))
    if not st.get("searchable"):
        print("搜索：这套栈没有推理引擎，新店搜不到商品，没有模拟搜索（起栈带上 compose.infero.yaml 再建）")
    else:
        print("搜了没有的词 %s 次：%s" % (rows["nores"][0], "、".join(
            "「%s」%s 次" % tuple(t.split("\t")) for t in terms.splitlines() if t) or "无"))
    print("→ 让 AI 员工巡一次店：%s run-agent --code %s" % (os.path.relpath(__file__, ROOT), st["code"]))


def run_agent(a):
    """用参考运行器（Claude Code 无头）巡一次这家店。会真的调 Claude，按一次巡店的量计费。"""
    st = load_state(a)
    no_proxy = ",".join(filter(None, [os.environ.get("NO_PROXY"), "localhost", "127.0.0.1", ".nip.io"]))
    env = dict(os.environ, KEEL_AGENT_KEY=open(os.path.join(CONF_DIR, "prospect-%s-agent-key" % a.code)).read().strip(),
               KEEL_MCP_URL="http://%s:%s/api/v1/mcp" % (st["domain"], st["port"]), NO_PROXY=no_proxy, no_proxy=no_proxy)
    sys.exit(subprocess.call([os.path.join(ROOT, "agent/runner/claude-daily.sh")], env=env))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("create", "simulate", "report", "run-agent"):
        p = sub.add_parser(name)
        p.add_argument("--code", required=True, help="店铺编码（小写字母数字与连字符），也是子域名")
        p.add_argument("--project", default=os.environ.get("COMPOSE_PROJECT_NAME", "keelmulti"))
        p.add_argument("--api", default=os.environ.get("KEEL_PROSPECT_API"), help="默认 http://127.0.0.1:$KEEL_HTTP_PORT/api/v1")
        p.add_argument("--base-domain", default=os.environ.get("KEEL_BASE_DOMAIN", "example.com"))
        p.add_argument("--core-pg"), p.add_argument("--inv-pg")
        p.add_argument("--core-db", default="keel"), p.add_argument("--inv-db")
        p.add_argument("--buyer-password", default=os.environ.get("KEEL_PROSPECT_BUYER_PASSWORD", "keel-dev-2026"),
                       help="模拟顾客的口令 = 参考买家的口令（口令哈希从参考买家抄）")
        if name == "create":
            p.add_argument("--catalog", required=True)
            p.add_argument("--name", required=True)
            p.add_argument("--days", type=int, default=14)
            p.add_argument("--orders-per-day", type=lambda s: tuple(map(int, s.split("-"))), default=(12, 25))
            p.add_argument("--buyers", type=int, default=20)
            p.add_argument("--ref-buyer", default="13800000001", help="抄口令哈希的买家手机号（种子里 shop-a 的买家）")
            p.add_argument("--seed", type=int, default=7)
            p.add_argument("--store-address", default="浙江省,杭州市,西湖区,文三路 90 号",
                           help="门店地址：省,市,区,详细地址（模拟顾客的收货地址也在杭州西湖区）")
            p.add_argument("--lat", type=float, default=30.2777), p.add_argument("--lng", type=float, default=120.1306)
            p.add_argument("--no-images", action="store_true")
            p.add_argument("--force", action="store_true")
            p.add_argument("--domain", help="给这家店登记的域名，默认 <code>.127.0.0.1.nip.io（本机浏览器与 AI 员工都用它）")
            p.add_argument("--platform-token-file")
            p.add_argument("--platform-host", default=os.environ.get("KEEL_PROSPECT_PLATFORM_HOST"),
                           help="开店请求带的 Host（任一已有店的域名），默认 shop-a.<base-domain>")
        if name == "simulate":
            p.add_argument("--orders", type=int, default=5)
    a = ap.parse_args()
    {"create": create, "simulate": simulate, "report": report, "run-agent": run_agent}[a.cmd](a)


if __name__ == "__main__":
    main()
