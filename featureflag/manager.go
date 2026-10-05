package featureflag

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Manager 包装 Engine，支持运行期整体热替换规则集：
// 替换原子生效，并发判定要么看到旧版、要么看到新版，绝不混杂；
// 同时保留最近若干版规则，支持按历史版本重放判定。
type Manager struct {
	current atomic.Pointer[Engine]

	mu      sync.Mutex         // 仅保护 Update 串行化与历史表
	keep    int                // 保留的历史规则版本数（不含当前版本）
	history map[uint64]*Engine // 最近若干已生效规则版本
}

// NewManager 用初始规则集构建 Manager（版本从 1 开始）。
// keep 为保留的历史规则版本数量（不含当前版本），小于 0 按 0 处理。
// 规则非法时返回与 Compile 相同的错误，不产生任何状态。
func NewManager(flags []Flag, keep int) (*Manager, error) {
	e, err := CompileWithVersion(flags, 1)
	if err != nil {
		return nil, err
	}
	if keep < 0 {
		keep = 0
	}
	m := &Manager{keep: keep, history: map[uint64]*Engine{}}
	m.current.Store(e)
	return m, nil
}

// Update 校验并整体替换规则集，版本号在当前基础上 +1。
// 校验失败时当前规则集保持不变，返回与 Compile 相同的错误，
// 不会用默认值顶替、也不产生新版本。
func (m *Manager) Update(flags []Flag) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.current.Load().Version() + 1
	e, err := CompileWithVersion(flags, next)
	if err != nil {
		return err // 当前规则集保持不变
	}
	old := m.current.Swap(e)
	m.history[old.Version()] = old
	m.evictLocked()
	return nil
}

// Evaluate 按当前生效的规则版本判定。
func (m *Manager) Evaluate(flag, target string) (Decision, error) {
	return m.current.Load().Evaluate(flag, target)
}

// EvaluateAt 按指定的历史规则版本重放判定，结果与当时一致。
// 版本不存在（未保留或从未存在）时返回 ReasonUnknownVersion 错误。
func (m *Manager) EvaluateAt(version uint64, flag, target string) (Decision, error) {
	e := m.current.Load()
	if e.Version() != version {
		m.mu.Lock()
		e = m.history[version]
		m.mu.Unlock()
		if e == nil {
			return Decision{}, &Error{Reason: ReasonUnknownVersion,
				Detail: fmt.Sprintf("rules version %d not retained or never existed", version)}
		}
	}
	return e.Evaluate(flag, target)
}

// Version 返回当前生效的规则集版本。
func (m *Manager) Version() uint64 { return m.current.Load().Version() }

// evictLocked 淘汰超出保留上限的最老规则版本。调用方须持有 m.mu。
func (m *Manager) evictLocked() {
	for len(m.history) > m.keep {
		oldest := uint64(0)
		first := true
		for v := range m.history {
			if first || v < oldest {
				oldest, first = v, false
			}
		}
		delete(m.history, oldest)
	}
}
