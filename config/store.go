package config

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Store 是并发安全的配置存储：读取无锁，更新原子生效，
// 更新失败整体放弃并保留上一份已生效配置。
type Store struct {
	schema  Schema
	sources []Source // 按优先级从低到高排列

	mu      sync.Mutex // 串行化 Reload/Rollback，并保护历史窗口
	current atomic.Pointer[Snapshot]

	// 历史版本窗口：history 按版本号升序连续排列，history[0] 即最老保留版本。
	history       []*Snapshot
	historyRetain int

	// 订阅注册表：可与 Reload 并发地订阅/退订。
	subMu sync.RWMutex
	subs  map[*Subscription]struct{}
}

// NewStore 用给定 schema 与来源（按优先级从低到高）构建 Store，
// 并立即执行一次加载；加载失败返回错误。历史版本保留默认数量。
func NewStore(schema Schema, sources ...Source) (*Store, error) {
	return newStore(schema, defaultHistoryRetain, sources)
}

// Snapshot 返回当前已生效的配置快照（不可变，可安全并发读取）。
func (s *Store) Snapshot() *Snapshot { return s.current.Load() }

// newStore 用给定历史保留数量构建 Store 并执行首次加载。
func newStore(schema Schema, retain int, sources []Source) (*Store, error) {
	if retain < 1 {
		retain = 1
	}
	s := &Store{
		schema:        schema,
		sources:       append([]Source(nil), sources...),
		historyRetain: retain,
		subs:          map[*Subscription]struct{}{},
	}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload 重新从所有来源加载并校验配置，全部成功才原子切换；
// 任一来源失败或校验失败则整体放弃，保留旧配置。
// 仅当新快照真正生效后才通知订阅方；失败不产生任何通知。
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next, err := s.build()
	if err != nil {
		return err // 旧快照保持不变，不通知
	}
	return s.commitLocked(next)
}

// commitLocked 在持有 s.mu 时把已校验快照变为当前版本：分配版本号、
// 原子切换、纳入历史窗口并通知订阅方。回滚同样走这条路径，
// 因此版本号始终单调递增、通知语义与正常更新一致。
func (s *Store) commitLocked(next *Snapshot) error {
	if old := s.current.Load(); old != nil {
		next.Version = old.Version + 1
	} else {
		next.Version = 1
	}
	s.current.Store(next)

	// 纳入历史窗口；超过保留上限时淘汰最老版本。
	s.history = append(s.history, next)
	if excess := len(s.history) - s.historyRetain; excess > 0 {
		for i := 0; i < excess; i++ {
			s.history[i] = nil // 帮助回收
		}
		s.history = s.history[excess:]
	}

	if subs := s.subscriberSnapshot(); len(subs) > 0 {
		var prev *Snapshot
		if n := len(s.history); n >= 2 {
			prev = s.history[n-2]
		}
		s.notify(buildChangeEvent(s.schema, prev, next), subs)
	}
	return nil
}

// build 加载、合并并校验全部来源，产出新快照；不修改当前状态。
func (s *Store) build() (*Snapshot, error) {
	// 1. 分层合并：记录每个键最终生效的来源，便于错误定位与变动明细。
	merged := map[string][]byte{}
	origin := map[string]string{}
	for _, src := range s.sources {
		vals, err := src.Load()
		if err != nil {
			return nil, err
		}
		for k, v := range vals {
			merged[k] = v
			origin[k] = src.Name()
		}
	}

	// 2. 校验：schema 内的键逐一解码校验；未知键直接拒绝，避免静默生效。
	values := map[string]any{}
	for k, raw := range merged {
		f, ok := s.schema[k]
		if !ok {
			return nil, &RejectError{
				Reason: ReasonInvalidValue, Source: origin[k], Key: k,
				Detail: "key not declared in schema",
			}
		}
		v, err := decodeValue(origin[k], k, f, raw)
		if err != nil {
			return nil, err
		}
		values[k] = v
	}
	for k, f := range s.schema {
		if f.Required {
			if _, ok := values[k]; !ok {
				return nil, &RejectError{
					Reason: ReasonMissingRequired, Source: "(merged)", Key: k,
					Detail: "required key missing in all sources",
				}
			}
		}
	}

	return &Snapshot{values: values, origin: origin}, nil
}

// String 输出当前快照（敏感字段脱敏）。
func (s *Store) String() string {
	return fmt.Sprintf("%s", s.Snapshot().String(s.schema))
}
