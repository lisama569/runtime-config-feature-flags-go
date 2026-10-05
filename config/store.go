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

	mu      sync.Mutex // 仅保护更新（Reload）串行化
	current atomic.Pointer[Snapshot]
	events  *broker // 变更通知：仅在实际生效后发布

	historyKeep int                  // 保留的历史版本数（不含当前版本）
	history     map[uint64]*Snapshot // 最近若干已生效版本，超出上限淘汰最老
}

// NewStore 用给定 schema 与来源（按优先级从低到高）构建 Store，
// 并立即执行一次加载；加载失败返回错误。
func NewStore(schema Schema, sources ...Source) (*Store, error) {
	s := &Store{
		schema:      schema,
		sources:     append([]Source(nil), sources...),
		events:      newBroker(),
		historyKeep: DefaultHistoryKeep,
		history:     map[uint64]*Snapshot{},
	}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Snapshot 返回当前已生效的配置快照（不可变，可安全并发读取）。
func (s *Store) Snapshot() *Snapshot { return s.current.Load() }

// Reload 重新从所有来源加载并校验配置，全部成功才原子切换；
// 任一来源失败或校验失败则整体放弃，保留旧配置，且不产生任何变更通知。
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap, err := s.build()
	if err != nil {
		return err // 旧快照保持不变
	}
	s.activate(snap, false)
	return nil
}

// activate 把一份已校验的快照原子切换为当前生效配置，
// 把被替换下来的旧版本收入历史，并向订阅方发布变更通知。
// 调用方须持有 s.mu。
func (s *Store) activate(snap *Snapshot, rollback bool) {
	old := s.current.Load()
	s.current.Store(snap)
	s.recordHistoryLocked(old)
	s.events.publish(Event{
		Snapshot: snap,
		Changes:  diffSnapshots(s.schema, old, snap),
		Rollback: rollback,
	})
}

// build 加载、合并并校验全部来源，产出新快照；不修改当前状态。
func (s *Store) build() (*Snapshot, error) {
	// 1. 分层合并：记录每个键最终生效的来源，便于错误定位。
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

	version := uint64(1)
	if old := s.current.Load(); old != nil {
		version = old.Version + 1
	}
	return &Snapshot{Version: version, values: values, origins: origin}, nil
}

// String 输出当前快照（敏感字段脱敏）。
func (s *Store) String() string {
	return fmt.Sprintf("%s", s.Snapshot().String(s.schema))
}
