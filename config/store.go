package config

import (
	"sync"
	"sync/atomic"
	"time"
)

// Snapshot 是一份已生效的不可变配置。
type Snapshot struct {
	Version   int64
	Values    map[string]any
	UpdatedAt time.Time
}

// Get 读取一个配置项。
func (s *Snapshot) Get(key string) (any, bool) {
	v, ok := s.Values[key]
	return v, ok
}

// GetString 读取字符串配置项。
func (s *Snapshot) GetString(key string) (string, bool) {
	v, ok := s.Values[key].(string)
	return v, ok
}

// Store 提供并发安全的配置读写与原子更新。
// 读路径无锁（atomic.Pointer），写路径互斥且整体替换，
// 读取方永远不会观察到半更新状态。
type Store struct {
	loader  *Loader
	current atomic.Pointer[Snapshot]
	mu      sync.Mutex // 串行化并发 Update
	version int64
}

// NewStore 加载初始配置并创建 Store。初始加载失败则返回错误。
func NewStore(loader *Loader) (*Store, error) {
	s := &Store{loader: loader}
	snap, err := loader.Load(1)
	if err != nil {
		return nil, err
	}
	s.version = 1
	s.current.Store(snap)
	return s, nil
}

// Current 返回当前生效的快照。
func (s *Store) Current() *Snapshot { return s.current.Load() }

// Update 重新加载并原子生效；失败时整体放弃并保留上一份已生效配置。
func (s *Store) Update() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.version + 1
	snap, err := s.loader.Load(next)
	if err != nil {
		// 整体放弃：不触碰 current，旧配置继续生效。
		return err
	}
	s.current.Store(snap) // 原子生效
	s.version = next
	return nil
}
