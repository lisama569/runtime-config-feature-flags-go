package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testSchema() Schema {
	return Schema{Fields: []Field{
		{Name: "app.name", Type: TypeString, Required: true},
		{Name: "app.port", Type: TypeInt, Required: true,
			Validate: func(v any) error {
				if p := int64(v.(float64)); p < 1 || p > 65535 {
					return fmt.Errorf("port %d out of range", p)
				}
				return nil
			}},
		{Name: "db.password", Type: TypeString, Required: true, Sensitive: true},
		{Name: "debug", Type: TypeBool},
	}}
}

func writeJSON(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func baseFiles(t *testing.T) (dir string, base, override string) {
	t.Helper()
	dir = t.TempDir()
	base = writeJSON(t, dir, "base.json",
		`{"app.name":"svc","app.port":8080,"db.password":"s3cret","debug":false}`)
	override = writeJSON(t, dir, "override.json", `{"app.port":9090}`)
	return
}

func TestLayeredMergePriority(t *testing.T) {
	_, base, override := baseFiles(t)
	loader := &Loader{
		Sources: []Source{
			FileSource{Path: base},
			FileSource{Path: override},
			MemorySource{Label: "final", Values: map[string]any{"debug": true}},
		},
		Schema: testSchema(),
	}
	snap, err := loader.Load(1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	port, _ := snap.Get("app.port")
	if port != 9090.0 {
		t.Fatalf("override source should win, got port=%v", port)
	}
	dbg, _ := snap.Get("debug")
	if dbg != true {
		t.Fatalf("memory source should win, got debug=%v", dbg)
	}
	name, _ := snap.GetString("app.name")
	if name != "svc" {
		t.Fatalf("base value should survive, got name=%q", name)
	}
	t.Logf("input=base.json+override.json+memory version=%d merged=%v(port hidden keys ok)",
		snap.Version, "app.port=9090 debug=true app.name=svc")
}

func TestRejectReasonsAreDistinct(t *testing.T) {
	cases := []struct {
		name string
		json string
		kind RejectKind
	}{
		{"type_mismatch", `{"app.name":123,"app.port":8080,"db.password":"x"}`, KindTypeMismatch},
		{"missing_required", `{"app.name":"svc","db.password":"x"}`, KindMissingRequired},
		{"invalid_value", `{"app.name":"svc","app.port":70000,"db.password":"x"}`, KindInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeJSON(t, t.TempDir(), "bad.json", tc.json)
			loader := &Loader{Sources: []Source{FileSource{Path: p}}, Schema: testSchema()}
			_, err := loader.Load(1)
			var re *RejectError
			if !errors.As(err, &re) {
				t.Fatalf("want *RejectError, got %v", err)
			}
			if re.Kind != tc.kind {
				t.Fatalf("want kind %v, got %v", tc.kind, re.Kind)
			}
			t.Logf("input=%s rejected kind=%s reason=%q", tc.json, re.Kind, re.Error())
		})
	}
}

func TestSensitiveFieldRedacted(t *testing.T) {
	p := writeJSON(t, t.TempDir(), "bad.json",
		`{"app.name":"svc","app.port":8080,"db.password":12345}`)
	loader := &Loader{Sources: []Source{FileSource{Path: p}}, Schema: testSchema()}
	_, err := loader.Load(1)
	if err == nil {
		t.Fatal("want rejection")
	}
	if strings.Contains(err.Error(), "12345") {
		t.Fatalf("sensitive value leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("want redacted marker, got: %v", err)
	}
	t.Logf("input=db.password:12345 rejected reason=%q (value redacted)", err.Error())
}

func TestUpdateRollbackOnInvalidConfig(t *testing.T) {
	_, base, override := baseFiles(t)
	loader := &Loader{
		Sources: []Source{FileSource{Path: base}, FileSource{Path: override}},
		Schema:  testSchema(),
	}
	store, err := NewStore(loader)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	v1 := store.Current().Version

	// 写入非法配置：端口越界。
	if err := os.WriteFile(override, []byte(`{"app.port":99999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = store.Update()
	var re *RejectError
	if !errors.As(err, &re) || re.Kind != KindInvalidValue {
		t.Fatalf("want invalid_value rejection, got %v", err)
	}
	cur := store.Current()
	if cur.Version != v1 {
		t.Fatalf("failed update must not bump version: v1=%d now=%d", v1, cur.Version)
	}
	port, _ := cur.Get("app.port")
	if port != 9090.0 {
		t.Fatalf("failed update must keep old config, got port=%v", port)
	}
	t.Logf("input=app.port:99999 rejected kind=%s version stays %d (rollback ok)", re.Kind, cur.Version)

	// 修复后更新应原子生效。
	if err := os.WriteFile(override, []byte(`{"app.port":7070}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(); err != nil {
		t.Fatalf("update: %v", err)
	}
	cur = store.Current()
	port, _ = cur.Get("app.port")
	if cur.Version != v1+1 || port != 7070.0 {
		t.Fatalf("want version=%d port=7070, got version=%d port=%v", v1+1, cur.Version, port)
	}
	t.Logf("input=app.port:7070 applied version=%d", cur.Version)
}

func TestConcurrentReadWriteNoRace(t *testing.T) {
	_, base, override := baseFiles(t)
	loader := &Loader{
		Sources: []Source{FileSource{Path: base}, FileSource{Path: override}},
		Schema:  testSchema(),
	}
	store, err := NewStore(loader)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	var readers, writers sync.WaitGroup
	stop := make(chan struct{})
	// 读方：持续读取，快照必须始终完整（要么旧要么新，不得半更新）。
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := store.Current()
				port, ok1 := snap.Get("app.port")
				name, ok2 := snap.GetString("app.name")
				if !ok1 || !ok2 || name != "svc" {
					t.Errorf("observed half-updated snapshot: port=%v name=%q", port, name)
					return
				}
			}
		}()
	}
	// 写方：交替写入两份合法配置并更新。
	ports := []int{7070, 9090}
	for i := 0; i < 20; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			content := fmt.Sprintf(`{"app.port":%d}`, ports[i%2])
			if err := os.WriteFile(override, []byte(content), 0o600); err != nil {
				return
			}
			_ = store.Update() // 与并发写文件竞争导致的失败可接受
		}(i)
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	t.Logf("concurrent test done, final version=%d", store.Current().Version)
}
