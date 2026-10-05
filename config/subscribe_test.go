package config

import (
	"errors"
	"log"
	"path/filepath"
	"sync"
	"testing"
)

// newTestStore 构造一个文件源 Store 并返回文件路径，便于后续改写触发 Reload。
func newTestStore(t *testing.T, body string, opts ...Option) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	writeFile(t, f, body)
	store, err := NewStoreWithOptions(testSchema(), []Source{&FileSource{Path: f}}, opts...)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, f
}

func recv(t *testing.T, sub *Subscription) *ChangeEvent {
	t.Helper()
	select {
	case ev := <-sub.Channel():
		return ev
	default:
		t.Fatal("expected a change event")
		return nil
	}
}

// TestSubscribeChangeDetails 验证生效更新推送新快照与逐键变动明细，
// 敏感字段脱敏，且来源可解释。
func TestSubscribeChangeDetails(t *testing.T) {
	store, f := newTestStore(t, `{"http_port": 8080, "timeout_ms": 1000, "api_token": "secret-1"}`)
	sub := store.Subscribe(4)

	// 改 http_port 与 api_token，删除 timeout_ms。
	writeFile(t, f, `{"http_port": 9090, "api_token": "secret-2"}`)
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	ev := recv(t, sub)
	log.Printf("[version] %d; [snapshot] %s", ev.Version, ev.Snapshot.String(testSchema()))
	if ev.Version != 2 {
		t.Fatalf("event version: want 2, got %d", ev.Version)
	}
	if ev.Snapshot != store.Snapshot() {
		t.Fatal("event must carry the newly effective snapshot")
	}

	byKey := map[string]KeyChange{}
	for _, c := range ev.Changes {
		byKey[c.Key] = c
		log.Printf("[change] key=%s kind=%s source=%s old=%v new=%v sensitive=%v",
			c.Key, c.Kind, c.Source, c.Old, c.New, c.Sensitive)
	}
	if c := byKey["http_port"]; c.Kind != ChangeUpdated || c.Old != int64(8080) || c.New != int64(9090) {
		t.Fatalf("http_port change wrong: %+v", c)
	}
	if c := byKey["timeout_ms"]; c.Kind != ChangeRemoved || c.Old != int64(1000) || c.New != nil {
		t.Fatalf("timeout_ms removal wrong: %+v", c)
	}
	tok := byKey["api_token"]
	if tok.Kind != ChangeUpdated || !tok.Sensitive || tok.Old != redacted || tok.New != redacted {
		t.Fatalf("sensitive change must be redacted: %+v", tok)
	}
	if byKey["http_port"].Source == "" {
		t.Fatal("change must carry winning source")
	}

	// 新增一个可选键。
	writeFile(t, f, `{"http_port": 9090, "api_token": "secret-2", "feature_x": true}`)
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	ev2 := recv(t, sub)
	var fx *KeyChange
	for i := range ev2.Changes {
		if ev2.Changes[i].Key == "feature_x" {
			fx = &ev2.Changes[i]
		}
	}
	if fx == nil || fx.Kind != ChangeAdded || fx.New != true {
		t.Fatalf("expected feature_x added, got %+v", ev2.Changes)
	}
	log.Printf("[change] key=feature_x kind=%s new=%v", fx.Kind, fx.New)
}

// TestSubscribeNoNotifyOnFailure 验证更新失败不生效、不通知，版本也不前进。
func TestSubscribeNoNotifyOnFailure(t *testing.T) {
	store, f := newTestStore(t, `{"http_port": 8080, "api_token": "secret-1"}`)
	sub := store.Subscribe(2)
	before := store.Snapshot()

	writeFile(t, f, `{"http_port": 70000, "api_token": "secret-2"}`) // 端口越界
	err := store.Reload()
	var re *RejectError
	if !errors.As(err, &re) || re.Reason != ReasonInvalidValue {
		t.Fatalf("want invalid_value rejection, got %v", err)
	}
	log.Printf("[rejected] %v", err)
	if store.Snapshot() != before {
		t.Fatal("failed reload must keep previous snapshot")
	}
	select {
	case ev := <-sub.Channel():
		t.Fatalf("failed reload must not notify, got event v%d", ev.Version)
	default:
		log.Printf("[notify] none after failed reload (as required)")
	}

	// 之后一次成功更新仍只通知一次、版本紧接之前。
	writeFile(t, f, `{"http_port": 9090, "api_token": "secret-2"}`)
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	ev := recv(t, sub)
	if ev.Version != 2 {
		t.Fatalf("version after rejected reload: want 2, got %d", ev.Version)
	}
}

// TestSubscribeUnsubscribe 验证退订后不再收到事件，重复退订安全；通道不会关闭。
func TestSubscribeUnsubscribe(t *testing.T) {
	store, f := newTestStore(t, `{"http_port": 8080, "api_token": "s"}`)
	sub := store.Subscribe(2)
	store.Unsubscribe(sub)
	store.Unsubscribe(sub) // 重复调用不 panic

	// 排空可能在退订前已入队的残留事件。
drain:
	for {
		select {
		case <-sub.Channel():
		default:
			break drain
		}
	}

	writeFile(t, f, `{"http_port": 9090, "api_token": "s"}`)
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	select {
	case ev := <-sub.Channel():
		t.Fatalf("unsubscribed subscriber must not receive new events, got v%d", ev.Version)
	default:
		log.Printf("[unsubscribed] no event after reload; dropped=%d", sub.Dropped())
	}
}

// TestSlowSubscriberDoesNotBlock 验证慢订阅方缓冲满时事件被丢弃、计数增长，
// 但更新流程不被阻塞，其他订阅方照常收到全部事件。
func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	store, f := newTestStore(t, `{"http_port": 8080, "api_token": "s"}`)
	slow := store.Subscribe(1) // 容量仅 1，很快填满
	fast := store.Subscribe(8)

	for port := int64(8081); port <= 8085; port++ {
		writeFile(t, f, `{"http_port": `+itoa64(port)+`, "api_token": "s"}`)
		if err := store.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	}
	log.Printf("[backpressure] slow dropped=%d (buffer=1, updates=5)", slow.Dropped())
	if slow.Dropped() == 0 {
		t.Fatal("slow subscriber should have dropped events")
	}

	// 快订阅方排空，应能收到全部 5 次事件。
	got := 0
	for {
		select {
		case <-fast.Channel():
			got++
		default:
			if got != 5 {
				t.Fatalf("fast subscriber: want 5 events, got %d", got)
			}
			log.Printf("[fast] received all %d events; [slow] dropped=%d", got, slow.Dropped())
			return
		}
	}
}

// TestConcurrentSubscribeReload 在竞态检测下让订阅/退订与大量更新交叉进行，
// 同时一个长期订阅方持续排空事件，验证注册表操作与扇出安全共存。
func TestConcurrentSubscribeReload(t *testing.T) {
	store, f := newTestStore(t, `{"http_port": 8080, "api_token": "s"}`)
	longLived := store.Subscribe(64)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 更新者：只写合法值，保证每次 Reload 都成功生效并通知。
	wg.Add(1)
	go func() {
		defer wg.Done()
		port := int64(9000)
		for i := 0; i < 300; i++ {
			port++
			writeFile(t, f, `{"http_port": `+itoa64(port)+`, "api_token": "s"}`)
			if err := store.Reload(); err != nil {
				t.Errorf("Reload: %v", err)
				return
			}
		}
		close(stop)
	}()

	// 频繁订阅/退订者。
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					sub := store.Subscribe(2)
					store.Unsubscribe(sub)
				}
			}
		}()
	}

	// 长期订阅方排空事件并校验：事件版本单调、快照即当前内容。
	wg.Add(1)
	go func() {
		defer wg.Done()
		last := uint64(1)
		for {
			select {
			case ev := <-longLived.Channel():
				if ev.Version <= last {
					t.Errorf("event version not monotonic: %d after %d", ev.Version, last)
					return
				}
				// 排空时 current 可能已更新到更新版本，但事件自带快照必须与事件版本自洽。
				if ev.Snapshot.Version != ev.Version {
					t.Errorf("event snapshot version %d != event version %d", ev.Snapshot.Version, ev.Version)
					return
				}
				last = ev.Version
			case <-stop:
				return
			}
		}
	}()

	wg.Wait()
	store.Unsubscribe(longLived)
	log.Printf("[final] %s; longLivedDropped=%d", store.Snapshot().String(testSchema()), longLived.Dropped())
}
