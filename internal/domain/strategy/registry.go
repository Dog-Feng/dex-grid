package strategy

import (
	"fmt"
	"sort"
	"sync"
)

// Factory 从策略参数构造一个策略实例。
//
// params 是从数据库读出的策略专有参数（JSON 反序列化后的原始字节），
// 由各策略自行解析，这样新增策略不需要改动应用层。
type Factory func(params []byte) (Strategy, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册一个策略。重复注册会 panic，因为这只会是编译期的编码错误。
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic(fmt.Sprintf("strategy: duplicate registration of %q", name))
	}
	factories[name] = f
}

// New 按名称构造策略。
func New(name string, params []byte) (Strategy, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("strategy: unknown strategy %q", name)
	}
	return f(params)
}

// Names 返回已注册的策略名，按字典序排列。
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for name := range factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
