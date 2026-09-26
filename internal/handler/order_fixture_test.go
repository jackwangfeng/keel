package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

// 下单链路测试的夹具与「绕过被测代码去核对事实」的那几个读法。
//
// 这里每一个读库的助手都走**管理员连接**（绕过 RLS），而不是再打一次 HTTP。
// 理由是本仓库反复出现过的那类问题：断言存在，但和被测代码没有因果关系。
// 「下单之后再 GET 一次订单，看到数字对得上」证明不了库存真的被扣了 ——
// 它只证明了同一段代码前后自洽。直接读 inventories / inventory_logs / orders
// 才能把「我守的这件事坏了」和「附近有别的东西坏了」分开。

// postJSON 往真实路由上发一个带任意请求头的 JSON 请求。
//
// auth_test.go 里那个 post 不带自定义头，而 Idempotency-Key 是这条链路的主角，
// 所以这里另起一个，而不是去改那个已经被十几条测试依赖的助手。
func postJSON(t *testing.T, host, path, body, bearer string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

// createOrder 打 POST /orders，带上幂等键。
func createOrder(t *testing.T, host, body, bearer, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, host, "/api/v1/orders", body, bearer,
		map[string]string{"Idempotency-Key": idemKey})
}

// previewOrder 打 POST /orders/preview。
func previewOrder(t *testing.T, host, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, host, "/api/v1/orders/preview", body, bearer, nil)
}

// admin 拿一条绕过 RLS 的连接，用来核对库里真实发生了什么。
func admin(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// merchantIDOf 按 code 取商家 id。gid 里那个租户段要用它。
func merchantIDOf(t *testing.T, code string) int64 {
	t.Helper()
	var id int64
	if err := admin(t).QueryRow(context.Background(),
		`SELECT id FROM merchants WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("取商家 %s 失败: %v", code, err)
	}
	return id
}

// skuIDOf 按商家 code + sku_code 取 SKU id。
//
// 不写死 id：种子里的 id 是自增的，跟加载顺序走，写死一个数字的测试
// 会在别人往种子里插一行之后变成「测另一个 SKU」，而且照样绿。
func skuIDOf(t *testing.T, merchantCode, skuCode string) int64 {
	t.Helper()
	var id int64
	err := admin(t).QueryRow(context.Background(), `
		SELECT s.id FROM skus s
		  JOIN merchants m ON m.id = s.merchant_id
		 WHERE m.code = $1 AND s.sku_code = $2`, merchantCode, skuCode).Scan(&id)
	if err != nil {
		t.Fatalf("取 %s 的 SKU %s 失败: %v", merchantCode, skuCode, err)
	}
	return id
}

// anySKUWithStock 挑一个当前水位 >= want 的 SKU，返回它的 id 与水位。
//
// 不挑固定的那一个：同一个包里的测试共用一个库，前面的测试扣过库存之后，
// 一个写死的 SKU 可能已经不够扣了 —— 那时测试会以「库存不足」的形式失败，
// 而真因是夹具，不是被测代码。
func anySKUWithStock(t *testing.T, merchantCode string, want int32) (skuID int64, qty int32) {
	t.Helper()
	// 商品必须在架且未软删：种子里 shop-a 刻意有一件草稿商品和一件软删商品，
	// 它们的 SKU 一样有库存行（「商品下架不等于 SKU 消失」）。挑中它们的话，
	// 定价会正确地把它判成「不可售」，而测试会以 422 的形式失败 ——
	// 真因是夹具挑错了 SKU，不是被测代码。这三个条件与
	// db/queries/orders.sql 的 ListSKUsForPricing 逐字对应。
	err := admin(t).QueryRow(context.Background(), `
		SELECT i.sku_id, i.available_qty
		  FROM inventories i
		  JOIN skus s      ON s.id = i.sku_id
		  JOIN products p  ON p.id = s.product_id
		  JOIN merchants m ON m.id = s.merchant_id
		 WHERE m.code = $1 AND s.status = 1
		   AND p.status = 1 AND p.deleted_at IS NULL
		   AND i.available_qty >= $2
		 ORDER BY i.available_qty DESC
		 LIMIT 1`, merchantCode, want).Scan(&skuID, &qty)
	if err != nil {
		t.Fatalf("%s 里找不到水位 >= %d 的 SKU: %v", merchantCode, want, err)
	}
	return skuID, qty
}

// availableOf 绕过 RLS 读真实水位。
func availableOf(t *testing.T, skuID int64) int32 {
	t.Helper()
	var qty int32
	if err := admin(t).QueryRow(context.Background(),
		`SELECT available_qty FROM inventories WHERE sku_id = $1`, skuID).Scan(&qty); err != nil {
		t.Fatalf("读 sku %d 的水位失败: %v", skuID, err)
	}
	return qty
}

// storeIDOf 取这家商家那家默认门店的 id（种子里 db/seed/dev.sql 播的那一家）。
//
// 不写死 id，理由同 skuIDOf：种子里的 id 是自增的，跟加载顺序走。
//
// 查不到直接 Fatal，**不回落到「随便一家店」**：契约里 store_id 是必填的，
// 而服务端那一侧刻意没有写任何回落分支（见 handler/order.go 上那段注释）。
// 夹具要是自己回落了，「漏传 store_id 会怎样」这件事就永远测不到，
// 而线上那一笔会按错误的门店扣减、按错误的门店计价，两边都是合法数据。
func storeIDOf(t *testing.T, merchantCode string) int64 {
	t.Helper()
	var id int64
	err := admin(t).QueryRow(context.Background(), `
		SELECT st.id FROM stores st
		  JOIN merchants m ON m.id = st.merchant_id
		 WHERE m.code = $1 AND st.is_default AND st.deleted_at IS NULL`,
		merchantCode).Scan(&id)
	if err != nil {
		t.Fatalf("取 %s 的默认门店失败: %v —— db/seed/dev.sql 里那段门店种子还在吗？",
			merchantCode, err)
	}
	return id
}

// addressIDOf 按商家 code + 收件人取地址 id（种子里播的那两条）。
func addressIDOf(t *testing.T, merchantCode, receiver string) int64 {
	t.Helper()
	var id int64
	err := admin(t).QueryRow(context.Background(), `
		SELECT a.id FROM user_addresses a
		  JOIN merchants m ON m.id = a.merchant_id
		 WHERE m.code = $1 AND a.receiver_name = $2`, merchantCode, receiver).Scan(&id)
	if err != nil {
		t.Fatalf("取 %s 的地址 %s 失败: %v", merchantCode, receiver, err)
	}
	return id
}

// orderStatusOf 读订单的履约状态。查不到返回 -1 —— 用一个不可能的状态值而不是 0，
// 因为 0 是「创建中」，是个合法状态。
func orderStatusOf(t *testing.T, orderNo string) int16 {
	t.Helper()
	var status int16
	err := admin(t).QueryRow(context.Background(),
		`SELECT status FROM orders WHERE order_no = $1`, orderNo).Scan(&status)
	if err == pgx.ErrNoRows {
		return -1
	}
	if err != nil {
		t.Fatalf("读订单 %s 失败: %v", orderNo, err)
	}
	return status
}

// invLog 是 inventory_logs 里的一行。
type invLog struct {
	SKUID     int64
	ChangeQty int32
	BizType   int16
	Before    int32
	After     int32
}

// inventoryLogsOf 读一笔订单的全部库存流水，按写入顺序。
//
// **这是「补偿真的跑了」与「根本没扣过」之间唯一的区别。**
// 正向扣减 + 补偿回补跑完之后，available_qty 回到原值，和从来没扣过一模一样；
// 只有这两行流水能把两者分开。断言只看水位的话，把整个库存分支删掉，
// 那条断言照样绿。
func inventoryLogsOf(t *testing.T, orderNo string) []invLog {
	t.Helper()
	rows, err := admin(t).Query(context.Background(), `
		SELECT sku_id, change_qty, biz_type, before_available, after_available
		  FROM inventory_logs WHERE biz_id = $1 ORDER BY id`, orderNo)
	if err != nil {
		t.Fatalf("读订单 %s 的库存流水失败: %v", orderNo, err)
	}
	defer rows.Close()
	var out []invLog
	for rows.Next() {
		var l invLog
		if err := rows.Scan(&l.SKUID, &l.ChangeQty, &l.BizType, &l.Before, &l.After); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// orderCountForKey 数一个幂等键下真的产生了几笔订单。
//
// 靠 idempotency_keys 的存档反查订单号，而不是数「这个用户一共有几单」——
// 同一个包里别的测试也在给同一个买家下单，那个计数会互相干扰，
// 而互相干扰的断言迟早会被改成一个恒真的不等式。
func orderNoForKey(t *testing.T, merchantCode, idemKey string) string {
	t.Helper()
	var body []byte
	err := admin(t).QueryRow(context.Background(), `
		SELECT k.response_body FROM idempotency_keys k
		  JOIN merchants m ON m.id = k.merchant_id
		 WHERE m.code = $1 AND k.idem_key = $2`, merchantCode, idemKey).Scan(&body)
	if err != nil {
		t.Fatalf("取幂等键 %s 的存档失败: %v", idemKey, err)
	}
	var archived struct {
		OrderNo string `json:"OrderNo"`
	}
	if err := json.Unmarshal(body, &archived); err != nil {
		t.Fatalf("存档解析失败: %v\n%s", err, body)
	}
	return archived.OrderNo
}

// ordersWithNo 数库里叫这个单号的订单有几笔（幂等断言用）。
func ordersWithNo(t *testing.T, orderNo string) int {
	t.Helper()
	var n int
	if err := admin(t).QueryRow(context.Background(),
		`SELECT count(*) FROM orders WHERE order_no = $1`, orderNo).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// orderBody 拼一个 OrderCreateRequest。
//
// store_id 由 merchantCode 现查（00020 起契约把它定成**必填**），而不是留给
// 每个调用点自己拼：漏掉它的症状是一个 422，而那个 422 与「请求体别处写错了」
// 长得一模一样 —— 二十来个调用点里只要有一个漏了，排查的人会先去翻定价。
func orderBody(t *testing.T, merchantCode string, addressID, skuID int64, qty int, extra string) string {
	t.Helper()
	body := fmt.Sprintf(`{"address_id":%d,"store_id":%d,"items":[{"sku_id":%d,"quantity":%d}]`,
		addressID, storeIDOf(t, merchantCode), skuID, qty)
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}
