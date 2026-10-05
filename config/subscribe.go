package config

import (
	"sort"
	"sync/atomic"
)

// defaultHistoryRetain 是历史版本（含当前版本）的默认保留数量。
const defaultHistoryRetain = 10

// defaultSubscriberBuffer 是订阅通道在未显式指定容量时的缓冲大小。
const defaultSubscriberBuffer = 16

// redacted 是敏感字段对外露出时的占位值。
const redacted = "<redacted>"

// ChangeKind 描述单个配置键在一次更新中的变化类型。
type ChangeKind string

const (
	ChangeAdded   ChangeKind = "added"   // 新键出现
	ChangeUpdated ChangeKind = "updated" // 值发生变化
	ChangeRemoved ChangeKind = "removed" // 键从所有来源中消失
)

// KeyChange 描述单个键的一次变动。敏感字段的 Old/New 不会出现真值，
// 而是以 "<redacted>" 代替，Sensitive 为 true。
type KeyChange struct {
	Key       string
	Kind      ChangeKind
	Source    string // 该键最终生效来源；被删除的键为旧来源
	Old       any    // 旧值；敏感字段为 "<redacted>"，新增键为 nil
	New       any    // 新值；敏感字段为 "<redacted>"，删除键为 nil
	Sensitive bool
}

// ChangeEvent 是一次成功生效的更新推送给订阅方的内容：
// 新快照本身，加上本次相对上一版本的变动明细。
type ChangeEvent struct {
	Version  uint64
	Snapshot *Snapshot
	Changes  []KeyChange // 按键名升序排列
}

// Subscription 表示一次订阅。
type Subscription struct {
	ch      chan *ChangeEvent
	dropped atomic.Int64 // 因该订阅方消费过慢而丢弃的事件数
	active  atomic.Bool  // 退订后置 false；通道本身不关闭，避免与并发扇出竞争
}

// Channel 返回事件通道。每次成功生效的更新向该通道做一次非阻塞发送：
// 若缓冲已满（订阅方处理太慢），该事件对该订阅方丢弃，绝不拖慢更新流程，
// 也绝不反压其他订阅方。被丢弃后仍可从最新事件携带的完整 Snapshot 自行对账。
func (sub *Subscription) Channel() <-chan *ChangeEvent { return sub.ch }

// Dropped 返回该订阅方累计被丢弃的事件数。
func (sub *Subscription) Dropped() int64 { return sub.dropped.Load() }

// Option 配置 Store 的可选行为。
type Option func(*options)

type options struct {
	historyRetain int
}

// WithHistoryRetain 配置历史版本保留数量（含当前版本，小于 1 时按 1 处理）。
func WithHistoryRetain(n int) Option {
	return func(o *options) { o.historyRetain = n }
}

// NewStoreWithOptions 与 NewStore 相同，但支持配置历史保留数量等选项。
// sources 按优先级从低到高排列，语义与 NewStore 一致。
func NewStoreWithOptions(schema Schema, sources []Source, opts ...Option) (*Store, error) {
	o := options{historyRetain: defaultHistoryRetain}
	for _, opt := range opts {
		opt(&o)
	}
	return newStore(schema, o.historyRetain, sources)
}

// Subscribe 注册一个订阅。buffer 为通道缓冲大小；buffer <= 0 时使用默认值。
// 订阅不会补发注册之前的事件（如需对账请在注册后立即读取 Snapshot）。
// 订阅/退订可与并发更新安全交叉进行。
func (s *Store) Subscribe(buffer int) *Subscription {
	if buffer <= 0 {
		buffer = defaultSubscriberBuffer
	}
	sub := &Subscription{ch: make(chan *ChangeEvent, buffer)}
	sub.active.Store(true)
	s.subMu.Lock()
	s.subs[sub] = struct{}{}
	s.subMu.Unlock()
	return sub
}

// Unsubscribe 注销订阅。可重复调用，仅第一次生效。
// 通道不会被关闭（关闭会与并发扇出发生 send-on-closed 竞争）：退订只是从注册表
// 摘除并置为非活跃，此后扇出不再向其投递；订阅方应停止接收并丢弃该订阅。
func (s *Store) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	s.subMu.Lock()
	if _, ok := s.subs[sub]; !ok {
		s.subMu.Unlock()
		return
	}
	delete(s.subs, sub)
	s.subMu.Unlock()
	sub.active.Store(false)
}

// subscriberSnapshot 在 subMu 下取出当前订阅方快照，避免扇出期间持锁。
func (s *Store) subscriberSnapshot() []*Subscription {
	s.subMu.RLock()
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subMu.RUnlock()
	return subs
}

// notify 向各订阅方做非阻塞扇出。慢订阅方只影响它自己：
// 缓冲满即丢弃当前事件并累加计数，更新流程不会被阻塞。
func (s *Store) notify(ev *ChangeEvent, subs []*Subscription) {
	for _, sub := range subs {
		if !sub.active.Load() {
			continue // 退订与扇出交叉：跳过，至多此前已入队一个事件
		}
		select {
		case sub.ch <- ev:
		default:
			sub.dropped.Add(1)
		}
	}
}

// buildChangeEvent 对比相邻两个已生效快照，产出变动明细。
// prev 为 nil 表示首次生效，全部现存键计为 added。
func buildChangeEvent(schema Schema, prev, next *Snapshot) *ChangeEvent {
	seen := map[string]struct{}{}
	var changes []KeyChange

	// 新快照中的键：新增或更新。
	for _, k := range next.Keys() {
		seen[k] = struct{}{}
		nv, _ := next.Get(k)
		src, _ := next.Origin(k)
		sensitive := false
		if f, ok := schema[k]; ok {
			sensitive = f.Sensitive
		}
		if prev == nil {
			changes = append(changes, KeyChange{
				Key: k, Kind: ChangeAdded, Source: src,
				Old: nil, New: maskValue(nv, sensitive), Sensitive: sensitive,
			})
			continue
		}
		ov, existed := prev.Get(k)
		switch {
		case !existed:
			changes = append(changes, KeyChange{
				Key: k, Kind: ChangeAdded, Source: src,
				Old: nil, New: maskValue(nv, sensitive), Sensitive: sensitive,
			})
		case !equalValue(ov, nv):
			changes = append(changes, KeyChange{
				Key: k, Kind: ChangeUpdated, Source: src,
				Old: maskValue(ov, sensitive), New: maskValue(nv, sensitive),
				Sensitive: sensitive,
			})
		}
	}

	// 旧快照有、新快照没有的键：删除。
	if prev != nil {
		for _, k := range prev.Keys() {
			if _, ok := seen[k]; ok {
				continue
			}
			ov, _ := prev.Get(k)
			oldSrc, _ := prev.Origin(k)
			sensitive := false
			if f, ok := schema[k]; ok {
				sensitive = f.Sensitive
			}
			changes = append(changes, KeyChange{
				Key: k, Kind: ChangeRemoved, Source: oldSrc,
				Old: maskValue(ov, sensitive), New: nil, Sensitive: sensitive,
			})
		}
	}

	sort.Slice(changes, func(i, j int) bool { return changes[i].Key < changes[j].Key })
	return &ChangeEvent{Version: next.Version, Snapshot: next, Changes: changes}
}

// maskValue 在字段敏感时以占位值代替真值。
func maskValue(v any, sensitive bool) any {
	if sensitive {
		return redacted
	}
	return v
}

// equalValue 比较两个已按 schema 解码的值；配置值仅含 string/int64/float64/bool，
// 直接用 == 即可得到确定结论。
func equalValue(a, b any) bool { return a == b }
