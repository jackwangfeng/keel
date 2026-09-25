package dtm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/tenant"
)

// 这一组测试守的是 M2 最重要的一条不变量：**畸形 gid 不会静默地落到某个租户上。**
//
// 为什么这条值得一整个文件：ParseOrderGID 的返回值会被直接塞进
// `SET LOCAL app.merchant_id`，之后 RLS 就按那个值放行。也就是说这里解析出来的
// 数字**就是**数据库眼里的租户身份 —— RLS 不会再复核一遍，因为从它的角度看，
// 我们是明明白白地宣称了自己是那个租户。
//
// 于是「宽容的解析器」在这一层的代价不是一个 400，是一次落在别人名下的库存扣减，
// 而且不报错、不留痕。这跟越权读取不同：越权读取会被 RLS 拦住，而这条路径是
// 我们自己把钥匙递过去的。

// round trip：拼出来的 gid 必须能原样解析回去。
//
// 这条是阳性对照，必须排在前面。没有它，下面那一大串「必须报错」在一个
// `func ParseOrderGID(string) (int64,string,error) { return 0,"",err }` 的实现上
// 也全绿 —— 而那个实现让整个下单链路一笔都跑不起来。
func TestOrderGIDRoundTrip(t *testing.T) {
	cases := []struct {
		merchantID int64
		orderNo    string
	}{
		{1, "20260926000001"},
		{42, "20260926000002"},
		// 订单号里带 '-' 是允许的：解析只切第一个分隔符，租户段在它左边。
		{7, "2026-09-26-0001"},
		{9007199254740993, "x"},
	}
	for _, c := range cases {
		gid, err := dtm.OrderGID(c.merchantID, c.orderNo)
		if err != nil {
			t.Fatalf("OrderGID(%d, %q) 报错: %v", c.merchantID, c.orderNo, err)
		}
		mid, no, err := dtm.ParseOrderGID(gid)
		if err != nil {
			t.Fatalf("ParseOrderGID(%q) 报错: %v", gid, err)
		}
		if mid != c.merchantID || no != c.orderNo {
			t.Fatalf("round trip 不闭合：%q → (%d, %q)，期望 (%d, %q)",
				gid, mid, no, c.merchantID, c.orderNo)
		}
	}
}

// 畸形 gid 必须报错，**而且租户必须是 0**。
//
// 两条断言缺一不可。只断言 err != nil 的话，一个
// `return 12, "", err` 的实现照样绿，而调用方在错误分支上顺手用了第一个返回值
// （`mid, _, err := ...` 后面忘了 return）就会拿到 12 —— 一个能通过
// tenant.FromContext 的合法租户。断言 0 是在保证「就算调用方写错了，
// 拿到的也是个会被下游挡住的值」。
func TestParseOrderGIDRejectsMalformed(t *testing.T) {
	cases := []struct {
		gid string
		why string
	}{
		{"", "空串"},
		{"order-", "只有前缀"},
		{"order-12", "没有订单号那一段的分隔符"},
		{"order-12-", "订单号为空"},
		{"order--x", "租户段为空"},
		{"order-0-x", "0 不是任何商家；塞进 SET LOCAL 会让 RLS 过滤成空结果集"},
		{"order-012-x", "前导零：同一个租户两种写法，屏障的 gid 幂等就漏了"},
		{"order-+12-x", "strconv.ParseInt 接受 +12，我们不接受"},
		{"order- 12-x", "前导空格"},
		{"order-12 -x", "尾随空格"},
		{"order-1_2-x", "下划线"},
		{"order-12x-y", "不是整段数字，不能「前缀能解析就算数」"},
		{"order-１２-x", "全角数字"},
		{"order--12-x", "负号写在分隔符位上"},
		{"order-99999999999999999999-x", "溢出 int64"},
		{"refund-12-x", "别的业务的 gid，能解析出 12 但不该跑进下单分支"},
		{"ORDER-12-x", "前缀大小写不同"},
		{"xorder-12-x", "前缀只是被包含，不是开头"},
		{"12-x", "根本没有前缀"},
		{strings.Repeat("order-1-", 40), "超过 dtmrs 的 128 上限"},
	}
	for _, c := range cases {
		mid, no, err := dtm.ParseOrderGID(c.gid)
		if err == nil {
			t.Errorf("ParseOrderGID(%q) 居然成功了，解析出租户 %d 订单号 %q —— %s",
				c.gid, mid, no, c.why)
			continue
		}
		if !errors.Is(err, dtm.ErrNotOrderGID) {
			t.Errorf("ParseOrderGID(%q) 的错误不是 ErrNotOrderGID：%v", c.gid, err)
		}
		if mid != 0 {
			t.Errorf("ParseOrderGID(%q) 出错时返回了租户 %d，必须是 0 —— "+
				"调用方漏了一句 return 的时候，这个值会被塞进 SET LOCAL", c.gid, mid)
		}
		if no != "" {
			t.Errorf("ParseOrderGID(%q) 出错时返回了订单号 %q，必须是空串", c.gid, no)
		}
	}
}

// 别家租户的 gid 解析出来的就是别家租户，一个字都不会串到本进程「当前」的租户上。
//
// 这条看着像废话，但它守的是一个很具体的实现走样：把租户从 ctx 取（像 HTTP 那一侧
// 那样）而不是从 gid 取。那样写在单元测试里会全绿 —— 直到协调器崩溃重启，
// 在没有任何请求上下文的情况下重放分支，那时 ctx 里什么都没有；
// 或者更糟：进程里恰好有个带着 A 商家的 ctx 被复用，于是 B 商家的补偿
// 扣在了 A 商家的库存上。
func TestTenantComesFromGIDNotFromContext(t *testing.T) {
	gidB, err := dtm.OrderGID(2, "B0001")
	if err != nil {
		t.Fatal(err)
	}

	// 故意递一个已经带着商家 1 的 ctx —— 模拟「复用了别处的 context」。
	poisoned := tenant.NewContext(context.Background(), 1)

	ctx, mid, no, err := dtm.TenantContextFromGID(poisoned, gidB)
	if err != nil {
		t.Fatal(err)
	}
	if mid != 2 || no != "B0001" {
		t.Fatalf("解析出 (%d, %q)，期望 (2, \"B0001\")", mid, no)
	}
	got, err := tenant.FromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("ctx 里的租户是 %d，期望 2 —— gid 说了算，不是外面递进来的 ctx 说了算", got)
	}
}

// 解析失败时不许返回一个「能用」的 ctx。
//
// 返回 (poisoned, err) 而调用方漏判 err 的话，分支会在**上一个请求的租户**
// 名下把业务做掉。所以出错时 ctx 必须是 nil：漏判就是当场 panic，
// 而不是一次安静的跨租户写入。
func TestTenantContextFromGIDReturnsNoContextOnError(t *testing.T) {
	poisoned := tenant.NewContext(context.Background(), 1)
	ctx, mid, no, err := dtm.TenantContextFromGID(poisoned, "order-0-x")
	if err == nil {
		t.Fatal("畸形 gid 居然解析成功了")
	}
	if ctx != nil {
		got, ctxErr := tenant.FromContext(ctx)
		t.Fatalf("出错时返回了一个非 nil 的 ctx（里面的租户是 %d，err=%v）—— "+
			"调用方漏判 err 时它会拿这个 ctx 去写库", got, ctxErr)
	}
	if mid != 0 || no != "" {
		t.Fatalf("出错时返回了 (%d, %q)，必须是 (0, \"\")", mid, no)
	}
}

// 拼 gid 这一侧也要挡住会让解析走样的输入。
func TestOrderGIDRejectsBadInput(t *testing.T) {
	cases := []struct {
		merchantID int64
		orderNo    string
		why        string
	}{
		{0, "x", "0 不是任何商家"},
		{-1, "x", "负数租户"},
		{1, "", "订单号为空"},
		{1, "a b", "订单号里有空格"},
		{1, "a\nb", "订单号里有换行"},
		{1, strings.Repeat("9", 130), "gid 会超过 dtmrs 的 128 上限；" +
			"MySQL 上那是静默截断，两笔事务在屏障表里撞成同一行"},
	}
	for _, c := range cases {
		gid, err := dtm.OrderGID(c.merchantID, c.orderNo)
		if err == nil {
			t.Errorf("OrderGID(%d, %q) 居然成功了，给出 %q —— %s",
				c.merchantID, c.orderNo, gid, c.why)
		}
		if gid != "" {
			t.Errorf("OrderGID(%d, %q) 出错时还返回了 %q，必须是空串",
				c.merchantID, c.orderNo, gid)
		}
	}
}
