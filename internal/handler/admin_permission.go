package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// writePermissionError 把同一租户内的分级权限拒绝（internal/service/authz.go）
// 翻成 403，翻了就返回 true。
//
// 这里**没有任何判断**，只有翻译 —— 判据全在 service 那一层
// （CONTRIBUTING 规矩一：handler 只做参数校验、鉴权中间件、响应封装）。
//
// 三个错误写函数（writeCatalogError / writeStoreError / writeStaffError）
// 都先过这一道，而不是各自多两个 case：它们翻出来的必须是同一个 type、
// 同一句 title，分在三处写的话，哪天有人改了一处，前端按 type 分支的那段
// 代码就会在一部分接口上失灵。
//
// detail 转述 service 的原话（「门店 12 所在的大区不归你管」）。那句话是人写的
// fmt，里面只有调用者自己租户里的 id，没有表名、列名或 SQL —— 与
// problem.Write 反对的「把 err.Error() 原样回给调用方」不是一回事。
// 界面把它原样显示出来，那是被拒的人唯一能拿来找对人的信息。
func writePermissionError(c *gin.Context, err error) bool {
	var kind, title string
	switch {
	case errors.Is(err, service.ErrRoleForbidden):
		kind, title = problem.TypeRoleForbidden, "你的角色不能做这件事"
	case errors.Is(err, service.ErrOutOfScope):
		kind, title = problem.TypeOutOfScope, "目标不在你的管辖范围内"
	default:
		return false
	}
	detail := err.Error()
	problem.WriteValue(c, http.StatusForbidden, api.Problem{
		Type:   kind,
		Title:  title,
		Status: http.StatusForbidden,
		Detail: &detail,
	})
	return true
}
