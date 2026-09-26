package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 买家自己的三组接口（个人信息 / 地址簿 / 购物车）的夹具。
//
// 每条测试在一家**新开的店**里跑（newCouponShop：两个大区各一家门店，
// 两件各有 50 件库存的在架商品），买家直接落 users 行再签令牌 ——
// 登录不是这一组在验的东西。
//
// 同一家店里至少要两个买家：越权那几条测试的全部意义在于「同一个租户里的
// 另一个人」。跨租户那一面由 RLS 挡，另开一家店来验。

type buyerShop struct {
	couponShop
}

func newBuyerShop(t *testing.T) buyerShop {
	t.Helper()
	cs := newCouponShop(t)
	// 这几张表在 couponShop 的清理清单之外，而它们都挂在 users 上。
	// t.Cleanup 后进先出：这一段先跑，然后才轮到 couponShop 删 users。
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM cart_items WHERE merchant_id = $1`,
			`DELETE FROM carts WHERE merchant_id = $1`,
			`DELETE FROM user_identities WHERE merchant_id = $1`,
			`DELETE FROM idempotency_keys WHERE merchant_id = $1`,
		} {
			if _, err := admin(t).Exec(context.Background(), stmt, cs.MerchantID); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})
	return buyerShop{couponShop: cs}
}

// call 以某个买家的身份打一个请求。POST 自动带一把新鲜的 Idempotency-Key
// （reqAs 的约定）；要指定钥匙的走 callWithKey。
func (bs buyerShop) call(t *testing.T, method, path, body string, b couponBuyer) *httptest.ResponseRecorder {
	t.Helper()
	return reqAs(t, method, bs.Host, path, body, b.Token)
}

func (bs buyerShop) callWithKey(t *testing.T, path, body string, b couponBuyer, key string) *httptest.ResponseRecorder {
	t.Helper()
	return postWithKey(t, bs.Host, path, body, b.Token, key)
}

// ---- 地址簿 ----

func addressBody(receiver string, isDefault bool) string {
	return fmt.Sprintf(`{"receiver_name":%q,"phone":"13800138000","province":"广东省","city":"广州市",`+
		`"district":"天河区","street":"林和街道","detail":"天河路 1 号","tag":1,"is_default":%v}`,
		receiver, isDefault)
}

func (bs buyerShop) createAddress(t *testing.T, b couponBuyer, receiver string, isDefault bool) api.Address {
	t.Helper()
	var a api.Address
	decodeInto(t, bs.call(t, http.MethodPost, "/api/v1/addresses", addressBody(receiver, isDefault), b),
		http.StatusCreated, "新增地址", &a)
	return a
}

func (bs buyerShop) listAddresses(t *testing.T, b couponBuyer) []api.Address {
	t.Helper()
	var out []api.Address
	decodeInto(t, bs.call(t, http.MethodGet, "/api/v1/addresses", "", b), http.StatusOK, "地址簿", &out)
	return out
}

// defaultAddressCount 绕过 RLS 直接数这个买家有几条未删除的默认地址。
// 「至多一个」这条规则的判据在库里，不在响应里。
func defaultAddressCount(t *testing.T, userID int64) int64 {
	t.Helper()
	return adminQueryInt64(t,
		`SELECT count(*) FROM user_addresses WHERE user_id = $1 AND is_default AND deleted_at IS NULL`, userID)
}

// ---- 购物车 ----

func cartPath(path string, storeID int64) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%sstore_id=%d", path, sep, storeID)
}

func (bs buyerShop) getCart(t *testing.T, b couponBuyer, storeID int64) api.Cart {
	t.Helper()
	var out api.Cart
	decodeInto(t, bs.call(t, http.MethodGet, cartPath("/api/v1/cart", storeID), "", b),
		http.StatusOK, "读购物车", &out)
	return out
}

func (bs buyerShop) addToCart(t *testing.T, b couponBuyer, storeID, skuID int64, qty int) *httptest.ResponseRecorder {
	t.Helper()
	return bs.call(t, http.MethodPost, cartPath("/api/v1/cart/items", storeID),
		fmt.Sprintf(`{"sku_id":%d,"quantity":%d}`, skuID, qty), b)
}

func (bs buyerShop) mustAdd(t *testing.T, b couponBuyer, storeID, skuID int64, qty int) api.Cart {
	t.Helper()
	var out api.Cart
	decodeInto(t, bs.addToCart(t, b, storeID, skuID, qty), http.StatusOK, "加购", &out)
	return out
}

// lineOf 在车里按 sku 找一行；找不到直接 Fatal —— 一个静默的零值会让后面
// 所有断言在「这一行根本不在」时照样比出点什么来。
func lineOf(t *testing.T, c api.Cart, skuID int64) api.CartItem {
	t.Helper()
	for _, it := range c.Items {
		if it.SkuId == skuID {
			return it
		}
	}
	t.Fatalf("车里没有 sku %d：%+v", skuID, c.Items)
	return api.CartItem{}
}

// cartItemCount 绕过 RLS 数这个买家车里有几行。
func cartItemCount(t *testing.T, userID int64) int64 {
	t.Helper()
	return adminQueryInt64(t,
		`SELECT count(*) FROM cart_items ci JOIN carts c ON c.id = ci.cart_id WHERE c.user_id = $1`, userID)
}
