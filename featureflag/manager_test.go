package featureflag

import (
	"errors"
	"log"
	"sync"
	"testing"
)

// TestManagerHotSwapConcurrent 验证热替换期间并发判定只会看到
// 替换前或替换后某一整版规则，绝不混杂；同版本内结论稳定。
func TestManagerHotSwapConcurrent(t *testing.T) {
	// 保留窗口（64）大于更新次数（50），测试期间任何已生效版本都不会被淘汰，
	// 因此每次实时判定都必须能按同版本重放出完全一致的结果。
	m, err := NewManager([]Flag{{Name: "g", Enabled: true, Rules: []Rule{
		{Priority: 1, Percentage: 50, Salt: "v1"},
	}}}, 64)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// 判定者：每次结论必须属于某个完整版本（版本号与判定内容自洽）。
	targets := []string{"user-1", "user-2", "user-3", "user-4"}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				tgt := targets[(w+i)%len(targets)]
				d, err := m.Evaluate("g", tgt)
				if err != nil {
					t.Errorf("Evaluate: %v", err)
					return
				}
				// 用同版本重放校验：结论必须与该版本规则集独立重算一致。
				replay, err := m.EvaluateAt(d.Version, "g", tgt)
				if err != nil {
					t.Errorf("EvaluateAt(%d): %v", d.Version, err)
					return
				}
				if replay != d {
					t.Errorf("mixed-version observation: live=%+v replay=%+v", d, replay)
					return
				}
			}
		}(w)
	}

	// 更新者：反复在两套规则间热替换。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			salt := "v1"
			pct := 50
			if i%2 == 1 {
				salt, pct = "v2", 70
			}
			if err := m.Update([]Flag{{Name: "g", Enabled: true, Rules: []Rule{
				{Priority: 1, Percentage: pct, Salt: salt},
			}}}); err != nil {
				t.Errorf("Update: %v", err)
				return
			}
		}
		close(stop)
	}()
	wg.Wait()
	log.Printf("[hot-swap] final rules version=%d", m.Version())
	if m.Version() != 51 {
		t.Fatalf("version: want 51, got %d", m.Version())
	}
}

// TestManagerReplayConsistency 验证按历史版本重放的判定与当时实时判定完全一致。
func TestManagerReplayConsistency(t *testing.T) {
	m, err := NewManager(testFlags(), 8)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// 记录 v1 下若干目标的实时判定。
	targets := []string{"user-7", "user-42", "user-100"}
	live := map[string]Decision{}
	for _, tgt := range targets {
		d, err := m.Evaluate("checkout_v2", tgt)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		live[tgt] = d
		log.Printf("[live] version=%d target=%s allowed=%v bucket=%d; [explain] %s",
			d.Version, tgt, d.Allowed, d.Bucket, d.Explain)
	}

	// 热更新两版规则。
	if err := m.Update([]Flag{{Name: "checkout_v2", Enabled: true, Rules: []Rule{
		{Priority: 10, Percentage: 90, Salt: "new"},
	}}}); err != nil {
		t.Fatalf("Update to v2: %v", err)
	}
	if err := m.Update(testFlags()); err != nil {
		t.Fatalf("Update to v3: %v", err)
	}

	// 按 v1 重放：必须与当时一致。
	for _, tgt := range targets {
		d, err := m.EvaluateAt(1, "checkout_v2", tgt)
		if err != nil {
			t.Fatalf("EvaluateAt(1): %v", err)
		}
		log.Printf("[replay] version=%d target=%s allowed=%v bucket=%d; [explain] %s",
			d.Version, tgt, d.Allowed, d.Bucket, d.Explain)
		if d != live[tgt] {
			t.Fatalf("replay mismatch for %s: live=%+v replay=%+v", tgt, live[tgt], d)
		}
	}
}

// TestManagerUnknownVersion 验证重放不存在/未保留的版本时明确报错。
func TestManagerUnknownVersion(t *testing.T) {
	m, err := NewManager(testFlags(), 1) // 只保留 1 个历史版本
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_ = m.Update(testFlags()) // v2
	_ = m.Update(testFlags()) // v3，v1 被淘汰

	for _, v := range []uint64{0, 1, 99} {
		_, err := m.EvaluateAt(v, "checkout_v2", "user-1")
		var fe *Error
		if !errors.As(err, &fe) {
			t.Fatalf("EvaluateAt(%d): want *Error, got %v", v, err)
		}
		log.Printf("[replay-rejected] version=%d reason=%s detail=%q", v, fe.Reason, fe.Detail)
		if fe.Reason != ReasonUnknownVersion {
			t.Fatalf("reason: want %s, got %s", ReasonUnknownVersion, fe.Reason)
		}
	}
}

// TestManagerUpdateValidation 验证热更新沿用原有校验与错误分类：
// 非法规则照样被拒、原因可区分，且当前规则集不变。
func TestManagerUpdateValidation(t *testing.T) {
	m, err := NewManager(testFlags(), 4)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	before := m.Version()

	cases := []struct {
		name   string
		flags  []Flag
		reason Reason
	}{
		{"percentage out of range", []Flag{{Name: "a", Enabled: true, Rules: []Rule{{Priority: 1, Percentage: 101}}}}, ReasonInvalidRule},
		{"priority conflict", []Flag{{Name: "a", Enabled: true, Rules: []Rule{
			{Priority: 1, Percentage: 10}, {Priority: 1, Percentage: 20}}}}, ReasonPriorityConflict},
		{"duplicate flag", []Flag{{Name: "a"}, {Name: "a"}}, ReasonInvalidRule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log.Printf("[input] flags=%+v", tc.flags)
			err := m.Update(tc.flags)
			var fe *Error
			if !errors.As(err, &fe) {
				t.Fatalf("want *Error, got %v", err)
			}
			log.Printf("[rejected] reason=%s detail=%q", fe.Reason, fe.Detail)
			if fe.Reason != tc.reason {
				t.Fatalf("reason: want %s, got %s", tc.reason, fe.Reason)
			}
			if m.Version() != before {
				t.Fatalf("failed update must not bump version: want %d, got %d", before, m.Version())
			}
		})
	}

	// 拒绝后当前规则集仍可用且结论不变。
	d, err := m.Evaluate("checkout_v2", "user-7")
	if err != nil {
		t.Fatalf("Evaluate after rejected updates: %v", err)
	}
	log.Printf("[effective] version=%d decision=%+v", m.Version(), d)
	if d.Version != before {
		t.Fatalf("decision version: want %d, got %d", before, d.Version)
	}
}
