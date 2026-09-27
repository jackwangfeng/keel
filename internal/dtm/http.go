package dtm

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/rpc"
)

// maxBranchBody 是 HTTP 分支请求体（即步骤载荷）的上限。载荷是编排方拼的
// 结构化 JSON，64 KiB 远超任何一步的需要。
const maxBranchBody = 64 << 10

// HTTPBranch 把一个 BranchFuncEx 变成 dtmrs 远程调用能打的 HTTP 端点。
//
// 协议照 dtmrs 的 HTTP 驱动（driver.rs call_http + dtmrs-core BranchResult::from_http）：
//
//	请求：POST <url>?gid=&trans_type=&branch_id=&op=，正文是步骤载荷，没有载荷时是 "{}"
//	判定：**先看正文**，含 "FAILURE" 即失败、含 "ONGOING" 即进行中；再看状态码，
//	      2xx 成功、409 失败、425 进行中，其余（含 5xx、超时、连不上）一律 Unknown 重试
//
// 所以这里的三种回法是：
//
//	Success → 200 {"dtm_result":"SUCCESS"}
//	Failure → 409 {"dtm_result":"FAILURE"}（状态码与正文双保险）
//	其余    → 500 {"dtm_result":"UNKNOWN"}
//
// 「先看正文」那条是这个适配器里最要紧的一件事：任何一个 Unknown 路径上的响应
// （参数缺失、panic、准入失败）正文里都**不能**出现 FAILURE 这个词 —— 否则一个
// 配错的请求会被协调器当成业务明确失败，触发全局补偿。所以错误响应用的是中文
// 标题的 Problem，测试里对这一点单独断言。
//
// 准入（分支令牌）不在这里做，由挂载它的分组负责（rpc.Routes.Saga）。
func HTTPBranch(fn BranchFuncEx) gin.HandlerFunc {
	return func(c *gin.Context) {
		gid, branchID, op := c.Query("gid"), c.Query("branch_id"), c.Query("op")
		if gid == "" || branchID == "" || (op != "action" && op != "compensate") {
			// 不是协调器发来的形状。400 在 dtmrs 那里是 Unknown：真是协调器发的
			// （比如将来的版本改了参数名），它会重试而不是回滚，同时这条日志会一直响。
			slog.ErrorContext(c.Request.Context(), "SAGA 分支请求缺少参数",
				"path", c.Request.URL.Path, "gid", gid, "branch_id", branchID, "op", op)
			c.JSON(http.StatusBadRequest, gin.H{"dtm_result": "UNKNOWN", "title": "分支请求缺少 gid / branch_id / op"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBranchBody))
		if err != nil {
			slog.ErrorContext(c.Request.Context(), "读取 SAGA 分支载荷失败", "gid", gid, "err", err)
			c.JSON(http.StatusBadRequest, gin.H{"dtm_result": "UNKNOWN", "title": "分支载荷读取失败"})
			return
		}

		ret := callBranch(fn, gid, branchID, op, normalizePayload(string(body)))
		switch ret {
		case Success:
			c.JSON(http.StatusOK, gin.H{"dtm_result": "SUCCESS"})
		case Failure:
			c.JSON(http.StatusConflict, gin.H{"dtm_result": "FAILURE"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"dtm_result": "UNKNOWN"})
		}
	}
}

// callBranch 调分支并拦下 panic，按 Unknown 处理 —— 理由同 goBranchHandler：
// panic 之后「业务做没做」没有答案，只能让协调器重试、由子事务屏障挡重复。
// 不靠 gin.Recovery：它回的 500 正文是空的，倒也不会误判，但这里要的是一条
// 带 gid 的日志，而不是一段没有上下文的栈。
func callBranch(fn BranchFuncEx, gid, branchID, op, payload string) (ret int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("HTTP 事务分支 panic，按 Unknown 上报以便协调器重试",
				"gid", gid, "branch_id", branchID, "op", op, "panic", r)
			ret = Unknown
		}
	}()
	return fn(gid, branchID, op, payload)
}

// branchName 是分支名的形状。它会出现在 URL 路径里（/internal/v1/saga/<name>），
// 也是 local://<name> 的那个 name；限定成小写标识符，两处都不用转义。
var branchName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// MountBranches 把一组分支挂到 g 上（通常是 rpc.Routes.Saga），每个分支一条
// POST /<name>。名字不合形状时 panic —— 那是启动期的编程错误。
func MountBranches(g *gin.RouterGroup, branches map[string]BranchFuncEx) {
	for name, fn := range branches {
		if !branchName.MatchString(name) {
			panic("dtm.MountBranches: 分支名 " + name + " 不合形状（小写字母开头，只含小写字母、数字、下划线）")
		}
		g.POST("/"+name, HTTPBranch(fn))
	}
}

// BranchResolver 决定编排里一个分支的地址：进程内（local://<name>）还是远端
// （<KEEL_INVENTORY_URL>/internal/v1/saga/<name>?bt=<令牌>）。
//
// 这是「一套代码，两种部署」在 SAGA 这一侧的全部差异：分支体是同一个函数，
// 编排是同一份步骤，只有地址由配置决定。零值是全部进程内的解析器。
//
// 阶段 0 只有这个类型本身；下单 SAGA 在阶段 1 才用它生成库存分支的地址。
type BranchResolver struct {
	base  string // 远端地址 + SagaPrefix；空 = 进程内
	token string
}

// NewBranchResolver 按 KEEL_INVENTORY_URL / KEEL_INTERNAL_SECRET 建解析器。
// remoteURL 为空时返回进程内解析器（secret 被忽略）。
func NewBranchResolver(remoteURL, secret string) (BranchResolver, error) {
	remoteURL = strings.TrimRight(strings.TrimSpace(remoteURL), "/")
	if remoteURL == "" {
		return BranchResolver{}, nil
	}
	u, err := url.Parse(remoteURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return BranchResolver{}, errInvalidRemote(remoteURL)
	}
	if len(secret) < rpc.MinSecretLen {
		return BranchResolver{}, errNoSecret
	}
	return BranchResolver{base: remoteURL + rpc.SagaPrefix, token: rpc.BranchToken(secret)}, nil
}

var errNoSecret = fmt.Errorf("配了 %s 却没有配 %s（至少 %d 字节）：远端分支地址要带由它派生的令牌",
	rpc.EnvInventoryURL, rpc.EnvInternalSecret, rpc.MinSecretLen)

func errInvalidRemote(s string) error {
	return fmt.Errorf("%s=%q 不是 http(s)://host[:port] 形式（不带 query）", rpc.EnvInventoryURL, s)
}

// Remote 报告分支是否在远端。
func (r BranchResolver) Remote() bool { return r.base != "" }

// BranchURL 返回名为 name 的分支地址。
func (r BranchResolver) BranchURL(name string) string {
	if r.base == "" {
		return "local://" + name
	}
	return r.base + "/" + name + "?bt=" + url.QueryEscape(r.token)
}
