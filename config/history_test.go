package config

import (
	"errors"
	"log"
	"testing"
)

// reloadWithPort 用内存源把端口更新为指定值并生效一个新版本。
func reloadWithPort(t *testing.T, store *Store, mem *MemorySource, port int64) {
	t.Helper()
	mem.Values["http_port"] = MustJSON(port)
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload(port=%d): %v", port, err)
	}
}

// TestSnapshotAtAndEviction 验证按版本读取、保留上限与淘汰，
// 以及"从未存在"与"已被淘汰"两类错误可区分。
func TestSnapshotAtAndEviction(t *testing.T) {
	mem := &MemorySource{SourceName: "inmemory", Values: map[string][]byte{
		"http_port": MustJSON(int64(8000)),
		"api_token": MustJSON("secret"),
	}}
	store, err := NewStore(testSchema(), mem)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.SetHistoryLimit(3) // 只保留最近 3 个历史版本

	// 生效 v2..v6（v1 为初始版本）。
	for i := int64(1); i <= 5; i++ {
		reloadWithPort(t, store, mem, 8000+i)
	}
	if got := store.Snapshot().Version; got != 6 {
		t.Fatalf("current version: want 6, got %d", got)
	}

	// 保留上限 3：历史应含 v3,v4,v5；v1,v2 已被淘汰。
	for _, v := range []uint64{3, 4, 5, 6} {
		snap, err := store.SnapshotAt(v)
		if err != nil {
			t.Fatalf("SnapshotAt(%d): %v", v, err)
		}
		log.Printf("[read] version=%d http_port=%d", v, snap.MustInt("http_port"))
		if want := int64(8000 + v - 1); snap.MustInt("http_port") != want {
			t.Fatalf("version %d port: want %d, got %d", v, want, snap.MustInt("http_port"))
		}
	}

	// 已被淘汰：v1, v2。
	for _, v := range []uint64{1, 2} {
		_, err := store.SnapshotAt(v)
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Fatalf("SnapshotAt(%d): want *VersionError, got %v", v, err)
		}
		log.Printf("[evicted] version=%d err=%q", v, ve)
		if !ve.Evicted {
			t.Fatalf("version %d should be reported as evicted", v)
		}
	}

	// 从未存在：v0 与超出当前版本号的 v99。
	for _, v := range []uint64{0, 99} {
		_, err := store.SnapshotAt(v)
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Fatalf("SnapshotAt(%d): want *VersionError, got %v", v, err)
		}
		log.Printf("[not-found] version=%d err=%q", v, ve)
		if ve.Evicted {
			t.Fatalf("version %d never existed, must not be reported as evicted", v)
		}
	}
}

// TestRollbackTo 验证回滚语义：历史内容成为当前配置、版本号继续递增、
// 订阅方收到标记为回滚的通知；并且来源不可用时回滚照样成功。
func TestRollbackTo(t *testing.T) {
	mem := &MemorySource{SourceName: "inmemory", Values: map[string][]byte{
		"http_port": MustJSON(int64(8080)),
		"api_token": MustJSON("good-secret"),
	}}
	store, err := NewStore(testSchema(), mem)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sub, unsubscribe := store.Subscribe(8)
	defer unsubscribe()

	reloadWithPort(t, store, mem, 9090) // v2
	log.Printf("[before-rollback] %s", store.Snapshot().String(testSchema()))

	// 来源此刻被改坏：普通 Reload 必然失败。
	mem.Values = map[string][]byte{"http_port": MustJSON("broken"), "api_token": MustJSON(1)}
	if err := store.Reload(); err == nil {
		t.Fatal("reload with broken source must fail")
	} else {
		log.Printf("[source-broken] reload rejected: %v", err)
	}
	// 失败的通知也不该出现。
	recvEvent(t, sub) // 只消费掉 v2 的那条通知

	// 回滚到 v1：不访问来源，必须成功。
	if err := store.RollbackTo(1); err != nil {
		t.Fatalf("RollbackTo(1) with broken source: %v", err)
	}
	snap := store.Snapshot()
	log.Printf("[after-rollback] %s", snap.String(testSchema()))
	if snap.Version != 3 {
		t.Fatalf("version after rollback: want 3 (monotonic), got %d", snap.Version)
	}
	if got := snap.MustInt("http_port"); got != 8080 {
		t.Fatalf("port after rollback: want 8080 (v1 content), got %d", got)
	}

	// 订阅方收到回滚事件：内容与 v1 一致，变动明细反映 9090->8080。
	ev := recvEvent(t, sub)
	log.Printf("[event] version=%d rollback=%v changes=%+v", ev.Snapshot.Version, ev.Rollback, ev.Changes)
	if !ev.Rollback {
		t.Fatal("rollback event must be marked as rollback")
	}
	if ev.Snapshot.Version != 3 {
		t.Fatalf("rollback event version: want 3, got %d", ev.Snapshot.Version)
	}
	c := findChange(t, ev, "http_port")
	if c.Old != int64(9090) || c.New != int64(8080) {
		t.Fatalf("rollback change: want 9090->8080, got %v->%v", c.Old, c.New)
	}

	// 回滚后来源恢复，普通更新继续工作且版本继续递增。
	mem.Values = map[string][]byte{"http_port": MustJSON(7070), "api_token": MustJSON("s")}
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload after rollback: %v", err)
	}
	if got := store.Snapshot().Version; got != 4 {
		t.Fatalf("version after post-rollback reload: want 4, got %d", got)
	}

	// 回滚到不存在/已淘汰的版本：明确报错，当前配置不变。
	store.SetHistoryLimit(1) // 只留最近 1 个历史版本，强制淘汰旧版本
	if err := store.RollbackTo(1); err == nil {
		t.Fatal("rollback to evicted version must fail")
	} else {
		var ve *VersionError
		if !errors.As(err, &ve) || !ve.Evicted {
			t.Fatalf("want evicted VersionError, got %v", err)
		}
		log.Printf("[rollback-rejected] %v", err)
	}
	if store.Snapshot().Version != 4 {
		t.Fatal("failed rollback must not change current snapshot")
	}
}

// TestRollbackNotifiesContentIdentity 验证回滚后的当前快照与历史版本内容一致。
func TestRollbackNotifiesContentIdentity(t *testing.T) {
	mem := &MemorySource{SourceName: "inmemory", Values: map[string][]byte{
		"http_port": MustJSON(int64(8001)),
		"api_token": MustJSON("secret"),
		"feature_x": MustJSON(true),
	}}
	store, err := NewStore(testSchema(), mem)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	v1 := store.Snapshot()
	reloadWithPort(t, store, mem, 8002)
	mem.Values["feature_x"] = MustJSON(false)
	reloadWithPort(t, store, mem, 8003)

	if err := store.RollbackTo(1); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	cur := store.Snapshot()
	for _, k := range v1.Keys() {
		got, _ := cur.Get(k)
		want, _ := v1.Get(k)
		log.Printf("[compare] key=%s v1=%v current(v%d)=%v", k, want, cur.Version, got)
		if got != want {
			t.Fatalf("key %s: want v1 value %v, got %v", k, want, got)
		}
	}
}
