package channel

import (
	"fmt"
	"sort"
	"sync"
)

// Registry 按 Kind 登记适配器。一个进程一份（app 装配时建），测试各建各的。
type Registry struct {
	mu sync.RWMutex
	m  map[string]Adapter
}

func NewRegistry() *Registry { return &Registry{m: map[string]Adapter{}} }

// Register 登记一个适配器。同一个 Kind 登记两次是装配错误，直接 panic。
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := a.Kind()
	if _, dup := r.m[k]; dup {
		panic(fmt.Sprintf("渠道适配器 %q 登记了两次", k))
	}
	r.m[k] = a
}

// Lookup 取一个适配器；没登记过返回 false（binding 指向了一个这个进程没编进来的渠道）。
func (r *Registry) Lookup(kind string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.m[kind]
	return a, ok
}

// Kinds 是登记过的全部 Kind（升序），后台建 binding 时校验用。
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
