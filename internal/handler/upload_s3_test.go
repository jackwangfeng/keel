package handler_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 上传存储换成对象存储（driver 2，service/upload_s3.go）之后，经真实的路由走一遍：
//
//   - 后台上传落进桶里，uploads.driver 记 2；GET /uploads/{id} 302 → 站内第二跳 → 同样的字节；
//   - 缩略图在桶里缓存（thumbs/…），第二次直接读缓存；
//   - 换存储的过渡期：之前写在磁盘上的图（driver 1）照旧读得到（按行路由）；
//   - 配了对外地址时第一跳直接 302 到对象存储的预签名地址，字节不经过应用。
//
// 对象存储是进程内的 gofakes3（真 S3 协议，后端在内存里）。

func s3Engine(t *testing.T, presign bool) (*service.S3Store, *s3mem.Backend, string) {
	t.Helper()
	be := s3mem.New()
	if err := be.CreateBucket("keel"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(be).Server())
	t.Cleanup(srv.Close)
	cfg := service.S3Config{Endpoint: srv.URL, Bucket: "keel", AccessKey: "ak", SecretKey: "sk", AddressStyle: "path"}
	if presign {
		cfg.PresignEndpoint = srv.URL
	}
	s3, err := service.NewS3Store(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := service.NewUploadStorage(s3, service.NewLocalDiskStore(testUploadRoot))
	if err != nil {
		t.Fatal(err)
	}
	useEngine(t, app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner, testOrders,
		service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithQuotaSync(testQuotaSync), app.WithUploadStore(st)))
	return s3, be, srv.URL
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func follow(t *testing.T, host, loc string) *httptest.ResponseRecorder {
	t.Helper()
	if strings.HasPrefix(loc, "http") { // 预签名地址：真打出去
		resp, err := http.Get(loc)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		w := httptest.NewRecorder()
		w.Code = resp.StatusCode
		_, _ = w.Body.ReadFrom(resp.Body)
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		return w
	}
	return getNoAuth(t, host, loc)
}

func TestUploadsGoToObjectStorageAndOldDiskFilesStayReadable(t *testing.T) {
	sh := newAdminShop(t)
	// 换存储之前传的一张（默认路由：本地磁盘，driver 1）。
	oldContent := []byte("RIFF....WEBP 磁盘上的老图 " + sh.Suffix)
	old := seedUpload(t, sh, oldContent)

	_, be, _ := s3Engine(t, false)
	content := pngOf(t, 400, 300)
	var up struct {
		Id  int64  `json:"id"`
		Url string `json:"url"`
	}
	decodeInto(t, uploadImage(t, sh, "image/png", content), http.StatusCreated, "传图到对象存储", &up)

	var driver int16
	var key string
	if err := admin(t).QueryRow(context.Background(), `SELECT driver, storage_key FROM uploads WHERE id = $1`, up.Id).Scan(&driver, &key); err != nil {
		t.Fatal(err)
	}
	if driver != 2 {
		t.Fatalf("新传的图 driver = %d，期望 2（对象存储）", driver)
	}
	if _, err := be.HeadObject("keel", key); err != nil {
		t.Fatalf("桶里没有 %s：%v", key, err)
	}

	read := func(path string) []byte {
		t.Helper()
		w := getNoAuth(t, sh.Host, path)
		if w.Code != http.StatusFound {
			t.Fatalf("GET %s 回了 %d：%s", path, w.Code, w.Body.String())
		}
		b := follow(t, sh.Host, w.Header().Get("Location"))
		if b.Code != http.StatusOK {
			t.Fatalf("跟着 %s 回了 %d：%s", w.Header().Get("Location"), b.Code, b.Body.String())
		}
		return b.Body.Bytes()
	}
	if got := read(up.Url); !bytes.Equal(got, content) {
		t.Fatalf("从对象存储读回来的字节不一致（%d vs %d）", len(got), len(content))
	}
	// 磁盘上的老图：按行的 driver 1 读，换存储之后照旧打得开。
	if got := read(old.Url); !bytes.Equal(got, oldContent) {
		t.Fatalf("换存储之后磁盘上的老图读不出来了")
	}

	// 缩略图：第一次现算并缓存进桶，第二次读缓存。
	thumb := read(up.Url + "?w=160")
	if _, _, err := image.Decode(bytes.NewReader(thumb)); err != nil {
		t.Fatalf("缩略图解不开：%v", err)
	}
	if _, err := be.HeadObject("keel", "thumbs/"+key+".w160.jpg"); err != nil {
		t.Fatalf("缩略图没缓存进桶：%v", err)
	}
	if again := read(up.Url + "?w=160"); !bytes.Equal(again, thumb) {
		t.Error("第二次读缩略图与第一次不一致")
	}
}

func TestUploadRedirectsStraightToPresignedObjectURL(t *testing.T) {
	sh := newAdminShop(t)
	_, _, s3URL := s3Engine(t, true)
	content := pngOf(t, 400, 300)
	var up struct {
		Url string `json:"url"`
	}
	decodeInto(t, uploadImage(t, sh, "image/png", content), http.StatusCreated, "传图", &up)

	w := getNoAuth(t, sh.Host, up.Url)
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc, s3URL) {
		t.Fatalf("配了对外地址时应当 302 到对象存储：%d %q", w.Code, loc)
	}
	u, _ := url.Parse(loc)
	if u.Query().Get("X-Amz-Signature") == "" || u.Query().Get("X-Amz-Expires") != "300" {
		t.Fatalf("预签名地址缺签名或寿命不是 5 分钟：%q", loc)
	}
	if b := follow(t, sh.Host, loc); b.Code != http.StatusOK || !bytes.Equal(b.Body.Bytes(), content) {
		t.Fatalf("照预签名地址取不回原图：%d", b.Code)
	}
	// 缩略图：还没生成时走站内第二跳现算；生成之后第一跳就直连对象存储。
	w = getNoAuth(t, sh.Host, up.Url+"?w=320")
	if loc := w.Header().Get("Location"); strings.HasPrefix(loc, s3URL) {
		t.Fatalf("没生成过的缩略图不该预签名：%q", loc)
	} else if b := follow(t, sh.Host, loc); b.Code != http.StatusOK {
		t.Fatalf("站内现算缩略图失败：%d", b.Code)
	}
	w = getNoAuth(t, sh.Host, up.Url+"?w=320")
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, s3URL) || !strings.Contains(loc, ".w320.jpg") {
		t.Fatalf("生成过的缩略图应当直连对象存储：%q", loc)
	}
}

// 孤儿回收在换存储的过渡期里两个 driver 都扫：磁盘上的老孤儿用磁盘 driver 删，桶里的新孤儿用对象存储 driver 删。
func TestUploadGCCollectsOrphansOnEveryDriver(t *testing.T) {
	sh := newAdminShop(t)
	old := seedUpload(t, sh, []byte("RIFF 磁盘上的孤儿 "+sh.Suffix))
	oldKey := uploadKey(t, old.Id)

	s3, be, _ := s3Engine(t, false)
	var up struct {
		Id int64 `json:"id"`
	}
	decodeInto(t, uploadImage(t, sh, "image/png", pngOf(t, 10, 10)), http.StatusCreated, "传图", &up)
	newKey := uploadKey(t, up.Id)
	ageUpload(t, old.Id, 48)
	ageUpload(t, up.Id, 48)

	st, err := service.NewUploadStorage(s3, service.NewLocalDiskStore(testUploadRoot))
	if err != nil {
		t.Fatal(err)
	}
	gc := service.NewUploadGCService(repository.New(testPool), st, service.SweepConfig{}, nil)
	if _, err := gc.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if uploadRowExists(t, old.Id) || fileExists(t, oldKey) {
		t.Error("磁盘上的孤儿（driver 1）应当连记录带文件一起删掉")
	}
	if uploadRowExists(t, up.Id) {
		t.Error("桶里的孤儿（driver 2）记录应当删掉")
	}
	if _, err := be.HeadObject("keel", newKey); err == nil {
		t.Error("桶里的孤儿文件应当删掉")
	}
}

// 存量迁移（service/upload_migrate.go，cmd/keel-uploads migrate）：磁盘 → 对象存储。
//
//   - 好的文件：同一个 storage_key 写进桶、sha256 对上才把 driver 改成 2，之后经路由照样读得到；
//   - 源文件缺了的、字节被改过的：计进报告、行留在磁盘 driver 上，桶里不留坏的那份；
//   - 重跑：搬过的不再动，只剩那两条坏的又被扫到；-delete-source 删掉磁盘上的旧文件。
func TestUploadMigrationFromDiskToObjectStorage(t *testing.T) {
	sh := newAdminShop(t)
	good := seedUpload(t, sh, []byte("RIFF 好的 "+sh.Suffix))
	gone := seedUpload(t, sh, []byte("RIFF 文件丢了 "+sh.Suffix))
	bad := seedUpload(t, sh, []byte("RIFF 字节被改了 "+sh.Suffix))
	goodKey, goneKey, badKey := uploadKey(t, good.Id), uploadKey(t, gone.Id), uploadKey(t, bad.Id)
	if err := os.Remove(filepath.Join(testUploadRoot, goneKey)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testUploadRoot, badKey), []byte("RIFF 字节被改了 xxxxxxxx"), 0o644); err != nil {
		t.Fatal(err)
	}

	s3, be, _ := s3Engine(t, false)
	local := service.NewLocalDiskStore(testUploadRoot)
	m, err := service.NewUploadMigrator(repository.New(testPool), local, s3, nil)
	if err != nil {
		t.Fatal(err)
	}
	opt := service.UploadMigrateOptions{Merchant: sh.MerchantID}

	// 试跑：什么都不改。
	dry := opt
	dry.DryRun = true
	rep, err := m.Run(context.Background(), dry)
	if err != nil || rep.Scanned != 3 || rep.Missing != 1 || rep.Moved != 0 {
		t.Fatalf("试跑：%+v %v", rep, err)
	}
	if adminQueryInt64(t, `SELECT count(*) FROM uploads WHERE merchant_id = $1 AND driver = 2`, sh.MerchantID) != 0 {
		t.Fatal("试跑改了库")
	}

	rep, err = m.Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 3 || rep.Moved != 1 || rep.Missing != 1 || rep.Mismatch != 1 {
		t.Fatalf("迁移报告：%+v", rep)
	}
	driverOf := func(id int64) int64 {
		return adminQueryInt64(t, `SELECT driver FROM uploads WHERE id = $1`, id)
	}
	if driverOf(good.Id) != 2 || driverOf(gone.Id) != 1 || driverOf(bad.Id) != 1 {
		t.Fatalf("改指不对：good=%d gone=%d bad=%d", driverOf(good.Id), driverOf(gone.Id), driverOf(bad.Id))
	}
	if _, err := be.HeadObject("keel", badKey); err == nil {
		t.Error("字节对不上的那份不该留在桶里")
	}
	// 搬过去的经路由（按行 driver 2）照样读得到，字节与原来一致。
	w := getNoAuth(t, sh.Host, good.Url)
	if b := follow(t, sh.Host, w.Header().Get("Location")); b.Code != http.StatusOK || b.Body.String() != "RIFF 好的 "+sh.Suffix {
		t.Fatalf("迁移之后读不回来：%d %q", b.Code, b.Body.String())
	}

	// 重跑：只扫到那两条坏的；-delete-source 不影响它们（没搬成就不删）。
	again := opt
	again.DeleteSource = true
	rep, err = m.Run(context.Background(), again)
	if err != nil || rep.Scanned != 2 || rep.Moved != 0 {
		t.Fatalf("重跑：%+v %v", rep, err)
	}
	if !fileExists(t, badKey) {
		t.Error("没搬成的不该删源文件")
	}
	// 修好那一条再跑：搬过去，并删掉磁盘上的旧文件。
	if err := os.WriteFile(filepath.Join(testUploadRoot, badKey), []byte("RIFF 字节被改了 "+sh.Suffix), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err = m.Run(context.Background(), again)
	if err != nil || rep.Moved != 1 || driverOf(bad.Id) != 2 || fileExists(t, badKey) {
		t.Fatalf("修好之后：%+v %v driver=%d 源还在=%v", rep, err, driverOf(bad.Id), fileExists(t, badKey))
	}
	if fileExists(t, goodKey) == false {
		// 第一次迁移没开 -delete-source：已经搬过的那条磁盘文件还在（重跑不会再扫到它，也就不会删）。
		t.Error("第一次没开 -delete-source，磁盘上已搬的那份应当还在")
	}
	if _, err := service.NewUploadMigrator(repository.New(testPool), s3, s3, nil); err == nil {
		t.Error("源和目标同一个 driver 应当拒绝")
	}
}
