package repository_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

var (
	_ service.RetentionRepository = (*repository.Repo)(nil)
	_ service.InventoryLogPurger  = (*repository.InventoryStore)(nil)
)

// 保留期清理（service/retention.go）对着真库跑一轮：两家店都删到、过了保留期的删、没过的留、
// 平台作用域那一抽屉的幂等存档也删；批大小取 3 让「一批删不完、接着下一批」真的发生，
// MaxBatches 取 2 让「删满上限、留给下一轮」也发生一次。
func TestRetentionPurgesAcrossTenants(t *testing.T) {
	ctx := context.Background()
	idA, idB := seedTwoTenants(t)
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	tag := fmt.Sprintf("ret-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		c := context.Background()
		for _, stmt := range []string{
			`DELETE FROM search_logs      WHERE merchant_id = ANY($1)`,
			`DELETE FROM agent_tool_calls WHERE merchant_id = ANY($1)`,
			`DELETE FROM inventory_logs   WHERE merchant_id = ANY($1)`,
			`DELETE FROM idempotency_keys WHERE merchant_id = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, []int64{idA, idB}); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
		if _, err := admin.Exec(c, `DELETE FROM idempotency_keys WHERE merchant_id IS NULL AND idem_key LIKE $1`,
			tag+"%"); err != nil {
			t.Errorf("清理平台幂等存档失败: %v", err)
		}
	})

	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-200 * 24 * time.Hour) // 过了全部保留期
	mid := now.Add(-100 * 24 * time.Hour) // 过了检索日志的 90 天，没过另外两张的 180 天
	fresh := now.Add(-time.Hour)

	// 夹具走管理员连接，session_replication_role = replica 跳过外键：
	// agent_tool_calls 挂着 staff / agent_keys 两条外键，这条测试不关心它们。
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s\n%v", sql, err)
		}
	}
	exec(`SET LOCAL session_replication_role = replica`)
	for _, m := range []int64{idA, idB} {
		for i, at := range []time.Time{old, old, old, old, old, mid, mid, fresh} {
			exec(`INSERT INTO search_logs (merchant_id, query, trace_id, strategy, stages, created_at)
			      VALUES ($1, 'q', $2, 'keyword', '{}', $3)`, m, fmt.Sprintf("%s-%d-%d", tag, m, i), at)
			exec(`INSERT INTO agent_tool_calls (merchant_id, agent_staff_id, key_id, tool, ok, duration_ms, created_at)
			      VALUES ($1, 1, 1, 't', true, 1, $2)`, m, at)
			exec(`INSERT INTO inventory_logs (merchant_id, sku_id, store_id, change_qty, biz_type, biz_id,
			                                  before_available, after_available, created_at)
			      VALUES ($1, 1, 1, 1, 1, $2, 0, 1, $3)`, m, fmt.Sprintf("%s-%d", tag, i), at)
		}
		for i, exp := range []time.Time{old, fresh.Add(2 * time.Hour)} { // 一条过期、一条没过期
			exec(`INSERT INTO idempotency_keys (merchant_id, scope, subject_kind, subject_id, idem_key,
			                                    request_hash, expire_at)
			      VALUES ($1, 'test', 1, 1, $2, 'h', $3)`, m, fmt.Sprintf("%s-%d-%d", tag, m, i), exp)
		}
	}
	exec(`INSERT INTO idempotency_keys (merchant_id, scope, subject_kind, subject_id, idem_key, request_hash, expire_at)
	      VALUES (NULL, 'test', 2, 1, $1, 'h', $2), (NULL, 'test', 2, 1, $3, 'h', $4)`,
		tag+"-p0", old, tag+"-p1", fresh.Add(2*time.Hour))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	p := pool(t)
	r := repository.New(p)
	cfg := service.DefaultRetentionConfig()
	cfg.Batch = 3
	cfg.MaxBatches = 2
	svc := service.NewRetentionService(r, repository.NewInventoryStore(p), cfg, nil).
		WithClock(func() time.Time { return now })

	count := func(table string, m int64) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE merchant_id = $1`, m).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 第一轮：search_logs 每店 7 行该删（5 old + 2 mid），批 3 × 上限 2 = 6，删 6 行、留 1 行给下一轮。
	rep, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 0 {
		t.Fatalf("有失败：%+v", rep)
	}
	if rep.Capped == 0 {
		t.Errorf("批 3 × 上限 2 删不完 7 行，应当记一次 Capped：%+v", rep)
	}
	for _, m := range []int64{idA, idB} {
		if got := count("search_logs", m); got != 2 {
			t.Errorf("商家 %d 第一轮后 search_logs 剩 %d，期望 2（删满上限，下一轮接着删）", m, got)
		}
	}

	// 第二轮删完。
	if _, err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, m := range []int64{idA, idB} {
		if got := count("search_logs", m); got != 1 {
			t.Errorf("商家 %d: search_logs 剩 %d，期望只剩 1 条新的（90 天外的 7 条都删）", m, got)
		}
		if got := count("agent_tool_calls", m); got != 3 {
			t.Errorf("商家 %d: agent_tool_calls 剩 %d，期望 3（180 天内的 2 条 mid + 1 条新的）", m, got)
		}
		if got := count("inventory_logs", m); got != 3 {
			t.Errorf("商家 %d: inventory_logs 剩 %d，期望 3", m, got)
		}
		if got := count("idempotency_keys", m); got != 1 {
			t.Errorf("商家 %d: idempotency_keys 剩 %d，期望 1（没过期的那条）", m, got)
		}
	}
	var platform int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys
	    WHERE merchant_id IS NULL AND idem_key LIKE $1`, tag+"%").Scan(&platform); err != nil {
		t.Fatal(err)
	}
	if platform != 1 {
		t.Errorf("平台作用域的幂等存档剩 %d，期望 1（过期那条删掉、没过期的留着）", platform)
	}
}

// 环境变量：写错报错、0 = 不清、库存流水不许短于 30 天。
func TestRetentionConfigFromEnv(t *testing.T) {
	t.Setenv(service.EnvRetentionSearchLogDays, "0")
	t.Setenv(service.EnvRetentionInventoryLogDays, "365")
	cfg, err := service.RetentionConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SearchLogs != 0 || cfg.InventoryLogs != 365*24*time.Hour || cfg.AgentToolCalls != 180*24*time.Hour {
		t.Errorf("解析结果不对：%+v", cfg)
	}
	for env, bad := range map[string]string{
		service.EnvRetentionSearchLogDays:    "90d",
		service.EnvRetentionInventoryLogDays: "7",
		service.EnvRetentionInterval:         "-1h",
	} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, bad)
			if _, err := service.RetentionConfigFromEnv(); err == nil {
				t.Errorf("%s=%s 应当报错", env, bad)
			}
		})
	}
}
