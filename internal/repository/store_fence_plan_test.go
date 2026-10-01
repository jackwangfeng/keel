package repository_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 围栏判定改成平面几何（ST_Intersects(st.fence::geometry, 点)，2026-10-01）之后的查询计划。
// 谓词与 stores.sql 的 ResolveStoresByFence / StoreServesPoint 逐字一致。
//
// 两件事分开钉：
//
//  1. 表达式与 00190 的 idx_stores_fence_geom 对得上：不经 RLS 的连接（运维 / 报表的路径）、关掉顺序扫，
//     （顺序扫与普通索引扫都关掉，只剩位图扫，而位图扫要有索引条件）计划走这条索引。哪天有人把谓词改回 geography、或者写成 ST_Intersects(点, fence) 以外某种
//     匹配不上表达式的形状，这里红。stores 以「几十行」计，不关顺序扫规划器本来就爱顺序扫，
//     这里钉的是「能不能」，不是代价模型。
//  2. 应用角色（NOBYPASSRLS）下走不了任何空间索引 —— PostGIS 的 && 不是 leakproof，规划器不许把它
//     排到租户谓词前面（rls_index_plans_test.go 文件头同一个机制）。改之前的 geography 索引在应用角色下
//     同样用不上（实测计划是 idx_stores_listing 的 merchant_id 条件 + Filter），所以这不是这次改动的退化。
//     钉住的是它退化成「本店几十行上的 Filter」而不是全表顺序扫：Index Cond 里有 merchant_id。
func TestPlanFenceLookup(t *testing.T) {
	a, _ := seedTwoTenants(t)
	ctx := context.Background()
	const fenceHit = `ST_Intersects(st.fence::geometry, ST_SetSRID(ST_MakePoint($1::float8, $2::float8), 4326))`
	explain := func(tx pgx.Tx, q string) (string, error) {
		for _, s := range []string{
			`SET LOCAL enable_seqscan = off`,
			`SET LOCAL enable_indexscan = off`,
			`SET LOCAL plan_cache_mode = force_generic_plan`,
			`PREPARE fence_probe AS ` + q,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return "", err
			}
		}
		defer tx.Exec(ctx, `DEALLOCATE fence_probe`)
		rows, err := tx.Query(ctx, `EXPLAIN (COSTS OFF) EXECUTE fence_probe(116.4, 39.9)`)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return "", err
			}
			b.WriteString("  " + line + "\n")
		}
		return b.String(), rows.Err()
	}

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 只留围栏谓词：stores 上还有 deleted_at IS NULL 的部分索引，全带上的话规划器在几行的表上会挑
	// 「整条扫部分索引 + Filter」，那是代价问题，不是表达式匹配不上。
	plan, err := explain(tx, `SELECT st.id FROM stores st WHERE `+fenceHit)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_stores_fence_geom") {
		t.Errorf("平面围栏判定的表达式没有匹配上 idx_stores_fence_geom：\n%s", plan)
	}

	var appPlan string
	err = repository.New(pool(t)).RawTenantTx(tenant.NewContext(ctx, a), func(tx pgx.Tx) error {
		var e error
		appPlan, e = explain(tx, `SELECT st.id FROM stores st
		  WHERE st.deleted_at IS NULL AND st.fence IS NOT NULL AND `+fenceHit)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(appPlan, "Seq Scan") || !strings.Contains(appPlan, "merchant_id = current_merchant()") {
		t.Errorf("应用角色下围栏判定应当退化成本店行上的 Filter（Index Cond 带 merchant_id），实得：\n%s", appPlan)
	}
}
