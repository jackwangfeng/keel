package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
)

// 店铺设置（契约 GET / PUT /admin/shop-settings，00059 的 shop_preferences）。
// 权限（只有管理员）由 permission_test.go 的矩阵逐角色敲；这里验读写、校验、
// 平台管理员切店之后写到哪一家，以及三个默认值与列默认值是同一个数。

const shopSettingsPath = "/api/v1/admin/shop-settings"

// setShopPreference 直接在库里改一家店的一项设置（没有那一行就插一行）。给要「店铺配成 N 天」
// 前提的用例用 —— 它们测的是定时任务，不是设置接口。
func setShopPreference(t *testing.T, merchantID int64, column string, value any) {
	t.Helper()
	switch column {
	case "auto_confirm_days", "return_ship_days", "timezone", "service_phone":
	default:
		t.Fatalf("setShopPreference 不认识列 %q", column)
	}
	adminExec(t, fmt.Sprintf(`INSERT INTO shop_preferences (merchant_id, %[1]s) VALUES ($1, $2)
		ON CONFLICT (merchant_id) DO UPDATE SET %[1]s = EXCLUDED.%[1]s`, column), merchantID, value)
}

// 三个 repository 默认值与列默认值是同一个数：两者分叉的后果是「改过设置的店」与
// 「没改过的店」在同一个默认之下按不同的天数 / 时区跑，而谁都没改过这个配置。
func TestShopPreferenceDefaultsMatchTheColumnDefaults(t *testing.T) {
	want := map[string]string{
		"auto_confirm_days": strconv.Itoa(repository.DefaultAutoConfirmDays),
		"return_ship_days":  strconv.Itoa(repository.DefaultReturnShipDays),
		"timezone":          "'" + repository.DefaultShopTimezone + "'::text",
	}
	for col, w := range want {
		var got string
		if err := admin(t).QueryRow(context.Background(), `
			SELECT column_default FROM information_schema.columns
			 WHERE table_name = 'shop_preferences' AND column_name = $1`, col).Scan(&got); err != nil {
			t.Fatalf("读 shop_preferences.%s 的列默认值: %v", col, err)
		}
		if got != w {
			t.Errorf("shop_preferences.%s 的列默认值是 %q，repository 的常量是 %q", col, got, w)
		}
	}
	// 搬走的两列不许还留在 shop_settings 上：留着就是两份真相（00059 文件头）。
	var n int
	if err := admin(t).QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'shop_settings' AND column_name IN ('timezone', 'auto_confirm_days')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("shop_settings 上还有 %d 列 timezone / auto_confirm_days —— 读旧列的地方会静默按旧值跑", n)
	}
}

func getShopSettings(t *testing.T, sh adminShop) api.ShopSettings {
	t.Helper()
	var out api.ShopSettings
	decodeInto(t, getAs(t, sh.Host, shopSettingsPath, sh.Token), http.StatusOK, "读店铺设置", &out)
	return out
}

func TestShopSettingsReadAndReplace(t *testing.T) {
	sh := newAdminShop(t)

	// 从没改过：默认值，updated_at 为 null，店铺名称是商家目录里的名字。
	got := getShopSettings(t, sh)
	if got.Timezone != repository.DefaultShopTimezone || got.AutoConfirmDays != repository.DefaultAutoConfirmDays ||
		got.ReturnShipDays != repository.DefaultReturnShipDays || got.ServicePhone != nil || got.UpdatedAt != nil {
		t.Fatalf("没改过的店铺设置是 %+v，期望全是默认值、updated_at 为 null", got)
	}
	var name string
	if err := admin(t).QueryRow(context.Background(), `SELECT name FROM merchants WHERE id = $1`,
		sh.MerchantID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if got.ShopName != name {
		t.Fatalf("shop_name = %q，商家目录里是 %q", got.ShopName, name)
	}
	// JSON 里 service_phone / updated_at 是 null 而不是缺席（契约里都是 required）。
	raw := getAs(t, sh.Host, shopSettingsPath, sh.Token).Body.String()
	if !strings.Contains(raw, `"service_phone":null`) || !strings.Contains(raw, `"updated_at":null`) {
		t.Fatalf("没设的可空字段应当是 null：%s", raw)
	}

	body := `{"timezone":"America/New_York","auto_confirm_days":10,"return_ship_days":5,"service_phone":" 400-800-1234 "}`
	var put api.ShopSettings
	decodeInto(t, putAs(t, sh.Host, shopSettingsPath, body, sh.Token), http.StatusOK, "改店铺设置", &put)
	if put.Timezone != "America/New_York" || put.AutoConfirmDays != 10 || put.ReturnShipDays != 5 ||
		put.ServicePhone == nil || *put.ServicePhone != "400-800-1234" || put.UpdatedAt == nil || put.ShopName != name {
		t.Fatalf("PUT 回显 %+v", put)
	}
	if again := getShopSettings(t, sh); again.AutoConfirmDays != 10 || again.Timezone != "America/New_York" {
		t.Fatalf("PUT 之后再读是 %+v", again)
	}
	// 天然幂等：同一个请求体再放一次，结果一样，库里仍是一行。
	var put2 api.ShopSettings
	decodeInto(t, putAs(t, sh.Host, shopSettingsPath, body, sh.Token), http.StatusOK, "重放 PUT", &put2)
	if put2.AutoConfirmDays != 10 || put2.ReturnShipDays != 5 || *put2.ServicePhone != "400-800-1234" {
		t.Fatalf("重放 PUT 回显 %+v", put2)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM shop_preferences WHERE merchant_id = $1`, sh.MerchantID); n != 1 {
		t.Fatalf("两次 PUT 之后这家店有 %d 行设置，期望 1", n)
	}

	// 整体替换：不给 service_phone 即清空。
	decodeInto(t, putAs(t, sh.Host, shopSettingsPath,
		`{"timezone":"UTC","auto_confirm_days":1,"return_ship_days":365}`, sh.Token), http.StatusOK, "清空客服电话", &put)
	if put.ServicePhone != nil || put.Timezone != "UTC" || put.AutoConfirmDays != 1 || put.ReturnShipDays != 365 {
		t.Fatalf("整体替换之后是 %+v，期望客服电话被清空、边界值 1 / 365 收下", put)
	}
}

// 校验：每一条 422 都点名字段，而且库里一个字都没改。
func TestShopSettingsRejectsBadValues(t *testing.T) {
	sh := newAdminShop(t)
	wantStatus(t, putAs(t, sh.Host, shopSettingsPath,
		`{"timezone":"Asia/Tokyo","auto_confirm_days":9,"return_ship_days":9}`, sh.Token), http.StatusOK, "先存一份")

	cases := []struct{ body, field string }{
		{`{"timezone":"Local","auto_confirm_days":7,"return_ship_days":7}`, "timezone"},
		{`{"timezone":"local","auto_confirm_days":7,"return_ship_days":7}`, "timezone"},
		{`{"timezone":"Mars/Olympus","auto_confirm_days":7,"return_ship_days":7}`, "timezone"},
		{`{"timezone":"CST","auto_confirm_days":7,"return_ship_days":7}`, "timezone"},
		{`{"timezone":"","auto_confirm_days":7,"return_ship_days":7}`, "timezone"},
		{`{"auto_confirm_days":7,"return_ship_days":7}`, "timezone"},
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":0,"return_ship_days":7}`, "auto_confirm_days"},
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":366,"return_ship_days":7}`, "auto_confirm_days"},
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":7,"return_ship_days":0}`, "return_ship_days"},
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":7}`, "return_ship_days"},
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":7,"return_ship_days":366}`, "return_ship_days"},
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":7,"return_ship_days":7,"service_phone":"  "}`, "service_phone"},
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":7,"return_ship_days":7,"service_phone":"` +
			strings.Repeat("1", 33) + `"}`, "service_phone"},
		// 乱点测试的真实案例：格式不对的字符串（含 <script>）不该被当成合法电话存进去。
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":7,"return_ship_days":7,"service_phone":"notaphone<script>"}`, "service_phone"},
		{`{"timezone":"Asia/Shanghai","auto_confirm_days":7,"return_ship_days":7,"service_phone":"123"}`, "service_phone"},
	}
	for _, c := range cases {
		w := putAs(t, sh.Host, shopSettingsPath, c.body, sh.Token)
		p := problemOf(t, w, http.StatusUnprocessableEntity)
		if p.Errors == nil {
			t.Errorf("%s：422 没带 errors：%s", c.body, w.Body.String())
			continue
		}
		hit := false
		for _, e := range *p.Errors {
			hit = hit || (e.Field != nil && *e.Field == c.field)
		}
		if !hit {
			t.Errorf("%s：422 没点名 %s：%s", c.body, c.field, w.Body.String())
		}
	}
	if got := getShopSettings(t, sh); got.Timezone != "Asia/Tokyo" || got.AutoConfirmDays != 9 || got.ReturnShipDays != 9 {
		t.Fatalf("被拒的 PUT 改动了库里的设置：%+v", got)
	}
	// 请求体不是 JSON。
	wantStatus(t, putAs(t, sh.Host, shopSettingsPath, `not json`, sh.Token), http.StatusUnprocessableEntity, "坏请求体")
}

// 客服电话格式闸门要收下商家实际会填的几种号码：手机、座机（带/不带分隔符）、
// 400/800，以及带分机号的座机。422 那半边已经在 TestShopSettingsRejectsBadValues
// 钉过了，这里补阳性对照——一个「只挡格式错的，不小心也挡了格式对的」的实现
// 在上面那条测试里全是绿的。
func TestShopSettingsAcceptsRealisticPhoneFormats(t *testing.T) {
	sh := newAdminShop(t)
	for _, phone := range []string{
		"13812345678",
		"010-12345678",
		"0512 1234567",
		"4001234567",
		"400-123-4567",
		"010-12345678转8080",
		"400-123-4567-1",
	} {
		body := fmt.Sprintf(`{"timezone":"Asia/Shanghai","auto_confirm_days":7,"return_ship_days":7,"service_phone":%q}`, phone)
		var put api.ShopSettings
		decodeInto(t, putAs(t, sh.Host, shopSettingsPath, body, sh.Token), http.StatusOK, "合法电话 "+phone, &put)
		if put.ServicePhone == nil || *put.ServicePhone != phone {
			t.Errorf("service_phone=%q 被拒或回显不对：%+v", phone, put)
		}
	}
}

// 平台管理员经 X-Keel-Merchant 切到 B 店：写落在 B 店，A 店（Host 那家）一个字不动；
// shop_name 回显的是 B 店的名字。这也是 shop_preferences 的租户隔离在接口层的一次验证。
func TestPlatformAdminEditsShopSettingsOfTheSwitchedMerchant(t *testing.T) {
	a, b := newAdminShop(t), newAdminShop(t)
	token := newPlatformAdmin(t)

	w := switchReq(t, nil, http.MethodPut, a.Host, shopSettingsPath,
		`{"timezone":"Europe/Paris","auto_confirm_days":12,"return_ship_days":3}`, token, shopCode(b))
	var out api.ShopSettings
	decodeInto(t, w, http.StatusOK, "平台管理员切到 B 店改设置", &out)
	var bName string
	if err := admin(t).QueryRow(context.Background(), `SELECT name FROM merchants WHERE id = $1`,
		b.MerchantID).Scan(&bName); err != nil {
		t.Fatal(err)
	}
	if out.ShopName != bName || out.AutoConfirmDays != 12 {
		t.Fatalf("切到 B 店改设置回显 %+v，期望 B 店（%s）", out, bName)
	}
	if got := getShopSettings(t, b); got.Timezone != "Europe/Paris" || got.ReturnShipDays != 3 {
		t.Fatalf("B 店的设置是 %+v，平台管理员的修改没落在 B 店", got)
	}
	if got := getShopSettings(t, a); got.UpdatedAt != nil || got.Timezone != repository.DefaultShopTimezone {
		t.Fatalf("A 店（请求的 Host）的设置被改了：%+v", got)
	}

	// 商家级管理员带头切别家：403 tenant-switch-forbidden（与其它后台接口一致）。
	w = switchReq(t, nil, http.MethodGet, a.Host, shopSettingsPath, "", a.Token, shopCode(b))
	if got := problemType(t, w, http.StatusForbidden, "商家级管理员带头读"); got != problem.TypeTenantSwitchForbidden {
		t.Fatalf("type 是 %q，期望 %q", got, problem.TypeTenantSwitchForbidden)
	}
	var leaked map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &leaked)
	if _, ok := leaked["timezone"]; ok {
		t.Fatalf("403 的响应体里出现了设置：%s", w.Body.String())
	}
}

// 订单详情的 auto_confirm_at：只有已发货的单有，= 发货时间 + 店铺设置的天数，
// 改了设置跟着变（按此刻的设置算，与定时任务同一个口径）。
func TestOrderDetailCarriesAutoConfirmAt(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "auto-confirm-at")
	paid := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	if d, _ := cs.lines(t, b, paid.OrderNo); d.AutoConfirmAt != nil {
		t.Fatalf("没发货的订单带了 auto_confirm_at：%v", d.AutoConfirmAt)
	}
	wantStatus(t, cs.ship(t, paid.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")

	d, _ := cs.lines(t, b, paid.OrderNo)
	if d.ShippedAt == nil || d.AutoConfirmAt == nil ||
		!d.AutoConfirmAt.Equal(d.ShippedAt.Add(time.Duration(repository.DefaultAutoConfirmDays)*24*time.Hour)) {
		t.Fatalf("已发货订单的 auto_confirm_at = %v，发货时间 %v，期望发货 + %d 天",
			d.AutoConfirmAt, d.ShippedAt, repository.DefaultAutoConfirmDays)
	}
	setShopPreference(t, cs.MerchantID, "auto_confirm_days", 15)
	d, _ = cs.lines(t, b, paid.OrderNo)
	if !d.AutoConfirmAt.Equal(d.ShippedAt.Add(15 * 24 * time.Hour)) {
		t.Fatalf("改成 15 天之后 auto_confirm_at = %v，期望发货 + 15 天", d.AutoConfirmAt)
	}

	wantStatus(t, orderAction(t, cs.Host, paid.OrderNo, "confirm", b.Token, "cf-"+uniqueKey()), http.StatusOK, "确认收货")
	if d, _ := cs.lines(t, b, paid.OrderNo); d.AutoConfirmAt != nil {
		t.Fatalf("已完成的订单还带着 auto_confirm_at：%v", d.AutoConfirmAt)
	}
}
