package dtm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/keel/keel/internal/tenant"
)

// gid 必须携带租户，形状是 `order-{merchant_id}-{order_no}`。
//
// 这不是审美问题，是 FFI 接口面推出来的唯一解：
//
//   - SubmitSaga(gid, stepsJSON) 的 steps 只有 {"action","compensate"} 两个地址，
//     **不带任何业务载荷**；
//   - BranchFunc 只拿到 (gid, branchID, op) 三个字符串；
//   - 分支跑在任何 HTTP 请求之外，崩溃重启后由协调器重放，进程对原请求毫无记忆。
//
// 于是分支需要的一切都得从 gid 推出来，**包括租户**。而租户是分支写库的前提：
// 没有 `SET LOCAL app.merchant_id`，RLS 下每一条语句都撞 current_merchant() 抛的
// 42501。想反过来「按 order_no 查表反查租户」是个死循环 —— 那张表本身在 RLS 之下。
//
// 也不为此开一张 RLS 豁免表：一张能按 order_no 无租户查出 merchant_id 的表，
// 就是一条绕过 RLS 的合法路径，它会比它解决的问题活得久。
//
// 架构 §5 原先写的是 `order-{order_no}`，本轮改成这个形状。

const (
	// OrderGIDPrefix 是下单 SAGA 的 gid 前缀。
	//
	// 带前缀而不是光秃秃的 `{merchant_id}-{order_no}`，是为了让别的业务（退款、
	// 超时关单）将来能有自己的前缀，而**解析函数会拒绝不属于自己的那些** ——
	// 一个退款 gid 落进下单分支时，我们要的是一句报错，不是一个碰巧能解析出来的
	// merchant_id 加一次跑错了地方的业务写入。
	OrderGIDPrefix = "order-"

	// maxGIDLen 来自 dtmrs：Backend::ID_MAX = 128（crates/dtmrs-core/src/dialect.rs）。
	//
	// 超了必须在这里挡掉，不能留给 dtmrs。它在 Postgres 上会报错（能看见），
	// 但在 MySQL 上 `INSERT IGNORE` 会把超长 gid **静默截断**（dtmrs 自己的注释
	// 写了这件事）：两笔前 128 个字符相同的事务在屏障表里撞成同一行，后来那笔
	// 被判成 Duplicated 直接跳过业务逻辑。静默、且丢数据。
	maxGIDLen = 128
)

// ErrNotOrderGID 表示这个字符串不是一个合法的下单 gid。
//
// 它永远伴随「租户是 0」一起返回。绝不回落到任何默认租户 —— 回落的后果不是
// 报错，是一次落在别人名下的库存扣减，而且不会有任何东西报警。
var ErrNotOrderGID = errors.New("不是合法的下单 gid")

// OrderGID 拼出下单 SAGA 的 gid。
//
// 返回 error 而不是无脑拼接：拼出一个 dtmrs 会拒绝（或更糟，会截断）的 gid，
// 报错要发生在这里，而不是发生在提交那一刻或者半年后的对账上。
func OrderGID(merchantID int64, orderNo string) (string, error) {
	if merchantID <= 0 {
		return "", fmt.Errorf("%w: merchant_id 是 %d，必须为正", ErrNotOrderGID, merchantID)
	}
	if orderNo == "" {
		return "", fmt.Errorf("%w: order_no 为空", ErrNotOrderGID)
	}
	// order_no 里带这几个字符会让 gid 解析不回来（前两段靠 '-' 切分，
	// 而首尾空白 / 换行会让「看起来一样的两个 gid」是两笔不同的事务）。
	if strings.ContainsAny(orderNo, " \t\r\n") {
		return "", fmt.Errorf("%w: order_no %q 含空白字符", ErrNotOrderGID, orderNo)
	}
	gid := OrderGIDPrefix + strconv.FormatInt(merchantID, 10) + "-" + orderNo
	if len(gid) > maxGIDLen {
		return "", fmt.Errorf("%w: gid 长度 %d 超过 dtmrs 的上限 %d（order_no 太长）",
			ErrNotOrderGID, len(gid), maxGIDLen)
	}
	return gid, nil
}

// ParseOrderGID 从 gid 里取回租户与订单号。
//
// **它是严格的，而且必须严格。** 这个函数是租户这一路上唯一的检查点：
// 它的返回值会被直接塞进 `SET LOCAL app.merchant_id`，之后 RLS 就按那个值放行。
// 一个「宽容」的解析器在这里的代价是一次跨租户写入 —— 而 RLS 不会拦它，
// 因为从数据库的角度看，我们是明明白白地宣称了自己是那个租户。
//
// 所以下面每一条拒绝都有对应的攻击/事故形状：
//
//	"order-012-x"   前导零。ParseInt 会给出 12，于是同一个租户有两种 gid 写法，
//	                而屏障表按 gid 做幂等 —— 同一笔业务能被执行两遍。
//	"order-+12-x"   ParseInt 接受 "+12"。同上。
//	"order- 12-x"   ParseInt 不接受前导空格，但别指望这一点：显式查。
//	"order-12x-y"   整段必须是数字，不能「前缀能解析就算数」。
//	"order-0-x"     0 不是任何商家；塞进 SET LOCAL 会让 RLS 静默过滤成空结果集，
//	                症状是「查不到数据」，而真因在这里。
//	"refund-12-x"   别的业务的 gid。能解析出 12，但它不该跑到下单分支里去。
//	"order-12-"     订单号为空。
//
// 任何一条不满足，返回 (0, "", err)。**不返回半个结果**：调用方在错误分支上
// 顺手用了 merchantID，拿到的也只能是 0，而 0 会被 tenant.FromContext 挡住。
func ParseOrderGID(gid string) (merchantID int64, orderNo string, err error) {
	if len(gid) > maxGIDLen {
		return 0, "", fmt.Errorf("%w: 长度 %d 超过 %d", ErrNotOrderGID, len(gid), maxGIDLen)
	}
	rest, ok := strings.CutPrefix(gid, OrderGIDPrefix)
	if !ok {
		return 0, "", fmt.Errorf("%w: %q 没有前缀 %q", ErrNotOrderGID, gid, OrderGIDPrefix)
	}
	// 只切第一个 '-'：order_no 里允许有 '-'，租户段不允许。
	mid, no, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, "", fmt.Errorf("%w: %q 里没有租户段与订单号的分隔符", ErrNotOrderGID, gid)
	}
	if !isCanonicalPositiveDecimal(mid) {
		return 0, "", fmt.Errorf("%w: %q 的租户段是 %q，必须是无前导零的正十进制整数",
			ErrNotOrderGID, gid, mid)
	}
	v, convErr := strconv.ParseInt(mid, 10, 64)
	if convErr != nil {
		return 0, "", fmt.Errorf("%w: %q 的租户段 %q 解析失败: %v",
			ErrNotOrderGID, gid, mid, convErr)
	}
	if no == "" {
		return 0, "", fmt.Errorf("%w: %q 的订单号为空", ErrNotOrderGID, gid)
	}
	return v, no, nil
}

// isCanonicalPositiveDecimal 只认 "1".."9" 打头、其余全是 ASCII 数字的串。
//
// 不用 strconv 的宽容规则：它接受 "+12"、接受下划线分隔（在某些 base 下），
// 而我们要的是「一个租户恰好一种写法」。多一种写法就多一个绕过屏障幂等的口子。
func isCanonicalPositiveDecimal(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// TenantContextFromGID 把 gid 里的租户放进 ctx，供 repository.WithTenant 使用。
//
// 这是分支进入数据库的唯一正门：分支手里只有三个字符串，而 repository.WithTenant
// 从 ctx 取租户、自己 Begin、自己 set_config('app.merchant_id')。
// 两头一接，分支既拿不到 pgx.Tx，也没有任何机会把租户传错 ——
// 它压根没有那个参数可以传。
//
// 注意入参 ctx 该是一个**干净的** context（分支跑在任何 HTTP 请求之外，
// 本来也没有别的 ctx 可用）。就算传进来一个已经带着别家租户的 ctx，
// tenant.NewContext 也会把它覆盖掉 —— 覆盖的方向永远是「以 gid 为准」，
// 因为 gid 是协调器重放时唯一还在的东西。
func TenantContextFromGID(ctx context.Context, gid string) (context.Context, int64, string, error) {
	merchantID, orderNo, err := ParseOrderGID(gid)
	if err != nil {
		return nil, 0, "", err
	}
	return tenant.NewContext(ctx, merchantID), merchantID, orderNo, nil
}
