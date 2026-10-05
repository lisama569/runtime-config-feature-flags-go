package featureflag

import (
	"sync"
	"sync/atomic"
)

// ReasonUnknownVersion 表示请求重放的规则集版本从未存在。
const ReasonUnknownVersion Reason = "unknown_version"

// Manager 在不可变 Engine 之上支持规则集运行期整体热替换，并保留全部
// 历史规则版本以支持按版本重放。判定走原子指针，无锁读取；
// 并发判定期间进行替换，每次判定只会看到替换前或替换后某一整版规则，
// 绝不会看到两版规则混合的结果。
type Manager struct {
	current atomic.Pointer[Engine]

	mu       sync.RWMutex // 仅保护 versions 历史表与替换过程
	versions map[uint64]*Engine
}

// NewManager 构建规则管理器（初始版本 1）。规则非法时返回错误且不产生管理器。
func NewManager(flags []Flag) (*Manager, error) {
	return NewManagerAtVersion(flags, 1)
}

// NewManagerAtVersion 以显式版本号构建规则管理器；版本号必须大于 0。
func NewManagerAtVersion(flags []Flag, version uint64) (*Manager, error) {
	if version == 0 {
		return nil, &Error{Reason: ReasonInvalidRule, Detail: "version must be > 0"}
	}
	e, err := CompileWithVersion(flags, version)
	if err != nil {
		return nil, err
	}
	m := &Manager{versions: map[uint64]*Engine{version: e}}
	m.current.Store(e)
	return m, nil
}

// Replace 整体替换规则集。新规则先完整编译校验，全部合法才原子切换；
// 任一规则非法则整体放弃，当前规则集保持不变（校验与错误分类语义与 Compile 一致）。
// 新版本号为当前版本 +1，并纳入历史版本表供重放。
func (m *Manager) Replace(flags []Flag) (*Engine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nextVersion := m.current.Load().version + 1
	e, err := CompileWithVersion(flags, nextVersion)
	if err != nil {
		return nil, err // 旧规则集保持不变
	}
	m.versions[nextVersion] = e
	m.current.Store(e) // 原子切换：读者只见旧整版或新整版
	return e, nil
}

// Current 返回当前生效的规则引擎（不可变）。
func (m *Manager) Current() *Engine { return m.current.Load() }

// Version 返回当前规则集版本。
func (m *Manager) Version() uint64 { return m.current.Load().version }

// Evaluate 使用当前生效规则判定。底层引擎在单次判定内不可变，
// 因此同一目标在同一版规则下结论稳定、可复现。
func (m *Manager) Evaluate(flag, target string) (Decision, error) {
	return m.current.Load().Evaluate(flag, target)
}

// EvaluateAt 按指定历史规则版本重放判定，结论与该版本当时生效时完全一致
// （版本参与分流哈希，历史引擎原样保留）。
// 版本不存在时返回 ReasonUnknownVersion 错误，绝不回退到当前版本。
func (m *Manager) EvaluateAt(version uint64, flag, target string) (Decision, error) {
	m.mu.RLock()
	e, ok := m.versions[version]
	m.mu.RUnlock()
	if !ok {
		return Decision{}, &Error{
			Reason: ReasonUnknownVersion, Flag: flag,
			Detail: "rule version " + uitoa(version) + " not found",
		}
	}
	return e.Evaluate(flag, target)
}

// uitoa 将无符号整数转为十进制字符串。
func uitoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
