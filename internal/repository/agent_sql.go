package repository

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// 只读 SQL 工具的执行（00131，service/agent_sql.go）。闸门与理由写在迁移 00131 的文件头。

var (
	// ErrAgentSQLTenantChanged：查询里改了租户设置 —— 结果已丢弃。
	ErrAgentSQLTenantChanged = errors.New("查询改动了租户上下文，结果已丢弃")
	// ErrAgentSQLUnavailable：这个事务没有底层连接（不是经 WithTenant 开的）。
	ErrAgentSQLUnavailable = errors.New("只读 SQL 只能在租户事务里跑")
)

// AgentSQLResult 是一次只读查询的结果：列名、行（值已是 JSON 友好的 Go 值）、是否截断。
// AgentSQLMaxBytes 是只读 SQL 一次结果的原始字节预算（1MB），AgentSQLMaxValueBytes 是单个文本值的上限（4KB）。
// 给 AI 员工看的是统计结果，不是导数据；超了就截断、Truncated = true。
const (
	AgentSQLMaxBytes      = 1 << 20
	AgentSQLMaxValueBytes = 4 << 10
)

// truncUTF8 按字节截短，不切断一个 UTF-8 字符。
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type AgentSQLResult struct {
	Columns   []string
	Rows      [][]any
	Truncated bool
}

// AgentSQLTx 是只读 SQL 这一面。
type AgentSQLTx interface {
	AgentReadOnlyQuery(ctx context.Context, sql string, maxRows int, timeout time.Duration) (AgentSQLResult, error)
}

func (t tenantTx) AgentReadOnlyQuery(ctx context.Context, sql string, maxRows int, timeout time.Duration) (AgentSQLResult, error) {
	if t.raw == nil {
		return AgentSQLResult{}, ErrAgentSQLUnavailable
	}
	guc := func() (string, error) {
		var m, p string
		err := t.raw.QueryRow(ctx, `SELECT COALESCE(current_setting('app.merchant_id', true), ''),
		                                   COALESCE(current_setting('app.platform_scope', true), '')`).Scan(&m, &p)
		return m + "|" + p, err
	}
	before, err := guc()
	if err != nil {
		return AgentSQLResult{}, err
	}
	for _, stmt := range []string{
		fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds()),
		"SET LOCAL search_path = agent_ro, pg_catalog",
		"SET LOCAL ROLE keel_agent_ro",
	} {
		if _, err := t.raw.Exec(ctx, stmt); err != nil {
			return AgentSQLResult{}, err
		}
	}
	// 扩展协议（带一个空参数表）：服务端只接受一条语句。
	rows, err := t.raw.Query(ctx, sql)
	if err != nil {
		return AgentSQLResult{}, err
	}
	var out AgentSQLResult
	for _, f := range rows.FieldDescriptions() {
		out.Columns = append(out.Columns, f.Name)
	}
	budget := AgentSQLMaxBytes
	for rows.Next() {
		if len(out.Rows) >= maxRows {
			out.Truncated = true
			break
		}
		// 字节预算：行数上限挡不住「一行很宽」（2026-09-28 破坏性测试：repeat('x', 1e6) × 200 行，
		// 响应 400MB、13.7 秒）。按线上原始字节累计，超了就截断；单个值再按 AgentSQLMaxValueBytes 截短。
		for _, raw := range rows.RawValues() {
			budget -= len(raw)
		}
		if budget < 0 && len(out.Rows) > 0 {
			out.Truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			rows.Close()
			return AgentSQLResult{}, err
		}
		for i, v := range vals {
			if str, ok := v.(string); ok && len(str) > AgentSQLMaxValueBytes {
				vals[i] = truncUTF8(str, AgentSQLMaxValueBytes) + "…（已截断）"
				out.Truncated = true
			}
		}
		out.Rows = append(out.Rows, vals)
		if budget < 0 {
			out.Truncated = true
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return AgentSQLResult{}, err
	}
	if _, err := t.raw.Exec(ctx, "RESET ROLE"); err != nil {
		return AgentSQLResult{}, err
	}
	after, err := guc()
	if err != nil {
		return AgentSQLResult{}, err
	}
	if after != before {
		return AgentSQLResult{}, ErrAgentSQLTenantChanged
	}
	return out, nil
}
