package tenant_test

import (
	"testing"

	"github.com/keel/keel/internal/tenant"
)

// 登记自有域名那条写路径要用的三个判据（internal/service/merchant_admin.go 调它们）。
//
// 这一组测试钉的不是「这三个函数算得对」——那是三行字符串操作，看一眼就放心。
// 它钉的是**这三者与解析器共用同一份实现**这件事还没被谁拆开：
//
//	NormalizeDomain 就是 Resolve 归一化 Host 用的那一个函数。写进去的文本与
//	比对时用的文本只要差一个大小写、一个结尾的点，那条登记就静静地不生效。
//	ValidDomain 就是 KEEL_BASE_DOMAIN 自己必须过的形状正则。
//	DomainUnderBase 就是 underBaseDomain，与 Resolve 分流用的同一条判据。
//
// 所以每条都拿解析器的行为当参照，而不是拿另一个字符串函数当参照：
// 参照是「这个域名进不进得去」，判据才有区分力。

func TestNormalizeDomainAgreesWithHostNormalization(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Shop-A.Example.COM", "shop-a.example.com"},      // 大小写：DNS 不区分
		{"shop-a.example.com.", "shop-a.example.com"},     // FQDN 结尾的点
		{"shop-a.example.com:8443", "shop-a.example.com"}, // Host 头带端口
		{"  shop-a.example.com  ", "shop-a.example.com"},  // 前后空白
		{"shop-a.example.com.:443", "shop-a.example.com"}, // 三个一起出现
		{"", ""},
	} {
		if got := tenant.NormalizeDomain(c.in); got != c.want {
			t.Errorf("NormalizeDomain(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 归一化是幂等的：登记接口对同一个值可能走「先校验再存」两次，
	// 不幂等的话第二次校验会拒掉自己第一次的产物。
	d := tenant.NormalizeDomain("Custom.Example.NET.")
	if again := tenant.NormalizeDomain(d); again != d {
		t.Errorf("NormalizeDomain 不幂等：%q → %q", d, again)
	}
}

func TestValidDomainIsTheBaseDomainShape(t *testing.T) {
	// 通过的：真实域名。
	for _, ok := range []string{
		"shop.example.net", "a.b.example.net", "xn--fiqs8s.example.com",
		"shop-1.example.co.uk", baseDomain,
		// 单字符标签过得去这条正则（DNS 允许它），"shop.example.c" 也一样过：
		// 这里管的是标签形状，不是「这个后缀是不是真实存在的 TLD」。
		// 把它当成该拒的一条会钉死一份谁也没写过的语义。
		"shop.example.c",
	} {
		if !tenant.ValidDomain(ok) {
			t.Errorf("ValidDomain(%q) = false —— 这形状是合法域名。"+
				"KEEL_BASE_DOMAIN 自己也要过这条正则，放宽或收紧都会两头不一致", ok)
		}
	}

	// 拒绝的：进不了 DNS 标签的写法，以及「看起来像但不是一个域名」的。
	//
	// 单段（"example"）必须拒：它过不了的话，登记一个 "com" 就占了整个顶级域，
	// 而解析器那边 byDomain 会拿它去等值比对一个完整 Host，永远匹配不上——
	// 一个不报错、不生效的登记，正是这条闸门要消灭的东西。
	for _, bad := range []string{
		"", " ", "example", "shop_example.net", "shop-a..example.com",
		"-shop.example.com", "shop-.example.com", "shop.exa mple.com",
		"shop.example.com:443", "shop.example_com.net",
		"//shop.example.com",
	} {
		if tenant.ValidDomain(bad) {
			t.Errorf("ValidDomain(%q) = true，期望拒", bad)
		}
	}

	// 大小写：ValidDomain 只吃已归一化的值。传大写进来判 false 是对的——
	// 「忘记先 NormalizeDomain」是调用方写得出来的一行代码，让它当场红，
	// 而不是放进库里一行永远匹配不上的登记。
	if tenant.ValidDomain("Shop.Example.NET") {
		t.Error(`ValidDomain("Shop.Example.NET") = true，期望 false：它只接受已归一化的值`)
	}
}

func TestDomainUnderBaseRejectsEveryNameThePlatformOwns(t *testing.T) {
	const base = baseDomain

	for _, c := range []struct {
		name string
		want bool
		why  string
	}{
		{"shop-a." + base, true, "恰好一级：code 那一条路"},
		{base, true, "apex 本身：放行的话商家能登记平台主站"},
		{"a.b." + base, true, "多级：放行的话商家能占住 admin.internal.<base> 这类平台后台域名"},
		{"x-not-a-code." + base, true, "与 code 无关的名字也在基础域名这片地盘里"},
		{"custom.example.net", false, "基础域名之外：这才是自有域名该走的那一支"},
		{"shop-a.example.net", false, "同第一段后缀不同，不是这片地盘"},
		{"notexample.com", false, "后缀相似但不是后缀"},
		{"shop-a." + base + ".evil.net", false, "把基础域名塞在中间，结尾不属于它"},
	} {
		if got := tenant.DomainUnderBase(c.name, base); got != c.want {
			t.Errorf("DomainUnderBase(%q, %q) = %v，期望 %v —— %s",
				c.name, base, got, c.want, c.why)
		}
	}

	// 没配 KEEL_BASE_DOMAIN：没有那片地盘，也就没有冲突。
	// 这一条不能退化成「任何两段名字都算落在某个基础域名之下」。
	for _, name := range []string{"shop.example.com", "example.com", "a.b.c"} {
		if tenant.DomainUnderBase(name, "") {
			t.Errorf("base 为空时 DomainUnderBase(%q) = true，期望 false", name)
		}
	}

	// base 自己带大写或带端口：与调用方传进来的 KEEL_BASE_DOMAIN 一致地归一化，
	// 否则「配置写法」会影响闸门判据。
	if !tenant.DomainUnderBase("shop-a.example.com", "EXAMPLE.com:443") {
		t.Error("base 没有归一化就参与比较")
	}
}

// 写入侧闸门用的是 `DomainUnderBase(NormalizeDomain(入参), base)` 这一处复合。
// 它必须对**同一个域名的各种写法**给出同一个结论，否则「换个拼法」就能把
// 一条本该被拒的登记塞进去：那条名字落在基础域名之下，解析器在那里只认 code，
// 于是库里有了一行永不生效的登记，而运营以为配好了。
//
// 反向也要稳：一个基础域名之外的域名，不能被任何拼法误判成「在基础域名之下」
// 而拒掉一条合法登记。
func TestTheGateGivesOneVerdictPerNameNotPerSpelling(t *testing.T) {
	gate := func(raw string) bool {
		return tenant.DomainUnderBase(tenant.NormalizeDomain(raw), baseDomain)
	}

	// 这五条是同一个名字「shop-a 落在 example.com 之下」的五种拼法。
	for _, raw := range []string{
		"shop-a.example.com", "Shop-A.Example.COM", "shop-a.example.com.",
		"shop-a.example.com:443", "  SHOP-a.Example.COM.  ",
	} {
		if !gate(raw) {
			t.Errorf("拼法 %q 绕过了闸门（归一化之后判成「不在基础域名之下」）—— "+
				"这种登记存得进库、解析器不采纳", raw)
		}
	}

	// 同理，apex 与多级不能有第二种拼法放行。
	for _, raw := range []string{"EXAMPLE.com", "example.com.", "A.B.Example.COM"} {
		if !gate(raw) {
			t.Errorf("拼法 %q 绕过了闸门", raw)
		}
	}

	for _, raw := range []string{
		"custom.example.net", "Custom.Example.NET", "custom.example.net.",
		"custom.example.net:8443", "notexample.com", "shop-a.example.com.evil.net",
	} {
		if gate(raw) {
			t.Errorf("拼法 %q 被误判成「落在基础域名之下」—— 一条合法登记会被 422 拒掉", raw)
		}
	}
}
