#!/usr/bin/env python3
"""给 H5（Flutter Web）浏览器测试造后台前置状态，把单号按 KEY=VALUE 打到 stdout。

flutter_app/integration_test 里有四条用例要「后台已经做过某件事」的单子，没给单号就跳过：
  KEEL_E2E_SHIPPED_ORDER     已发货的订单（买家确认收货）
  KEEL_E2E_REJECTED_REFUND   被驳回的售后（看驳回理由与通知）
  KEEL_E2E_REFUNDED_REFUND   已退款的售后
  KEEL_E2E_RETURN_REFUND     同意退货、等买家寄回的售后（填物流）
演示站上这些单号由本机脚本 demo-admin.sh 代做；CI 的那一栈由这个脚本经接口造。

  scripts/h5-e2e-fixtures.py --api http://127.0.0.1:28180/api/v1 --admin-session web/admin/e2e/.auth/session.json \\
      --phone 13800000000 --password keel-demo-2026 >> "$GITHUB_ENV"   # 或 eval 进当前 shell

后台身份用后台浏览器测试（make admin-e2e）留下的那份本店管理员会话：同一栈上它先跑，
引导 token 已经被它用掉了，这里不再重复走引导。
"""
import argparse, json, sys, urllib.error, urllib.request, uuid

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class Api:
    def __init__(self, base, token=None):
        self.base, self.token = base.rstrip("/"), token

    def call(self, method, path, body=None, ok=(200, 201)):
        h = {"Content-Type": "application/json"}
        if self.token:
            h["Authorization"] = "Bearer " + self.token
        if method != "GET":
            h["Idempotency-Key"] = str(uuid.uuid4())
        url = path if path.startswith("http") else self.base + path
        req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), headers=h, method=method)
        try:
            with opener.open(req, timeout=60) as r:
                st, raw = r.status, r.read()
        except urllib.error.HTTPError as e:
            st, raw = e.code, e.read()
        if st not in ok:
            sys.exit("✗ %s %s → %s %s" % (method, path, st, raw[:300].decode(errors="replace")))
        return json.loads(raw) if raw else None


def paid_order(buyer, anon, origin):
    """挑一个可售 ≥ 2 的 SKU 下一单并沙箱付款，返回订单号。"""
    store = next(s for s in anon.call("GET", "/stores")["items"] if s.get("is_default"))["id"]
    sku = None
    for p in anon.call("GET", "/products?page_size=50")["items"]:
        d = anon.call("GET", "/products/%d" % p["id"])
        sku = next((s["id"] for s in d.get("skus", []) if s.get("available_qty", 0) >= 2), None)
        if sku:
            break
    if not sku:
        sys.exit("✗ 店里没有可售 ≥ 2 的 SKU")
    addrs = buyer.call("GET", "/addresses")
    addrs = addrs.get("items", addrs) if isinstance(addrs, dict) else addrs
    addr = next((a for a in addrs if a.get("is_default")), addrs[0])["id"]
    o = buyer.call("POST", "/orders", {"items": [{"sku_id": sku, "quantity": 1}], "address_id": addr, "store_id": store})
    intent = buyer.call("POST", "/orders/%s/payments" % o["order_no"], {"channel": "wechat"})
    settle = intent["payload"]["settle"]
    req = urllib.request.Request(origin + settle["url"], data=settle["body"].encode(), headers=settle["headers"], method="POST")
    opener.open(req, timeout=30).read()
    return o["order_no"]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--api", required=True)
    ap.add_argument("--admin-session", required=True, help="StaffSession JSON 文件（本店管理员）")
    ap.add_argument("--phone", required=True)
    ap.add_argument("--password", required=True)
    a = ap.parse_args()
    origin = a.api.rstrip("/").rsplit("/api/v1", 1)[0]
    anon = Api(a.api)
    admin = Api(a.api, json.load(open(a.admin_session))["token"])
    buyer = Api(a.api, anon.call("POST", "/auth/login", {"phone": a.phone, "password": a.password})["access_token"])

    def ship(no):
        admin.call("POST", "/admin/orders/%s/shipments" % no, {"carrier_code": "sf", "tracking_no": "H5E2E" + uuid.uuid4().hex[:12]})

    def apply(no, refund_type):
        items = buyer.call("GET", "/orders/" + no)["items"]
        r = buyer.call("POST", "/orders/%s/refunds" % no, {"refund_type": refund_type, "reason_code": 1, "reason_text": "H5 e2e 夹具",
                                                           "items": [{"order_item_id": it["id"], "quantity": it["quantity"]} for it in items]})
        return r["refund_no"]

    shipped = paid_order(buyer, anon, origin)
    ship(shipped)

    rejected = apply(paid_order(buyer, anon, origin), 1)
    admin.call("POST", "/admin/refunds/%s/audit" % rejected, {"action": "reject", "reject_reason": "H5 e2e：照片看不清，请重新上传"})

    refunded = apply(paid_order(buyer, anon, origin), 1)
    admin.call("POST", "/admin/refunds/%s/audit" % refunded, {"action": "approve"})

    ret_order = paid_order(buyer, anon, origin)
    ship(ret_order)
    returning = apply(ret_order, 2)
    admin.call("POST", "/admin/refunds/%s/audit" % returning, {"action": "approve"})

    print("KEEL_E2E_SHIPPED_ORDER=%s" % shipped)
    print("KEEL_E2E_REJECTED_REFUND=%s" % rejected)
    print("KEEL_E2E_REFUNDED_REFUND=%s" % refunded)
    print("KEEL_E2E_RETURN_REFUND=%s" % returning)


if __name__ == "__main__":
    main()
