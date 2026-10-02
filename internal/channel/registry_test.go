package channel_test

import (
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/channeltest"
)

func TestRegistry(t *testing.T) {
	r := channel.NewRegistry()
	r.Register(channeltest.New())
	if _, ok := r.Lookup(channeltest.Kind); !ok {
		t.Fatal("登记过的找不到")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("没登记过的找到了")
	}
	defer func() {
		if recover() == nil {
			t.Error("同一个 Kind 登记两次没有 panic")
		}
	}()
	r.Register(channeltest.New())
}
