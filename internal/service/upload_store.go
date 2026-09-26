package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 文件存储的 driver 面（数据模型 §13）。
//
// ===========================================================================
// 为什么这一轮真的把字节写进磁盘，而不是只登记一条元数据
// ===========================================================================
//
// POST /admin/uploads 的契约收的是 multipart 的一个文件。只登记元数据的话，
// 服务端仍然要把整个文件读一遍（size_bytes 与 sha256 是必填列），
// 读完把字节丢掉 —— 而 uploads.storage_key 那一列的语义是「driver 内部路径」。
// 也就是说库里会躺着一条指向不存在的文件的记录，而它与一条正常记录长得
// 一模一样：没有任何字段说得出「这个 key 后面什么都没有」。
//
// §13 已经把本地磁盘 driver 设计完了，而它「不是新增组件，就是容器的一个卷」。
// 既然读文件那一步反正要做，把 io.Copy 的目的地从 io.Discard 换成一个真实
// 文件是 30 行的事 —— 比在契约清单上再挂一笔账便宜。
//
// **仍然缺的那一半要说清楚**：契约里读文件那条 GET /uploads/{upload_id}
// 没有实现（它是买家侧接口，不在本轮那 16 条 /admin/ 写接口里）。
// 所以 Upload.url 指向的地址今天会 404。字节是真的在磁盘上，缺的是读它的路由。
// 这两件事的差别不是措辞：前者「文件在，路由还没写」下一个任务就能补上，
// 后者「文件从来没存过」意味着存量数据全是废的。

// UploadStore 是 §13 那三个方法里本轮用得上的那一个：写入并返回 storage_key。
//
// 删除与「生成访问 URL」不在这里：前者属于孤儿回收任务（§12 的 jobs，
// 本轮没有），后者在契约里被钉成了 /api/v1/uploads/{upload_id} 这个固定形状 ——
// 它由 upload 的 id 拼出来，与 driver 无关，所以它不该是 driver 的方法。
type UploadStore interface {
	// Driver 是 uploads.driver 的取值：1 本地磁盘 / 2 S3 兼容（§13）。
	Driver() int16

	// Put 把 r 的全部内容写进存储，返回 storage_key、字节数与 sha256。
	//
	// merchantID 进 key 的前缀，这是 §13 的原话（「storage_key 里应包含
	// merchant_id 前缀，换 S3 driver 时按前缀分桶最省事」）。
	//
	// limit 是硬上限（契约：10 MB）。超出时返回 ErrUploadTooLarge 并且
	// **不留下半个文件** —— 判大小不能靠先读进内存再比，那样一个 2 GB 的
	// 请求体会在判出来之前就把进程吃掉。
	Put(merchantID int64, ext string, r io.Reader, limit int64) (key string, size int64, sum string, err error)
}

// LocalDiskStore 是 §13 的本地磁盘 driver。
type LocalDiskStore struct{ root string }

// NewLocalDiskStore 建一个落在 root 下的磁盘 driver。
func NewLocalDiskStore(root string) *LocalDiskStore { return &LocalDiskStore{root: root} }

func (*LocalDiskStore) Driver() int16 { return 1 }

// Root 是这个 driver 的根目录，给启动日志用。
func (s *LocalDiskStore) Root() string { return s.root }

// storageKeyRandomBytes 是 key 里那段随机数的长度。
//
// **key 刻意不是内容寻址（sha256）的。** 内容寻址会让两家店传同一张图片
// 撞上同一个 key，而 uk_uploads_key 是全局唯一的（§2 那条分界线的第三类：
// 物理路径的唯一性不能按租户切）—— 也就是说第二家店会写入失败，
// 而失败的原因是「别家店传过同一个文件」。那是一条跨租户的存在性泄露，
// 而且它以一个 500 的形式出现。
const storageKeyRandomBytes = 16

func (s *LocalDiskStore) Put(merchantID int64, ext string, r io.Reader, limit int64) (string, int64, string, error) {
	if s.root == "" {
		return "", 0, "", errors.New("本地磁盘 driver 没有配置根目录")
	}
	var buf [storageKeyRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", 0, "", err
	}
	name := hex.EncodeToString(buf[:]) + ext
	// merchant_id 前缀（§13），外加一层两字符的散列目录：一个租户传几万张图
	// 之后单目录下的文件数会让 ls 和备份都变慢，而这一层是免费的。
	key := strconv.FormatInt(merchantID, 10) + "/" + name[:2] + "/" + name

	full := filepath.Join(s.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", 0, "", err
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", 0, "", err
	}
	// 失败路径上必须把半个文件删掉：留着的话它既不在 uploads 表里
	// （那条 INSERT 没跑），也就永远不会被孤儿回收看见 —— 一个谁也不认识的字节堆。
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(full)
		}
	}()

	h := sha256.New()
	// LimitReader 取 limit+1：正好读到 limit+1 个字节就说明它超了。
	// 读满 limit 就判超的话，一个正好 10 MB 的文件（契约允许）会被拒。
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if err != nil {
		return "", 0, "", err
	}
	if n > limit {
		return "", 0, "", ErrUploadTooLarge
	}
	if n == 0 {
		// chk_upload_size 也会拦（size_bytes > 0），但它给出的是 23514，
		// 而那条错误里没有任何东西指向「文件是空的」。
		return "", 0, "", fmt.Errorf("%w: 文件是空的", ErrCatalogBadRequest)
	}
	if err := f.Sync(); err != nil {
		return "", 0, "", err
	}
	ok = true
	return key, n, hex.EncodeToString(h.Sum(nil)), nil
}

// uploadExtensions 是契约允许的三种 content_type 与它们的扩展名。
//
// 扩展名由**服务端**按 content_type 定，不取客户端给的文件名：文件名是
// 客户端可以随便写的东西，而它会变成磁盘上的一段路径。
var uploadExtensions = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

// uploadExtensionFor 按 content_type 给扩展名。不在允许列表里返回 false，
// 调用方据此回契约里那个 415。
//
// 只比 MIME 类型本身，不比后面的参数（`image/jpeg; charset=x` 这种）：
// 浏览器与各家 HTTP 客户端在 multipart 的 part 头上加不加参数并不一致，
// 而按整串比会把一个合法的 JPEG 判成 415。
func uploadExtensionFor(contentType string) (string, string, bool) {
	mime := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	ext, ok := uploadExtensions[mime]
	return mime, ext, ok
}
