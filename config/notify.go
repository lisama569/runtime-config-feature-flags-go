package config

import (
	"sort"
	"sync"
	"sync/atomic"
)

// KeyChange 描述一次生效更新中单个键的变动明细。
type KeyChange struct {
	Key      string // 配置键
	Source   string // 该键最终生效的来源（键被移除时为其旧来源）
	Old      any    // 旧值；键此前不存在或字段敏感时为 nil
	New      any    // 新值；键被移除或字段敏感时为 nil
	Redacted bool   // 敏感字段：Old/New 一律不携带真值
}

// Event 是一次"已生效"更新推送给订阅者的内容：
// 新快照连同本次变动的明细。更新失败未生效时不产生任何 Event。
type Event struct {
	Snapshot *Snapshot   // 本次生效后的新快照
	Changes  []KeyChange // 相对上一份生效配置的变动明细（按键排序）
	Rollback bool        // 是否由回滚到历史版本触发
}

// Subscription 是一个订阅句柄。事件通过 C 推送；
// 订阅方处理过慢导致缓冲满时，新事件对该订阅方丢弃（不阻塞更新流程），
// 丢弃数量可通过 Dropped 观察。退订后 C 会被关闭。
type Subscription struct {
	C       <-chan Event
	c       chan Event
	dropped atomic.Uint64
}

// Dropped 返回因订阅方消费过慢而被丢弃的事件数。
func (sub *Subscription) Dropped() uint64 { return sub.dropped.Load() }

// broker 管理订阅者集合：注册/退订与并发发布安全；
// 发布对每个订阅方都是非阻塞发送，缓冲满即丢弃并计数，
// 因此慢订阅方永远不会拖住更新流程。
type broker struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

func newBroker() *broker { return &broker{subs: map[*Subscription]struct{}{}} }

// subscribe 注册一个缓冲容量为 buffer 的订阅，返回句柄与幂等的退订函数。
func (b *broker) subscribe(buffer int) (*Subscription, func()) {
	if buffer < 0 {
		buffer = 0
	}
	c := make(chan Event, buffer)
	sub := &Subscription{C: c, c: c}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, sub)
			b.mu.Unlock()
			close(c)
		})
	}
	return sub, unsubscribe
}

// publish 把事件非阻塞地推给当前所有订阅方；
// 与 subscribe/unsubscribe 并发安全：发布期间退订的订阅方
// 可能收到也可能收不到本次事件，但绝不会向已关闭的通道发送。
func (b *broker) publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subs {
		select {
		case sub.c <- ev:
		default:
			sub.dropped.Add(1) // 订阅方缓冲满：丢弃，不阻塞更新
		}
	}
}

// Subscribe 注册一个订阅，buffer 为该订阅方的事件缓冲容量。
// 返回的取消函数用于退订，可并发调用、幂等；退订后 C 会被关闭。
//
// 削峰语义：更新流程向订阅方推送事件时绝不阻塞；若某订阅方消费过慢、
// 缓冲已满，则该次事件对此订阅方丢弃（通过 Subscription.Dropped 可观察），
// 其余订阅方与更新流程不受影响。
func (s *Store) Subscribe(buffer int) (*Subscription, func()) {
	return s.events.subscribe(buffer)
}

// diffSnapshots 计算从 old 到 new 的变动明细（按键排序）。
// 敏感字段的 Old/New 一律置 nil 并标记 Redacted，不泄露真值。
// old 为 nil 时（首次加载）所有键都视为新增。
func diffSnapshots(schema Schema, old, new *Snapshot) []KeyChange {
	keys := map[string]struct{}{}
	if old != nil {
		for k := range old.values {
			keys[k] = struct{}{}
		}
	}
	for k := range new.values {
		keys[k] = struct{}{}
	}

	changes := []KeyChange{}
	for k := range keys {
		oldV, hadOld := oldValue(old, k)
		newV, hadNew := new.values[k]
		if hadOld && hadNew && oldV == newV {
			continue // 值未变，不产生明细
		}
		kc := KeyChange{Key: k}
		if f, ok := schema[k]; ok && f.Sensitive {
			kc.Redacted = true
		} else {
			if hadOld {
				kc.Old = oldV
			}
			if hadNew {
				kc.New = newV
			}
		}
		if hadNew {
			kc.Source = new.origins[k]
		} else {
			kc.Source = old.origins[k]
		}
		changes = append(changes, kc)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Key < changes[j].Key })
	return changes
}

func oldValue(old *Snapshot, key string) (any, bool) {
	if old == nil {
		return nil, false
	}
	v, ok := old.values[key]
	return v, ok
}
