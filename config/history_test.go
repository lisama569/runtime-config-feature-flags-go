package config

import (
	"errors"
	"log"
	"path/filepath"
	"testing"
)

func reloadPort(t *testing.T, store *Store, f string, port int64) {
	t.Helper()
	writeFile(t, f, `{"http_port": `+itoa64(port)+`, "api_token": "s"}`)
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload port=%d: %v", port, err)
	}
}

// TestHistoryRetentionAndRead 验证历史窗口保留最近 N 个版本，
// 超出后最老版本淘汰，且 not-found 与 evicted 两类错误可区分。
func TestHistoryRetentionAndRead(t *testing.T) {
	store, f := newTestStore(t, `{"http_port": 8080, "api_token": "s"}`, WithHistoryRetain(3))
	// v1=8080(初始), v2=8081, v3=8082, v4=8083；保留窗口 [v2,v3,v4]，v1 淘汰。
	reloadPort(t, store, f, 8081)
	reloadPort(t, store, f, 8082)
	reloadPort(t, store, f, 8083)

	if cur := store.Snapshot(); cur.Version != 4 {
		t.Fatalf("current version: want 4, got %d", cur.Version)
	}

	v3, err := store.HistoryVersion(3)
	if err != nil {
		t.Fatalf("HistoryVersion(3): %v", err)
	}
	log.Printf("[read] v3 port=%d (immutable)", v3.MustInt("http_port"))
	if v3.MustInt("http_port") != 8082 {
		t.Fatalf("v3 port: want 8082, got %d", v3.MustInt("http_port"))
	}

	// v1 已淘汰。
	_, err = store.HistoryVersion(1)
	var evicted *VersionEvictedError
	if !errors.As(err, &evicted) {
		t.Fatalf("v1: want VersionEvictedError, got %v", err)
	}
	log.Printf("[evicted] %v", err)

	// v9 从未存在。
	_, err = store.HistoryVersion(9)
	var notFound *VersionNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("v9: want VersionNotFoundError, got %v", err)
	}
	log.Printf("[not-found] %v", err)

	// 取出的历史快照不受后续更新影响（不可变）。
	reloadPort(t, store, f, 8084) // v5，窗口变 [v3,v4,v5]
	if v3.MustInt("http_port") != 8082 {
		t.Fatalf("historical snapshot mutated: got %d", v3.MustInt("http_port"))
	}
	_, err = store.HistoryVersion(2)
	var ev2 *VersionEvictedError
	if !errors.As(err, &ev2) {
		t.Fatalf("v2 should now be evicted, got %v", err)
	}
}

// TestRollbackAdvancesVersionAndNotifies 验证回滚内容成为当前配置、
// 版本号继续前进不倒退，并像正常更新一样发出变动明细。
func TestRollbackAdvancesVersionAndNotifies(t *testing.T) {
	store, f := newTestStore(t, `{"http_port": 8080, "api_token": "s"}`, WithHistoryRetain(10))
	sub := store.Subscribe(4) // 订阅在初始加载之后，不补发历史事件

	reloadPort(t, store, f, 9090) // v2
	reloadPort(t, store, f, 9091) // v3
	recv(t, sub)                  // v2 事件
	recv(t, sub)                  // v3 事件

	// 回滚到 v1（8080）。
	if err := store.Rollback(1); err != nil {
		t.Fatalf("Rollback(1): %v", err)
	}
	cur := store.Snapshot()
	log.Printf("[rollback] current %s", cur.String(testSchema()))
	if cur.Version != 4 {
		t.Fatalf("version after rollback: want 4 (monotonic), got %d", cur.Version)
	}
	if cur.MustInt("http_port") != 8080 {
		t.Fatalf("content after rollback: want 8080, got %d", cur.MustInt("http_port"))
	}

	rev := recv(t, sub)
	if rev.Version != 4 || rev.Snapshot != cur {
		t.Fatalf("rollback must notify like a normal update, got v%d", rev.Version)
	}
	found := false
	for _, c := range rev.Changes {
		log.Printf("[rollback-change] key=%s kind=%s old=%v new=%v", c.Key, c.Kind, c.Old, c.New)
		if c.Key == "http_port" && c.Kind == ChangeUpdated && c.Old == int64(9091) && c.New == int64(8080) {
			found = true
		}
	}
	if !found {
		t.Fatal("rollback event must describe the port change back to 8080")
	}

	// 回滚后的版本也可读、可再回滚。
	got, err := store.HistoryVersion(4)
	if err != nil || got.MustInt("http_port") != 8080 {
		t.Fatalf("post-rollback v4 should be readable with port 8080, got %v", err)
	}
}

// TestRollbackWorksWhenSourceBroken 回滚不读来源：来源被删/改坏后回滚仍成功。
func TestRollbackWorksWhenSourceBroken(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	writeFile(t, f, `{"http_port": 8080, "api_token": "good-secret"}`)
	mem := &MemorySource{SourceName: "inmemory", Values: map[string][]byte{
		"http_port": MustJSON(int64(9090)),
	}}
	store, err := NewStoreWithOptions(testSchema(),
		[]Source{&FileSource{Path: f}, mem}, WithHistoryRetain(10))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if p := store.Snapshot().MustInt("http_port"); p != 9090 {
		t.Fatalf("memory override: want 9090, got %d", p)
	}
	// 再下发一次，形成多个历史版本。
	mem.Values["http_port"] = MustJSON(int64(9091))
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	vPrev := store.Snapshot().Version - 1 // 上一版本号（端口 9090）

	// 让所有来源失效：文件内容改坏 + 内存源置 nil。
	writeFile(t, f, "{not-json")
	mem.Values = nil
	if err := store.Reload(); err == nil {
		t.Fatal("reload with broken sources must fail")
	} else {
		log.Printf("[sources-broken] reload rejected: %v", err)
	}

	// 回滚到上一版本：必须成功，且内容就是当时那份（端口取内存源、token 取文件）。
	if err := store.Rollback(vPrev); err != nil {
		t.Fatalf("rollback must succeed despite broken sources: %v", err)
	}
	cur := store.Snapshot()
	log.Printf("[rollback despite broken sources] %s", cur.String(testSchema()))
	if cur.MustInt("http_port") != 9090 || cur.MustString("api_token") != "good-secret" {
		t.Fatalf("rollback content wrong: port=%d token=%q", cur.MustInt("http_port"), cur.MustString("api_token"))
	}
}

// TestRollbackUnknownErrors 验证回滚不存在/已淘汰版本返回与读取一致的错误分类。
func TestRollbackUnknownErrors(t *testing.T) {
	store, f := newTestStore(t, `{"http_port": 8080, "api_token": "s"}`, WithHistoryRetain(1))
	reloadPort(t, store, f, 8081) // v2；窗口仅 v2，v1 淘汰

	if err := store.Rollback(99); err == nil {
		t.Fatal("rollback to non-existent version must fail")
	} else {
		var nf *VersionNotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("want VersionNotFoundError, got %v", err)
		}
		log.Printf("[rollback not-found] %v", err)
	}
	if err := store.Rollback(1); err == nil {
		t.Fatal("rollback to evicted version must fail")
	} else {
		var ev *VersionEvictedError
		if !errors.As(err, &ev) {
			t.Fatalf("want VersionEvictedError, got %v", err)
		}
		log.Printf("[rollback evicted] %v", err)
	}
}
