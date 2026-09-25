// Package tenant 承载「当前请求属于哪个商家」。
//
// 租户只经 context 传递，业务函数签名里没有 merchantID 参数 ——
// 于是「传错租户」这个动作在类型层面就不存在，不需要靠 code review 去盯。
// 写入由租户解析中间件完成，读取由 repository.WithTenant 完成。
package tenant

import (
	"context"
	"errors"
)

// ctxKey 是未导出的空结构体类型。
//
// 用它而不是字符串常量：context 的 key 按「类型 + 值」比较，未导出类型
// 让本包之外无法构造出相同的 key，也就无法从外部伪造或覆盖租户。
type ctxKey struct{}

// ErrNoTenant 表示请求上下文里没有租户。
//
// 这总是一个 bug（中间件没挂、或者某处凭空造了个 context.Background），
// 不是可恢复的业务错误。调用方该做的是让请求失败，而不是回落到某个默认租户 ——
// 回落意味着随便谁都能读到那个租户的数据。
var ErrNoTenant = errors.New("请求上下文中没有租户；中间件是否未挂载？")

// NewContext 由租户解析中间件调用，把商家 ID 放进 ctx。
func NewContext(ctx context.Context, merchantID int64) context.Context {
	return context.WithValue(ctx, ctxKey{}, merchantID)
}

// FromContext 取当前租户。
//
// 取不到时返回 ErrNoTenant，绝不返回零值：返回 0 会被下游当成一个合法的
// merchant_id 塞进 SET LOCAL，RLS 于是安静地过滤出空结果集 ——
// 一个「查不到数据」的故障，而真因是中间件没挂。
func FromContext(ctx context.Context) (int64, error) {
	v, ok := ctx.Value(ctxKey{}).(int64)
	if !ok || v <= 0 {
		return 0, ErrNoTenant
	}
	return v, nil
}
