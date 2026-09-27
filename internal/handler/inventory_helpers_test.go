package handler_test

import (
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// localInventory 是建在测试池上的进程内库存服务 —— 与 app.Router 不给 WithInventory 时
// 装的是同一个东西（微服务拆分阶段 1a）。直接构造 service 的测试用它。
func localInventory() inventory.Service {
	return inventory.NewLocal(repository.NewInventoryStore(testPool))
}
