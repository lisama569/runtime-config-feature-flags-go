package featureflag

import (
	"errors"
	"log"
	"sync"
	"testing"
)

func testFlags() []Flag {
	return []Flag{
		{Name: "checkout_v2", Enabled: true, Rules: []Rule{
			{Priority: 10, Percentage: 50, Salt: "a"},
			{Priority: 5, Percentage: 80, Salt: "b"},
		}},
		{Name: "disabled_flag", Enabled: false, Rules: []Rule{
			{Priority: 1, Percentage: 100},
		}},
		{Name: "no_rules", Enabled: true},
	}
}

// TestEvaluateStability 同一目标在同一规则版本下结论稳定可复现，
// 且不同目标的分流比例大致符合百分比配置。
func TestEvaluateStability(t *testing.T) {
	e, err := Compile([]Flag{{Name: "g", Enabled: true, Rules: []Rule{{Priority: 1, Percentage: 30}}}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	target := "user-42"
	first, err := e.Evaluate("g", target)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	log.Printf("[input] flag=g target=%s; [version] %d; [decision] allowed=%v bucket=%d rule=%d; [explain] %s",
		target, first.Version, first.Allowed, first.Bucket, first.RuleIndex, first.Explain)

	// 重复判定（含并发）必须得到完全一致的结果。
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				d, err := e.Evaluate("g", target)
				if err != nil {
					t.Errorf("Evaluate: %v", err)
					return
				}
				if d != first {
					t.Errorf("unstable decision: got %+v, want %+v", d, first)
					return
				}
			}
		}()
	}
	wg.Wait()

	// 统计 10000 个目标的命中率，应接近 30%。
	hits := 0
	const n = 10000
	for i := 0; i < n; i++ {
		d, err := e.Evaluate("g", "user-"+itoaT(i))
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if d.Allowed {
			hits++
		}
	}
	ratio := float64(hits) / n
	log.Printf("[stats] targets=%d hits=%d ratio=%.3f (configured 0.30)", n, hits, ratio)
	if ratio < 0.25 || ratio > 0.35 {
		t.Fatalf("bucket ratio %.3f far from configured 0.30", ratio)
	}
}

// TestEvaluateExplainAndVersion 验证判定依据可解释，且版本参与分流。
func TestEvaluateExplainAndVersion(t *testing.T) {
	e1, err := CompileWithVersion(testFlags(), 1)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	e2, err := CompileWithVersion(testFlags(), 2)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	d, err := e1.Evaluate("checkout_v2", "user-7")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	log.Printf("[input] flag=checkout_v2 target=user-7; [version] %d; [decision] %+v; [explain] %s",
		d.Version, d, d.Explain)
	if d.Version != 1 {
		t.Fatalf("version: want 1, got %d", d.Version)
	}
	if d.Explain == "" {
		t.Fatal("explain must not be empty")
	}

	// 版本变化允许分流结果变化，但同版本内必须稳定。
	diff := 0
	for i := 0; i < 1000; i++ {
		tgt := "user-" + itoa(i)
		a, _ := e1.Evaluate("checkout_v2", tgt)
		b, _ := e2.Evaluate("checkout_v2", tgt)
		if a.Allowed != b.Allowed {
			diff++
		}
	}
	log.Printf("[version-sensitivity] decisions differing across v1/v2: %d/1000", diff)
	if diff == 0 {
		t.Fatal("expected version to participate in bucketing")
	}

	// 禁用开关：明确拒绝且可解释。
	d, err = e1.Evaluate("disabled_flag", "user-7")
	if err != nil {
		t.Fatalf("Evaluate disabled: %v", err)
	}
	log.Printf("[input] flag=disabled_flag; [decision] allowed=%v; [explain] %s", d.Allowed, d.Explain)
	if d.Allowed {
		t.Fatal("disabled flag must not allow")
	}

	// 无规则开关：默认不放行。
	d, err = e1.Evaluate("no_rules", "user-7")
	if err != nil {
		t.Fatalf("Evaluate no_rules: %v", err)
	}
	log.Printf("[input] flag=no_rules; [decision] allowed=%v; [explain] %s", d.Allowed, d.Explain)
	if d.Allowed {
		t.Fatal("flag without rules must not allow by default")
	}
}

// TestRulePriority 验证规则按优先级从高到低依次判定，第一条命中的规则决定结论。
func TestRulePriority(t *testing.T) {
	// 两条规则都 100% 命中：命中的必须是优先级更高的那条（排序后下标 0）。
	e, err := Compile([]Flag{{Name: "f", Enabled: true, Rules: []Rule{
		{Priority: 1, Percentage: 100, Salt: "low"},
		{Priority: 2, Percentage: 100, Salt: "high"},
	}}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	d, err := e.Evaluate("f", "anyone")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	log.Printf("[input] rules=[p2:100%%, p1:100%%]; [decision] %+v; [explain] %s", d, d.Explain)
	if !d.Allowed || d.RuleIndex != 0 {
		t.Fatalf("expected highest-priority rule (index 0) to win, got %+v", d)
	}

	// 高优先级规则百分比为 0（永不命中）时，才轮到低优先级 100% 规则。
	e2, err := Compile([]Flag{{Name: "f", Enabled: true, Rules: []Rule{
		{Priority: 2, Percentage: 0, Salt: "high"},
		{Priority: 1, Percentage: 100, Salt: "low"},
	}}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	d, err = e2.Evaluate("f", "anyone")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	log.Printf("[input] rules=[p2:0%%, p1:100%%]; [decision] %+v; [explain] %s", d, d.Explain)
	if !d.Allowed || d.RuleIndex != 1 {
		t.Fatalf("expected fallthrough to lower-priority rule (index 1), got %+v", d)
	}
}

// TestCompileErrors 验证非法规则、优先级冲突、未知开关的错误分类。
func TestCompileErrors(t *testing.T) {
	cases := []struct {
		name   string
		flags  []Flag
		reason Reason
	}{
		{"percentage out of range", []Flag{{Name: "a", Enabled: true, Rules: []Rule{{Priority: 1, Percentage: 101}}}}, ReasonInvalidRule},
		{"negative percentage", []Flag{{Name: "a", Enabled: true, Rules: []Rule{{Priority: 1, Percentage: -1}}}}, ReasonInvalidRule},
		{"priority conflict", []Flag{{Name: "a", Enabled: true, Rules: []Rule{
			{Priority: 1, Percentage: 10}, {Priority: 1, Percentage: 20}}}}, ReasonPriorityConflict},
		{"duplicate flag", []Flag{{Name: "a"}, {Name: "a"}}, ReasonInvalidRule},
		{"empty name", []Flag{{Name: ""}}, ReasonInvalidRule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log.Printf("[input] flags=%+v", tc.flags)
			_, err := Compile(tc.flags)
			var fe *Error
			if !errors.As(err, &fe) {
				t.Fatalf("want *Error, got %v", err)
			}
			log.Printf("[rejected] reason=%s flag=%q detail=%q", fe.Reason, fe.Flag, fe.Detail)
			if fe.Reason != tc.reason {
				t.Fatalf("reason: want %s, got %s", tc.reason, fe.Reason)
			}
		})
	}
}

// TestUnknownFlagNotAllowed 引用不存在的开关必须报错而非默认放行。
func TestUnknownFlagNotAllowed(t *testing.T) {
	e, err := Compile(testFlags())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = e.Evaluate("does_not_exist", "user-1")
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("want *Error, got %v", err)
	}
	log.Printf("[input] flag=does_not_exist; [rejected] reason=%s detail=%q", fe.Reason, fe.Detail)
	if fe.Reason != ReasonUnknownFlag {
		t.Fatalf("reason: want %s, got %s", ReasonUnknownFlag, fe.Reason)
	}
}

// TestConcurrentEvaluate 竞态检测下并发判定多个开关。
func TestConcurrentEvaluate(t *testing.T) {
	e, err := Compile(testFlags())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	flags := []string{"checkout_v2", "disabled_flag", "no_rules"}
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				d, err := e.Evaluate(flags[(w+i)%len(flags)], "user-"+itoaT(i))
				if err != nil {
					t.Errorf("Evaluate: %v", err)
					return
				}
				if d.Version != e.Version() {
					t.Errorf("decision version %d != engine version %d", d.Version, e.Version())
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

func itoaT(n int) string {
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
