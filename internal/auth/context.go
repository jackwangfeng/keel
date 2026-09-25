package auth

import (
	"context"
	"errors"
)

// ctxKey 是未导出的空结构体类型，手法与 internal/tenant 里的那个完全一样：
// context 的 key 按「类型 + 值」比较，未导出类型让本包之外无法构造出相同的
// key，也就无法从外部伪造当前用户。
type ctxKey struct{}

// ErrNoUser 表示上下文里没有当前用户。
//
// 它总是一个 bug（bearer 中间件没挂、或者某处凭空造了个 context.Background），
// 不是可恢复的业务错误。调用方该做的是让请求失败，而不是回落到某个默认用户 ——
// 那意味着匿名请求会以某个人的身份下单。理由与 tenant.ErrNoTenant 一字不差。
var ErrNoUser = errors.New("请求上下文中没有当前用户；bearer 中间件是否未挂载？")

// Identity 是「这个请求是谁发的」。
//
// 带上 SessionID 而不是只带 UserID：退出登录要吊销的是**这一个会话**，
// 而不是这个人的全部会话（手机上退出不该把平板踢下线）。
// 它不带 MerchantID —— 租户在 tenant 那个 context 里，是独立的输入，
// 复制一份过来只会制造两个可能对不上的真相。
type Identity struct {
	UserID    int64
	SessionID int64
}

// NewContext 由 bearer 中间件调用。
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext 取当前用户。取不到时返回 ErrNoUser，绝不返回零值：
// 返回 0 会被下游当成一个合法的 user_id 塞进 WHERE，而 0 号用户不存在，
// 症状是「查不到数据」，真因是中间件没挂。
func FromContext(ctx context.Context) (Identity, error) {
	v, ok := ctx.Value(ctxKey{}).(Identity)
	if !ok || v.UserID <= 0 {
		return Identity{}, ErrNoUser
	}
	return v, nil
}
