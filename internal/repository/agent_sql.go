package repository

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 只读 SQL 工具的执行（00131，service/agent_sql.go）。闸门与理由写在迁移 00131 的文件头。

var (
	// ErrAgentSQLTenantChanged：查询里改了租户设置 —— 结果已丢弃。
	ErrAgentSQLTenantChanged = errors.New("查询改动了租户上下文，结果已丢弃")
	// ErrAgentSQLUnavailable：这个事务没有底层连接（不是经 WithTenant 开的）。
	ErrAgentSQLUnavailable = errors.New("只读 SQL 只能在租户事务里跑")
)

// AgentSQLResult 是一次只读查询的结果：列名、行（值已是 JSON 友好的 Go 值）、是否截断。
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
	for rows.Next() {
		if len(out.Rows) >= maxRows {
			out.Truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			rows.Close()
			return AgentSQLResult{}, err
		}
		out.Rows = append(out.Rows, vals)
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
