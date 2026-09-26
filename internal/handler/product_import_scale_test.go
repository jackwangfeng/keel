package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/catalogimport"
)

// 两条「设计上说了、要有东西盯着」的性质：
//
//   - 同一份文件的两个确认**并发**打进来（换了钥匙），只建一次 —— 后到的那条在
//     uk_product_import_batches_sha 上等前一个事务结束，然后走「已导入过」。
//   - 上限那一档（2000 行）在一个事务里跑得完。确认导入不走队列的全部理由是
//     「几秒量级」，这里把那个数量出来。

func TestImportConcurrentConfirmsOfSameFileCreateOnce(t *testing.T) {
	sh := newAdminShop(t)
	cat := namedCategory(t, sh, "杂货", nil)
	var b strings.Builder
	b.WriteString(importHeader)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "并发商品%d,,杂货,,,CC-%s-%d,1,1,,,\n", i, sh.Suffix, i)
	}
	file := []byte(b.String())

	const n = 4
	results := make([]api.ProductImportResult, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", file, "", freshIdemKey())
			codes[i] = w.Code
			if w.Code == http.StatusCreated {
				_ = json.Unmarshal(w.Body.Bytes(), &results[i])
			}
		}(i)
	}
	wg.Wait()

	created, already := 0, 0
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusCreated {
			t.Fatalf("第 %d 个并发确认：状态码 %d", i, codes[i])
		}
		if results[i].AlreadyImported {
			already++
		} else {
			created++
		}
	}
	if created != 1 || already != n-1 {
		t.Fatalf("并发确认同一份文件应当只建一次：建了 %d 次，已导入 %d 次", created, already)
	}
	if got := adminQueryInt64(t, `SELECT count(*) FROM products WHERE merchant_id = $1 AND category_id = $2`,
		sh.MerchantID, cat); got != 20 {
		t.Errorf("应当恰好 20 件商品，得到 %d", got)
	}
}

func TestImportAtTheRowLimitFitsInOneTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过 2000 行那一档")
	}
	sh := newAdminShop(t)
	namedCategory(t, sh, "杂货", nil)
	var b strings.Builder
	b.WriteString(importHeader)
	// 1000 件商品 × 2 个规格 = 2000 行，正好是上限。
	for i := 0; i < catalogimport.MaxRows/2; i++ {
		for _, size := range []string{"S", "M"} {
			fmt.Fprintf(&b, "上限商品%04d,,杂货,尺码,%s,LIM-%s-%d-%s,19.9,10,100,,\n", i, size, sh.Suffix, i, size)
		}
	}
	file := []byte(b.String())

	start := time.Now()
	pv := previewImport(t, sh, file)
	previewTook := time.Since(start)
	if pv.TotalRows != catalogimport.MaxRows || pv.ErrorRows != 0 {
		t.Fatalf("2000 行预检不对：rows=%d errors=%d", pv.TotalRows, pv.ErrorRows)
	}

	start = time.Now()
	var res api.ProductImportResult
	decodeInto(t, importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", file, "", freshIdemKey()),
		http.StatusCreated, "2000 行确认导入", &res)
	commitTook := time.Since(start)
	if res.CreatedProducts != catalogimport.MaxRows/2 || res.CreatedSkus != catalogimport.MaxRows {
		t.Fatalf("2000 行导入回执不对：%+v", res)
	}
	t.Logf("2000 行：预检 %v，确认导入（一个事务）%v", previewTook.Round(time.Millisecond), commitTook.Round(time.Millisecond))
	// 宽松的上限：本机多个 agent 并行跑测试时会慢好几倍。它挡的是「数量级不对」
	// （比如有人把某一步改成了逐行开事务），不是性能回归。
	if commitTook > 2*time.Minute {
		t.Errorf("2000 行确认导入用了 %v，一个事务的设计前提（几秒量级）不成立了", commitTook)
	}
}
