package handler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 孤儿上传文件回收（service/upload_gc.go，数据模型 §13 的 24 小时规则）。

func newUploadGC() *service.UploadGCService {
	return service.NewUploadGCService(repository.New(testPool), service.NewLocalDiskStore(testUploadRoot),
		service.SweepConfig{}, nil)
}

// ageUpload 把一条上传记录的创建时间拨到 hours 小时之前。
func ageUpload(t *testing.T, id int64, hours int) {
	t.Helper()
	adminExec(t, `UPDATE uploads SET created_at = now() - make_interval(hours => $2) WHERE id = $1`, id, hours)
}

// uploadFileExists 看这条记录对应的文件还在不在磁盘上。记录已经删了的，用调用方先前记下的 key。
func uploadKey(t *testing.T, id int64) string {
	t.Helper()
	var key string
	if err := admin(t).QueryRow(context.Background(), `SELECT storage_key FROM uploads WHERE id = $1`, id).Scan(&key); err != nil {
		t.Fatalf("读 upload %d 的 storage_key: %v", id, err)
	}
	return key
}

func fileExists(t *testing.T, key string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(testUploadRoot, key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func uploadRowExists(t *testing.T, id int64) bool {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM uploads WHERE id = $1`, id) == 1
}

// 只收「没被引用、创建超过 24 小时」的：买家凭证与后台商品图一样收；不到 24 小时的、
// 被售后单引用了的留着。记录与文件一起删。
func TestUploadGCCollectsOnlyOldOrphans(t *testing.T) {
	cs := uploadShop(t)
	b := cs.newBuyer(t, "gc")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines := cs.lines(t, b, o.OrderNo)

	oldOrphan := mustBuyerUpload(t, cs, b, 3, []byte("传了没提交 "+cs.Suffix))
	young := mustBuyerUpload(t, cs, b, 3, []byte("刚传的 "+cs.Suffix))
	used := mustBuyerUpload(t, cs, b, 3, []byte("提交了的 "+cs.Suffix))
	staffOrphan := seedUpload(t, cs.adminShop, []byte("没挂上商品的图 "+cs.Suffix))
	cs.mustApply(t, b, o.OrderNo, fmt.Sprintf(
		`{"items":[{"order_item_id":%d,"quantity":1}],"refund_type":1,"reason_code":3,"evidence_urls":[%q]}`,
		lines[cs.DressSKU].Id, used.Url))

	ageUpload(t, oldOrphan.Id, 25)
	ageUpload(t, young.Id, 23)
	ageUpload(t, used.Id, 72)
	ageUpload(t, staffOrphan.Id, 25)
	keys := map[int64]string{}
	for _, id := range []int64{oldOrphan.Id, young.Id, used.Id, staffOrphan.Id} {
		keys[id] = uploadKey(t, id)
		if !fileExists(t, keys[id]) {
			t.Fatalf("upload %d 的文件在回收之前就不在磁盘上", id)
		}
	}

	rep, err := newUploadGC().CollectOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted < 2 || rep.Failed != 0 || rep.FileErrors != 0 {
		t.Fatalf("这一轮的报告是 %+v，期望至少删 2 条、没有失败", rep)
	}
	for _, id := range []int64{oldOrphan.Id, staffOrphan.Id} {
		if uploadRowExists(t, id) || fileExists(t, keys[id]) {
			t.Errorf("创建 25 小时、没被引用的 upload %d 没被回收（记录 %v，文件 %v）",
				id, uploadRowExists(t, id), fileExists(t, keys[id]))
		}
	}
	for name, id := range map[string]int64{"不到 24 小时的": young.Id, "被售后单引用的": used.Id} {
		if !uploadRowExists(t, id) || !fileExists(t, keys[id]) {
			t.Errorf("%s upload %d 被回收了", name, id)
		}
	}

	// 回收掉的凭证再拿去申请售后：422，不是 500，也不会落一张指着 404 图片的申请。
	o2 := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines2 := cs.lines(t, b, o2.OrderNo)
	if p := problemOf(t, applyRefund(t, cs.Host, o2.OrderNo, b.Token, fmt.Sprintf(
		`{"items":[{"order_item_id":%d,"quantity":1}],"refund_type":1,"reason_code":3,"evidence_urls":[%q]}`,
		lines2[cs.DressSKU].Id, oldOrphan.Url), "ev-"+uniqueKey()), http.StatusUnprocessableEntity); p.Status != 422 {
		t.Fatalf("拿已回收的凭证申请售后应 422，实得 %+v", p)
	}
}

// 与「提交售后时标记引用」赛跑：标引用的事务先拿到行锁、还没提交时，回收那条 DELETE 必须
// 等它，然后按新版本重判 —— 已被引用，不删。只按 id 删（谓词里没有 NOT referenced）的话，
// 这里会删掉一张刚被引用的凭证。
func TestUploadGCLosesTheRaceToAReference(t *testing.T) {
	cs := uploadShop(t)
	b := cs.newBuyer(t, "gc-race")
	up := mustBuyerUpload(t, cs, b, 3, []byte("赛跑的凭证 "+cs.Suffix))
	ageUpload(t, up.Id, 30)
	key := uploadKey(t, up.Id)

	repo := repository.New(testPool)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	marked := make(chan struct{})
	release := make(chan struct{})
	markErr := make(chan error, 1)
	go func() {
		markErr <- repo.WithTenant(ctx, func(tx repository.Tx) error {
			if err := tx.MarkUploadReferenced(ctx, up.Id); err != nil {
				return err
			}
			close(marked)
			<-release // 持着行锁，等回收那一侧撞上来
			return nil
		})
	}()
	<-marked

	gcDone := make(chan service.UploadGCReport, 1)
	go func() {
		rep, err := newUploadGC().CollectOnce(context.Background())
		if err != nil {
			t.Error(err)
		}
		gcDone <- rep
	}()

	// 等到回收那一侧真的在等行锁（而不是已经跑完），再让标引用的事务提交。
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := adminQueryInt64(t, `SELECT count(*) FROM pg_stat_activity
		                          WHERE wait_event_type = 'Lock' AND query LIKE '%DELETE FROM uploads%'`)
		if n > 0 {
			break
		}
		select {
		case rep := <-gcDone:
			close(release)
			t.Fatalf("回收没有等标引用的行锁就跑完了：%+v", rep)
		default:
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("10 秒内没看到回收那条 DELETE 在等行锁")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	if err := <-markErr; err != nil {
		t.Fatal(err)
	}
	rep := <-gcDone

	if !uploadRowExists(t, up.Id) || !fileExists(t, key) {
		t.Fatalf("标引用的事务提交之后凭证被回收了（记录 %v，文件 %v；报告 %+v）",
			uploadRowExists(t, up.Id), fileExists(t, key), rep)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM uploads WHERE id = $1 AND referenced`, up.Id); n != 1 {
		t.Fatal("凭证没有停在已引用")
	}
	if rep.Raced < 1 {
		t.Fatalf("回收报告里没有记下这一次竞态：%+v", rep)
	}
}
