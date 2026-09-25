package config

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testSchema() Schema {
	return Schema{
		"http_port": {Type: TypeInt, Required: true, Validate: func(v any) string {
			p := v.(int64)
			if p < 1 || p > 65535 {
				return "port out of range"
			}
			return ""
		}},
		"timeout_ms": {Type: TypeInt, Required: false},
		"feature_x":  {Type: TypeBool, Required: false},
		"api_token":  {Type: TypeString, Required: true, Sensitive: true},
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

// TestMergePriorityAndOverride 验证多来源按优先级覆盖且版本递增。
func TestMergePriorityAndOverride(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	override := filepath.Join(dir, "override.json")

	writeFile(t, base, `{"http_port": 8080, "timeout_ms": 1000, "api_token": "base-secret"}`)
	writeFile(t, override, `{"http_port": 9090, "feature_x": true}`)

	low := &FileSource{Path: base}
	high := &FileSource{Path: override}
	store, err := NewStore(testSchema(), low, high)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	snap := store.Snapshot()
	log.Printf("[input] low=%s high=%s; [effective] %s", low.Name(), high.Name(), snap.String(testSchema()))

	if got := snap.MustInt("http_port"); got != 9090 {
		t.Fatalf("http_port: want 9090 (high priority), got %d", got)
	}
	if got := snap.MustInt("timeout_ms"); got != 1000 {
		t.Fatalf("timeout_ms: want 1000 from low source, got %d", got)
	}
	if got := snap.MustBool("feature_x"); !got {
		t.Fatalf("feature_x: want true")
	}
	if snap.Version != 1 {
		t.Fatalf("initial version: want 1, got %d", snap.Version)
	}

	// 动态更新：高优先级来源改为 7070。
	writeFile(t, override, `{"http_port": 7070, "feature_x": true}`)
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	snap2 := store.Snapshot()
	log.Printf("[input] http_port=7070; [effective] %s", snap2.String(testSchema()))
	if snap2.MustInt("http_port") != 7070 {
		t.Fatalf("http_port after reload: want 7070, got %d", snap2.MustInt("http_port"))
	}
	if snap2.Version != 2 {
		t.Fatalf("version after reload: want 2, got %d", snap2.Version)
	}
	// 旧快照不可变。
	if snap.MustInt("http_port") != 9090 {
		t.Fatalf("old snapshot mutated: got %d", snap.MustInt("http_port"))
	}
}

// TestRejectReasons 验证各类非法配置的错误原因可区分，且不静默回退默认值。
func TestRejectReasons(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")

	cases := []struct {
		name   string
		body   string
		reason Reason
	}{
		{"missing required", `{"http_port": 8080}`, ReasonMissingRequired}, // api_token 缺失
		{"type mismatch", `{"http_port": "eight", "api_token": "t"}`, ReasonTypeMismatch},
		{"invalid value", `{"http_port": 70000, "api_token": "t"}`, ReasonInvalidValue},
		{"unknown key", `{"http_port": 80, "api_token": "t", "bogus": 1}`, ReasonInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, f, tc.body)
			log.Printf("[input] %s", tc.body)
			_, err := NewStore(testSchema(), &FileSource{Path: f})
			var re *RejectError
			if !errors.As(err, &re) {
				t.Fatalf("want *RejectError, got %v", err)
			}
			log.Printf("[rejected] reason=%s key=%s detail=%q", re.Reason, re.Key, re.Detail)
			if re.Reason != tc.reason {
				t.Fatalf("reason: want %s, got %s", tc.reason, re.Reason)
			}
		})
	}
}

// TestSensitiveRedaction 验证敏感字段不出现在快照日志与错误信息中。
func TestSensitiveRedaction(t *testing.T) {
	const secret = "super-secret-token"
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	writeFile(t, f, `{"http_port": 8080, "api_token": "`+secret+`"}`)
	store, err := NewStore(testSchema(), &FileSource{Path: f})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rendered := store.Snapshot().String(testSchema())
	log.Printf("[effective] %s", rendered)
	if strings.Contains(rendered, secret) {
		t.Fatalf("secret leaked in snapshot rendering: %s", rendered)
	}
	if !strings.Contains(rendered, "<redacted>") {
		t.Fatalf("expected redaction marker: %s", rendered)
	}

	// 敏感字段类型错误时，错误信息也不得包含值。
	writeFile(t, f, `{"http_port": 8080, "api_token": 12345}`)
	err = store.Reload()
	if err == nil || strings.Contains(err.Error(), "12345") {
		t.Fatalf("error must not contain sensitive value, got: %v", err)
	}
	log.Printf("[rejected] %v", err)
}

// TestRollbackKeepsPrevious 验证更新失败时整体放弃并保留上一份配置。
func TestRollbackKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	mem := &MemorySource{SourceName: "inmemory", Values: map[string][]byte{}}

	writeFile(t, f, `{"http_port": 8080, "api_token": "first-secret"}`)
	store, err := NewStore(testSchema(), &FileSource{Path: f}, mem)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	before := store.Snapshot()
	log.Printf("[before] %s", before.String(testSchema()))

	// 内存源下发一个非法端口：更新必须失败并回滚。
	mem.Values["http_port"] = MustJSON(int64(-1))
	if err := store.Reload(); err == nil {
		t.Fatal("reload with invalid value must fail")
	} else {
		log.Printf("[rollback] rejected: %v", err)
	}
	after := store.Snapshot()
	if after != before {
		t.Fatal("expected snapshot pointer unchanged after failed reload")
	}
	if after.MustInt("http_port") != 8080 {
		t.Fatalf("after failed reload port: want 8080, got %d", after.MustInt("http_port"))
	}
	log.Printf("[after] %s", after.String(testSchema()))

	// 内存源源读取失败同样回滚。
	mem.Values = nil
	if err := store.Reload(); err == nil {
		t.Fatal("reload with broken source must fail")
	}
	if store.Snapshot() != before {
		t.Fatal("expected snapshot unchanged after source failure")
	}
}

// TestConcurrentReadReload 在竞态检测下并发读取与更新。
func TestConcurrentReadReload(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	writeFile(t, f, `{"http_port": 8080, "api_token": "secret"}`)
	store, err := NewStore(testSchema(), &FileSource{Path: f})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 多个读取者：只能观察到完整快照，端口永远是合法值。
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					snap := store.Snapshot()
					p := snap.MustInt("http_port")
					tok := snap.MustString("api_token")
					if p < 1 || tok == "" {
						t.Errorf("observed half-updated snapshot: port=%d token=%q", p, tok)
						return
					}
				}
			}
		}()
	}

	// 更新者：交替写入合法值（生效）与非法值（回滚）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		ports := []int64{9000, 9001, 9002}
		for i := 0; i < 200; i++ {
			writeFile(t, f, `{"http_port": `+itoa64(ports[i%3])+`, "api_token": "secret"}`)
			_ = store.Reload()
			writeFile(t, f, `{"http_port": "bad", "api_token": "secret"}`)
			_ = store.Reload() // 必失败并回滚
		}
		close(stop)
	}()
	wg.Wait()
	log.Printf("[effective] final %s", store.Snapshot().String(testSchema()))
}

func itoa64(n int64) string {
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
