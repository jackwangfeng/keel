package dtm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Step 是 SAGA 的一步。Action / Compensate 是分支地址（local://<name> 或
// http(s)://...，用 BranchResolver.BranchURL 生成）；Compensate 可以为空（不补偿）。
//
// Payload 是这一步的业务载荷，正向与补偿共用。进程内分支经 BranchFuncEx 的
// payload 参数拿到，HTTP 分支拿到的是请求体。它会原样落进协调器的存储，
// 所以不要往里放任何秘密（口令、令牌），也不要放租户 —— 租户从 gid 推。
type Step struct {
	Action     string `json:"action"`
	Compensate string `json:"compensate,omitempty"`
	Payload    string `json:"payload,omitempty"`
}

// StepsJSON 把步骤编成 dtmrs_submit_saga 要的 JSON。
//
// 手拼 JSON 的话，载荷本身就是 JSON，要转义一层 —— 少转一层 dtmrs 会把它
// 当成步骤的字段解析，症状是提交期一句看不懂的解析错误，或者更糟，
// 载荷被截在第一个引号处。
func StepsJSON(steps ...Step) (string, error) {
	if len(steps) == 0 {
		return "", errors.New("SAGA 至少要有一步")
	}
	for i, s := range steps {
		if s.Action == "" {
			return "", fmt.Errorf("第 %d 步没有 action 地址", i+1)
		}
		steps[i].Payload = normalizePayload(s.Payload)
	}
	b, err := json.Marshal(steps)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SubmitSagaSteps 是 SubmitSaga 的结构化版本。
func (t *TC) SubmitSagaSteps(gid string, steps ...Step) error {
	s, err := StepsJSON(steps...)
	if err != nil {
		return fmt.Errorf("提交 %s 失败: %w", gid, err)
	}
	return t.SubmitSaga(gid, s)
}

// normalizePayload 让同一个 BranchFuncEx 在进程内与 HTTP 两条路径上看到同一个载荷。
//
// dtmrs 的 HTTP 驱动在载荷为空（或只有空白）时发的请求体是 "{}"
// （driver.rs branch_payload），而进程内回调拿到的是原样的空串。不归一的话，
// 同一个分支在单体里收到 ""、拆分后收到 "{}"，写分支的人得同时处理两种
// 「没有载荷」—— 而只在其中一种部署形态下才出现的分支，正是最难测的那种。
// 所以约定：空白与 "{}" 一律当作「没有载荷」，交给分支的是 ""。
// 代价是分支分不出「调用方故意传了 {}」与「没传」—— 对 JSON 载荷两者本来同义。
func normalizePayload(p string) string {
	switch strings.TrimSpace(p) {
	case "", "{}":
		return ""
	}
	return p
}
