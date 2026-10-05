package config

import "fmt"

// DefaultHistoryKeep 是未显式配置时保留的历史版本数量（不含当前生效版本）。
const DefaultHistoryKeep = 16

// VersionError 描述按版本访问历史配置失败的原因，
// 可区分"从未存在过/尚未达到"与"曾生效但已被淘汰"两种情况。
type VersionError struct {
	Version uint64
	Evicted bool // true 表示该版本曾生效但已超出保留上限被淘汰
}

func (e *VersionError) Error() string {
	if e.Evicted {
		return fmt.Sprintf("config version %d was evicted from history (retention exceeded)", e.Version)
	}
	return fmt.Sprintf("config version %d does not exist", e.Version)
}

// SnapshotAt 按版本号取回当时生效的配置快照（只读、不可变）。
// 版本不存在或已被淘汰时返回 *VersionError，可用 Evicted 字段区分。
func (s *Store) SnapshotAt(version uint64) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotAtLocked(version)
}

// snapshotAtLocked 在持有 s.mu 的前提下按版本查找快照。
func (s *Store) snapshotAtLocked(version uint64) (*Snapshot, error) {
	if cur := s.current.Load(); cur != nil && cur.Version == version {
		return cur, nil
	}
	if snap, ok := s.history[version]; ok {
		return snap, nil
	}
	cur := s.current.Load()
	switch {
	case version == 0 || cur == nil || version > cur.Version:
		return nil, &VersionError{Version: version} // 从未存在或尚未达到
	default:
		return nil, &VersionError{Version: version, Evicted: true} // 曾生效但已淘汰
	}
}

// RollbackTo 回滚到某个历史版本：该版本的内容成为当前生效配置，
// 版本号继续单调递增（取当前版本号 +1，不倒退），并像正常更新一样通知订阅方。
// 回滚只读取内存中的历史快照，不访问任何配置来源，
// 因此来源不可用或被改坏时回滚照样成功。
func (s *Store) RollbackTo(version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	target, err := s.snapshotAtLocked(version)
	if err != nil {
		return err
	}
	cur := s.current.Load()
	// 复用历史快照的不可变内容，但分配新的（更大的）版本号。
	restored := &Snapshot{Version: cur.Version + 1, values: target.values, origins: target.origins}
	s.activate(restored, true)
	return nil
}

// SetHistoryLimit 设置保留的历史版本数量（不含当前生效版本）。
// 超过上限后最老的版本被淘汰。n 小于 0 时按 0 处理。
// 调小上限会立即淘汰多余的旧版本。
func (s *Store) SetHistoryLimit(n int) {
	if n < 0 {
		n = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyKeep = n
	s.evictLocked()
}

// recordHistoryLocked 把刚被替换下来的旧当前版本收入历史，并按上限淘汰。
// 调用方须持有 s.mu。
func (s *Store) recordHistoryLocked(prev *Snapshot) {
	if prev == nil {
		return
	}
	s.history[prev.Version] = prev
	s.evictLocked()
}

// evictLocked 淘汰超出保留上限的最老历史版本。调用方须持有 s.mu。
func (s *Store) evictLocked() {
	for len(s.history) > s.historyKeep {
		oldest := uint64(0)
		first := true
		for v := range s.history {
			if first || v < oldest {
				oldest, first = v, false
			}
		}
		delete(s.history, oldest)
	}
}
