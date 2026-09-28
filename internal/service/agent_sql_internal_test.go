package service

import "testing"

func TestCheckAgentSQL(t *testing.T) {
	ok := []string{
		"select count(*) from orders",
		"WITH x AS (SELECT id FROM orders) SELECT * FROM x;",
		"select updated_at, created_at from products",
	}
	for _, q := range ok {
		if _, err := checkAgentSQL(q); err != nil {
			t.Errorf("%q 应通过：%v", q, err)
		}
	}
	bad := []string{
		"", "update orders set status = 1", "select 1; select 2",
		"select set_config('app.merchant_id', '2', true)", "SELECT pg_catalog.SET_CONFIG('role','keel_app',false)",
		"select pg_read_file('/etc/passwd')", "with d as (delete from orders returning *) select * from d",
		"select current_setting('app.merchant_id')", "explain select 1",
	}
	for _, q := range bad {
		if _, err := checkAgentSQL(q); err == nil {
			t.Errorf("%q 应被拒", q)
		}
	}
}
