package db_test

import (
	"os"
	"testing"

	"github.com/keel/keel/internal/testdb"
)

// TestMain 给本包一个只属于它的库（keel_test_db），迁移一次存成模板，
// 之后每条测试经 migrate(t) 从模板克隆一个新库。见 internal/testdb。
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, testdb.Package{Name: "db", Template: true}))
}
