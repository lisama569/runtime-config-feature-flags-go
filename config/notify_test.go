package config

import (
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// recvEvent 在超时内收取一个事件，超时则失败。
func recvEvent(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

// findChange 在变动明细中查找指定键。
func findChange(t *testing.T, ev Event, key string) KeyChange {
	t.Helper()
	for _, c := range ev.Changes {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("no change recorded for key %q in %+v", key, ev.Changes)
	return KeyChange{}
}

// TestNotifyChangeDetails 验证生效更新后订阅方收到新快照与变动明细：
// 变更键、最终生效来源、新旧值；敏感字段不泄露真值。
func TestNotifyChangeDetails(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/c.json"
	writeFile(t, f, `{"http_port": 8080, "timeout_ms": 1000, "api_token": "old-secret"}`)
	store, err := NewStore(testSchema(), &FileSource{Path: f})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sub, unsubscribe := store.Subscribe(8)
	defer unsubscribe()

	writeFile(t, f, `{"http_port": 9090, "feature_x": true, "api_token": "new-secret"}`)
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	ev := recvEvent(t, sub)
	log.Printf("[event] version=%d rollback=%v changes=%+v", ev.Snapshot.Version, ev.Rollback, ev.Changes)
	if ev.Snapshot.Version != 2 {
		t.Fatalf("event snapshot version: want 2, got %d", ev.Snapshot.Version)
	}
	if ev.Rollback {
		t.Fatal("normal reload must not be marked as rollback")
	}

	// http_port：值变更，来源为文件源。
	c := findChange(t, ev, "http_port")
	log.Printf("[change] key=http_port source=%s old=%v new=%v", c.Source, c.Old, c.New)
	if c.Old != int64(8080) || c.New != int64(9090) {
		t.Fatalf("http_port change: want 8080->9090, got %v->%v", c.Old, c.New)
	}
	if !strings.HasPrefix(c.Source, "file:") {
		t.Fatalf("http_port source: want file source, got %q", c.Source)
	}

	// feature_x：新增键，Old 为 nil。
	c = findChange(t, ev, "feature_x")
	if c.Old != nil || c.New != true {
		t.Fatalf("feature_x change: want nil->true, got %v->%v", c.Old, c.New)
	}

	// timeout_ms：被移除（新配置未携带），New 为 nil。
	c = findChange(t, ev, "timeout_ms")
	if c.Old != int64(1000) || c.New != nil {
		t.Fatalf("timeout_ms change: want 1000->nil, got %v->%v", c.Old, c.New)
	}

	// api_token：敏感字段，只标记变动，不携带真值。
	c = findChange(t, ev, "api_token")
	log.Printf("[change] key=api_token redacted=%v old=%v new=%v", c.Redacted, c.Old, c.New)
	if !c.Redacted {
		t.Fatal("sensitive key change must be marked redacted")
	}
	if c.Old != nil || c.New != nil {
		t.Fatalf("sensitive values must not leak: old=%v new=%v", c.Old, c.New)
	}
}

// TestNoNotifyOnFailedReload 验证更新失败未生效时不产生任何通知。
func TestNoNotifyOnFailedReload(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/c.json"
	writeFile(t, f, `{"http_port": 8080, "api_token": "secret"}`)
	store, err := NewStore(testSchema(), &FileSource{Path: f})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sub, unsubscribe := store.Subscribe(8)
	defer unsubscribe()

	writeFile(t, f, `{"http_port": "bad", "api_token": "secret"}`)
	if err := store.Reload(); err == nil {
		t.Fatal("reload with invalid value must fail")
	} else {
		log.Printf("[rejected] %v", err)
	}

	select {
	case ev := <-sub.C:
		t.Fatalf("failed reload must not notify, got %+v", ev)
	case <-time.After(100 * time.Millisecond):
		log.Printf("[ok] no event after failed reload")
	}
}

// TestSlowSubscriberDoesNotBlock 验证慢订阅方缓冲满后事件被丢弃并计数，
// 而更新流程与其他订阅方不被拖住。
func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/c.json"
	writeFile(t, f, `{"http_port": 8080, "api_token": "secret"}`)
	store, err := NewStore(testSchema(), &FileSource{Path: f})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	slow, unsubSlow := store.Subscribe(1) // 容量 1，且从不消费
	defer unsubSlow()
	fast, unsubFast := store.Subscribe(64)
	defer unsubFast()

	const updates = 20
	start := time.Now()
	for i := 0; i < updates; i++ {
		writeFile(t, f, `{"http_port": `+itoa64(int64(9000+i))+`, "api_token": "secret"}`)
		if err := store.Reload(); err != nil {
			t.Fatalf("Reload %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	log.Printf("[updates] %d reloads in %v with a stuck subscriber", updates, elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("updates blocked by slow subscriber: %v", elapsed)
	}

	// 快订阅方收齐全部事件。
	for i := 0; i < updates; i++ {
		ev := recvEvent(t, fast)
		if ev.Snapshot.Version != uint64(2+i) {
			t.Fatalf("fast subscriber event %d: want version %d, got %d", i, 2+i, ev.Snapshot.Version)
		}
	}
	// 慢订阅方只缓冲得下 1 条，其余被丢弃且可观察。
	if got := slow.Dropped(); got != updates-1 {
		t.Fatalf("slow subscriber dropped: want %d, got %d", updates-1, got)
	}
	log.Printf("[slow-subscriber] buffered=1 dropped=%d/%d", slow.Dropped(), updates)
}

// TestConcurrentSubscribeReload 在竞态检测下并发订阅、退订与更新交叉进行。
func TestConcurrentSubscribeReload(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/c.json"
	writeFile(t, f, `{"http_port": 8080, "api_token": "secret"}`)
	store, err := NewStore(testSchema(), &FileSource{Path: f})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 更新者：持续生效合法更新。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			writeFile(t, f, `{"http_port": `+itoa64(int64(8000+i%50))+`, "api_token": "secret"}`)
			_ = store.Reload()
		}
		close(stop)
	}()

	// 订阅/退订者：反复注册、消费若干事件后退订。
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sub, unsubscribe := store.Subscribe(4)
				// 消费到通道关闭或收到停止信号。
				for {
					select {
					case _, ok := <-sub.C:
						if !ok {
							goto next
						}
					case <-stop:
						unsubscribe()
						return
					}
				}
			next:
				unsubscribe() // 幂等：重复退订安全
			}
		}()
	}
	wg.Wait()
	log.Printf("[ok] concurrent subscribe/unsubscribe/reload finished, final %s",
		store.Snapshot().String(testSchema()))
}
