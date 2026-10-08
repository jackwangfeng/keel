package db_test

import (
	"context"
	"slices"
	"testing"
)

func TestAppRoleGrantSurface(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	const appRole = "keel_app"
	all := []string{"SELECT", "INSERT", "UPDATE", "DELETE"}

	for _, tbl := range allTables(t, conn) {
		want := man.grants(tbl)
		var got []string
		for _, priv := range all {
			var ok bool
			if err := conn.QueryRow(context.Background(),
				`SELECT has_table_privilege($1, $2, $3)`, appRole, tbl, priv).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				got = append(got, priv)
			}
		}
		slices.Sort(got)
		w := slices.Clone(want)
		slices.Sort(w)
		if !slices.Equal(got, w) {
			_, entry := man.class(tbl)
			cls := entry.Class
			if cls == "" {
				cls = "tenant（未登记，按默认类别）"
			}
			t.Errorf("%s 上 %s 的权限是 %v，清单说应该是 %v（类别 %s）——"+
				"多给的每一项都是一条可写的路径，少给的每一项都是一次运行期 42501",
				tbl, appRole, got, w, cls)
		}
	}
}

// 带 updated_at 的表必须挂上 touch_updated_at 触发器。
//
// 在 00007 之前，七张带这一列的表都只有 DEFAULT now()：那一列记的是**创建
// 时间**，改一行不会动它。这种错不报警，只会让「这条记录最后什么时候变过」
// 在半年后得到一个自信而错误的答案。
//
// 清单从系统目录枚举，判据是「有没有 updated_at 这一列」——不需要豁免机制，
// 因为没有这一列的表天然不在范围内。新表加了这一列却忘了挂触发器就会红。
