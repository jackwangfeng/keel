package service

import (
	"fmt"
	"io"
	"sort"
)

// UploadStorage 把几个 driver 合成一个（数据模型 §13）：**写**一律落在主 driver 上，**读、删、缩略图**按那一行
// uploads.driver 找当初写它的那个 driver。
//
// 为什么不是「换 driver 就整体换」：从本地磁盘换到对象存储时，存量文件还在磁盘上。按行路由的话可以先把写切到
// 对象存储（新图直接进桶）、老图照旧从磁盘读，再由迁移工具（cmd/keel-uploads migrate）一行一行搬过去、
// 把 driver 从 1 改成 2——全程不停服、没有「搬完之前图都打不开」的窗口。搬完之后磁盘 driver 可以撤掉。
//
// 它自己也实现 UploadStore（主 driver 那一面），所以只认一个 driver 的旧调用方不用改。
type UploadStorage struct {
	write UploadStore
	by    map[int16]UploadStore
}

var _ UploadStore = (*UploadStorage)(nil)

// NewUploadStorage 以 write 为主 driver，others 是只读、只删的旧 driver（同一个 Driver() 只能有一个）。
func NewUploadStorage(write UploadStore, others ...UploadStore) (*UploadStorage, error) {
	s := &UploadStorage{write: write, by: map[int16]UploadStore{write.Driver(): write}}
	for _, o := range others {
		if _, dup := s.by[o.Driver()]; dup {
			return nil, fmt.Errorf("driver %d 配了两次", o.Driver())
		}
		s.by[o.Driver()] = o
	}
	return s, nil
}

// Primary 是写入用的 driver。
func (s *UploadStorage) Primary() UploadStore { return s.write }

// For 返回写 driver d 那些行的 driver。没配那个 driver 时报错（不悄悄拿别的 driver 去读：读不到会变成一个
// 看上去像「文件被回收了」的 404，而真相是「换了 driver、旧的没留着」）。
func (s *UploadStorage) For(d int16) (UploadStore, error) {
	if st, ok := s.by[d]; ok {
		return st, nil
	}
	return nil, fmt.Errorf("%w: 这个文件在 driver %d 上，而本进程没有配置它（换了存储、存量没迁完？见 cmd/keel-uploads）",
		ErrUploadBlobMissing, d)
}

// Drivers 返回配置了的全部 driver（升序），孤儿回收逐个扫。
func (s *UploadStorage) Drivers() []int16 {
	out := make([]int16, 0, len(s.by))
	for d := range s.by {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *UploadStorage) Driver() int16 { return s.write.Driver() }
func (s *UploadStorage) Put(merchantID int64, ext string, r io.Reader, limit int64) (string, int64, string, error) {
	return s.write.Put(merchantID, ext, r, limit)
}
func (s *UploadStorage) Open(key string) (io.ReadCloser, error) { return s.write.Open(key) }
func (s *UploadStorage) Remove(key string) error                { return s.write.Remove(key) }

// storeFor 是服务层取「这一行的 driver」的唯一入口：传进来的若是 UploadStorage 就按行路由；
// 只是单个 driver 时，行的 driver 必须就是它。
func storeFor(store UploadStore, driver int16) (UploadStore, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: 本进程没有配置文件存储", ErrUploadBlobMissing)
	}
	if st, ok := store.(*UploadStorage); ok {
		return st.For(driver)
	}
	if store.Driver() == driver {
		return store, nil
	}
	return nil, fmt.Errorf("%w: 这个文件在 driver %d 上，本进程只配了 driver %d", ErrUploadBlobMissing, driver, store.Driver())
}

// storeDrivers 是孤儿回收要扫的 driver 列表。
func storeDrivers(store UploadStore) []int16 {
	if st, ok := store.(*UploadStorage); ok {
		return st.Drivers()
	}
	return []int16{store.Driver()}
}
