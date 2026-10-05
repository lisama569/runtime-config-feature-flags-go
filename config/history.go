package config

import "fmt"

// VersionNotFoundError 表示请求的版本号从未存在（大于当前最新版本）。
type VersionNotFoundError struct {
	Version uint64
	Current uint64
}

func (e *VersionNotFoundError) Error() string {
	return fmt.Sprintf("config version %d not found (current=%d)", e.Version, e.Current)
}

// VersionEvictedError 表示请求的版本曾经生效，但已超出保留上限被淘汰。
type VersionEvictedError struct {
	Version     uint64
	OldestKnown uint64
}

func (e *VersionEvictedError) Error() string {
	return fmt.Sprintf("config version %d evicted (oldest retained=%d)", e.Version, e.OldestKnown)
}

// HistoryVersion 按版本号取回一份已生效的历史快照（只读，不可变）。
// 版本号大于当前版本返回 *VersionNotFoundError；
// 版本号小于当前保留窗口中最老版本返回 *VersionEvictedError，两者可区分。
func (s *Store) HistoryVersion(version uint64) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.current.Load()
	if cur == nil {
		return nil, &VersionNotFoundError{Version: version}
	}
	if version > cur.Version {
		return nil, &VersionNotFoundError{Version: version, Current: cur.Version}
	}
	oldest := s.history[0].Version
	if version < oldest {
		return nil, &VersionEvictedError{Version: version, OldestKnown: oldest}
	}
	// 窗口内版本连续，直接按下标取。
	return s.history[version-oldest], nil
}

// Rollback 回滚到某个保留中的历史版本。回滚使用的是当时已确认生效的快照内容，
// 不重新读取任何来源——因此即使来源此刻不可用或内容已损坏，回滚照常成功。
// 回滚后该内容成为当前生效配置，版本号在当前基础上 +1（不倒退），
// 并与正常更新一样通知订阅方。
// 目标版本不存在/已淘汰时错误分类与 HistoryVersion 相同。
func (s *Store) Rollback(version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.current.Load()
	if cur == nil || version > cur.Version {
		c := uint64(0)
		if cur != nil {
			c = cur.Version
		}
		return &VersionNotFoundError{Version: version, Current: c}
	}
	oldest := s.history[0].Version
	if version < oldest {
		return &VersionEvictedError{Version: version, OldestKnown: oldest}
	}
	target := s.history[version-oldest]
	return s.commitLocked(target.cloneForRollback())
}
