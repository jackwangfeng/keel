package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/tenant"
)

// ===========================================================================
// 平台级会话的租户切换：X-Keel-Merchant
// ===========================================================================
//
// 后台请求的租户由 Host 决定（tenant.Resolver），而后台界面只有一个源——
// 于是平台管理员永远只能管那一家。这个头让**平台级会话**说出「这一次请求
// 管的是哪家店」。
//
// ## 它为什么不违背 resolver.go 那句「刻意不支持用请求头指定租户」
//
// 那句话的理由是「公开接口没有鉴权，那等于让调用方自己声明它是哪家店」。
// 这里读这个头的前提恰恰是鉴权：只在 StaffBearer 已经验过签名、查过会话、
// 确认这个人在岗**之后**，而且只对**平台级**身份生效——平台级操作员按定义
// 就能跨租户运维（数据模型 §14）。解析器本身一个字没改，它照旧只看配置与 Host。
//
// ## 四条规则，每一条都有一个行为测试钉着（internal/handler/tenant_switch_test.go）
//
//  1. **没带这个头**：什么都不做，租户就是 Host 解析出来的那家。
//  2. **平台级带了头**：按 code 查出商家（含停用，不含软删），用它**替换** ctx 里的
//     租户。之后 repository.WithTenant 的 SET LOCAL app.merchant_id 就落在这家店上，
//     行级安全照旧兜底。
//  3. **商家级带了头**：403 tenant-switch-forbidden。**不生效，也不静默忽略。**
//     忽略的话，一个以为自己切过去了的客户端会往自己的店里写本该写给别家的数据，
//     而那不会报任何错——显式拒绝才让误用被看见。
//  4. **code 不存在 / 已软删 / 头是空的 / 头出现了不止一次**：422 unknown-merchant，
//     **不回落**到 Host 那家。回落是最危险的一种失败：运营以为在管 B 店，
//     实际改的是 A 店。
//
// 停用的店可以切进去（要进得去才修得好、再启用），所以这里用的是
// Resolver.ByCodeForPlatform 而不是买家侧那条只认 status = 1 的解析。
//
// ## 为什么在 StaffBearer 里，而不是一道单独的中间件
//
// 规则 3 必须对**每一条**挂后台会话的路由成立。做成单独的中间件，哪条路由漏挂了
// 它，商家级员工带的头就会在那条路由上被静默忽略——恰好是规则 3 要消灭的行为。
// 放在 StaffBearer 里，「这条路由鉴了后台身份」与「这条路由处理了这个头」
// 就是同一件事，没有第二个可以漏的地方。

// MerchantSwitchHeader 是平台级会话切换租户用的请求头。契约里叫 KeelMerchant。
const MerchantSwitchHeader = "X-Keel-Merchant"

// MerchantDirectory 是租户切换需要的唯一一次查库。tenant.Resolver 实现它。
//
// 与 StaffSessionLoader 同一个思路：中间件只拿得到它真正要的那一个方法。
type MerchantDirectory interface {
	// ByCodeForPlatform 按 code 找一家未软删的商家（含停用）。
	// 找不到返回 tenant.ErrNoMerchant。
	ByCodeForPlatform(ctx context.Context, code string) (int64, error)
}

// applyPlatformTenantSwitch 在 StaffBearer 把身份放进 ctx 之后调用。
// 返回 false 表示已经写了错误响应并中止，调用方直接 return。
func applyPlatformTenantSwitch(c *gin.Context, dir MerchantDirectory, id StaffIdentity, log *slog.Logger) bool {
	values, present := c.Request.Header[textproto.CanonicalMIMEHeaderKey(MerchantSwitchHeader)]
	if !present {
		return true // 规则 1
	}
	ctx := c.Request.Context()

	if !id.Platform() {
		// 规则 3。留一条日志：一个商家级客户端在带这个头，要么是界面写错了，
		// 要么是有人在试。两种都值得被看见，而 403 本身在运维那里不可观测。
		log.WarnContext(ctx, "商家级后台会话带了租户切换头，拒绝",
			"staff_id", id.StaffID, "path", c.Request.URL.Path)
		problem.Write(c, http.StatusForbidden, problem.TypeTenantSwitchForbidden,
			"只有平台级操作员能切换要管理的商家；商家级会话不要带 "+MerchantSwitchHeader+" 头")
		return false
	}

	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		// 空值或多个值：意图说不清。当作「没带」就是回落到 Host 那家，
		// 那正是规则 4 禁止的。
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeUnknownMerchant,
			MerchantSwitchHeader+" 头必须恰好出现一次，值是商家的 code")
		return false
	}
	code := strings.TrimSpace(values[0])

	if dir == nil {
		// 装配 bug：StaffBearer 没拿到商家目录。报成 422 会把它伪装成客户端的错。
		_ = c.Error(errors.New("StaffBearer 没有装配 MerchantDirectory，无法处理租户切换"))
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
		return false
	}

	merchantID, err := dir.ByCodeForPlatform(ctx, code)
	switch {
	case err == nil:
	case errors.Is(err, tenant.ErrNoMerchant):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeUnknownMerchant,
			"找不到 code 为 "+code+" 的商家（或它已被删除）；不会回落到当前域名对应的那家店")
		return false
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
		return false
	}

	// 规则 2：替换，不是叠加。之后所有 tenant.FromContext 读到的都是这家。
	c.Request = c.Request.WithContext(context.WithValue(tenant.NewContext(ctx, merchantID), merchantSwitchedKey{}, true))
	return true
}

type merchantSwitchedKey struct{}

// MerchantSwitched 回答这一次请求是不是平台级会话经 X-Keel-Merchant 切进了某家店。
// 员工管理据此决定落在哪个作用域：切进来了就是管这家店的员工（与切进来之后管商品、券同一个道理），
// 没切就是管平台级员工（service/staff.go 的 staffPlatformScope）。
func MerchantSwitched(ctx context.Context) bool {
	v, _ := ctx.Value(merchantSwitchedKey{}).(bool)
	return v
}
