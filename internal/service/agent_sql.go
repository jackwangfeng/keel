package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 只读 SQL 工具（AI 经营 M11，docs/AI经营-M10M11设计.md §7，00131）：MCP 工具 query_sql。
//
// 三道闸的前两道在库里（角色 keel_agent_ro 只对 schema agent_ro 的视图有 SELECT；视图只放经营数据、显式按租户过滤），
// 第三道在这里：静态拒掉能改会话设置、读服务器文件、连外部库的函数，只接受一条 SELECT / WITH；执行后复核租户设置
// （repository.AgentReadOnlyQuery）。视图覆盖全店，所以只给全店范围的 AI 员工。

const (
	agentSQLMaxRows  = 500
	agentSQLTimeout  = 3 * time.Second
	agentSQLMaxBytes = 8000
)

// ErrAgentSQLRejected：SQL 没过静态检查，或执行出错（语法、超时、没权限）。契约 422，detail 说明原因。
var ErrAgentSQLRejected = errors.New("只读 SQL 被拒绝")

var (
	agentSQLDenied = regexp.MustCompile(`(?i)(set_config|pg_read|pg_ls|pg_stat_file|lo_|dblink|pg_terminate|pg_cancel|` +
		`pg_reload|pg_rotate|pg_promote|pg_advisory|pg_notify|txid_|current_setting)`)
	agentSQLWrite = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|truncate|alter|drop|create|grant|revoke|` +
		`call|do|copy|vacuum|analyze|reset|set|lock|listen|notify|prepare|execute|refresh|comment|security)\b`)
	agentSQLStart = regexp.MustCompile(`(?is)^\s*(select|with)\b`)
)

// checkAgentSQL 是静态那一道。不追求完备（完备的是库里的角色），只挡住明面上的东西、给出能看懂的原因。
func checkAgentSQL(sql string) (string, error) {
	q := strings.TrimSpace(sql)
	q = strings.TrimSuffix(q, ";")
	switch {
	case q == "":
		return "", fmt.Errorf("%w: sql 为空", ErrAgentSQLRejected)
	case len(q) > agentSQLMaxBytes:
		return "", fmt.Errorf("%w: sql 至多 %d 字节", ErrAgentSQLRejected, agentSQLMaxBytes)
	case strings.Contains(q, ";"):
		return "", fmt.Errorf("%w: 只能是一条语句", ErrAgentSQLRejected)
	case !agentSQLStart.MatchString(q):
		return "", fmt.Errorf("%w: 只接受 SELECT / WITH 开头的查询", ErrAgentSQLRejected)
	}
	if m := agentSQLDenied.FindString(q); m != "" {
		return "", fmt.Errorf("%w: 不允许调用 %s", ErrAgentSQLRejected, m)
	}
	if m := agentSQLWrite.FindString(q); m != "" {
		return "", fmt.Errorf("%w: 只读查询里不能出现 %s", ErrAgentSQLRejected, strings.ToUpper(m))
	}
	return q, nil
}

// AgentSQLResult 是 query_sql 的返回。
type AgentSQLResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
}

// QuerySQL 是 MCP 工具 query_sql。
func (s *AgentProposalService) QuerySQL(ctx context.Context, sql string) (AgentSQLResult, error) {
	if _, err := requireAgent(ctx); err != nil {
		return AgentSQLResult{}, err
	}
	if _, err := requireMerchantWide(ctx); err != nil {
		return AgentSQLResult{}, err
	}
	q, err := checkAgentSQL(sql)
	if err != nil {
		return AgentSQLResult{}, err
	}
	var res repository.AgentSQLResult
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		res, e = tx.AgentReadOnlyQuery(ctx, q, agentSQLMaxRows, agentSQLTimeout)
		return e
	})
	if err != nil {
		if errors.Is(err, repository.ErrAgentSQLTenantChanged) || errors.Is(err, repository.ErrAgentSQLUnavailable) {
			return AgentSQLResult{}, fmt.Errorf("%w: %v", ErrAgentSQLRejected, err)
		}
		// 语法错、列不存在、没权限（表不在 agent_ro 里）、超时：都是查询本身的问题，原因给 agent 看。
		return AgentSQLResult{}, fmt.Errorf("%w: %v", ErrAgentSQLRejected, err)
	}
	out := AgentSQLResult{Columns: res.Columns, Rows: make([][]any, 0, len(res.Rows)), Truncated: res.Truncated}
	if out.Columns == nil {
		out.Columns = []string{}
	}
	for _, r := range res.Rows {
		row := make([]any, len(r))
		for i, v := range r {
			row[i] = jsonSafe(v)
		}
		out.Rows = append(out.Rows, row)
	}
	out.RowCount = len(out.Rows)
	return out, nil
}

// jsonSafe 把 pgx 解出来的值变成能进 JSON 的：能直接 Marshal 的原样，别的（numeric、区间、字节）转成字符串。
func jsonSafe(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int16, int32, int64, int, float32, float64, time.Time:
		return x
	case []byte:
		return string(x)
	}
	if b, err := json.Marshal(v); err == nil {
		var back any
		if json.Unmarshal(b, &back) == nil {
			return back
		}
	}
	return fmt.Sprint(v)
}
