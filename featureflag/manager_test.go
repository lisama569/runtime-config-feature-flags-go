package featureflag

import (
	"errors"
	"log"
	"sync"
	"testing"
)

func rulesV1() []Flag {
	return []Flag{{Name: "g", Enabled: true, Rules: []Rule{
		{Priority: 10, Percentage: 50, Salt: "a"},
	}}}
}

func rulesV2() []Flag {
	// 调整 Salt 使分流域改变，便于区分版本。
	return []Flag{{Name: "g", Enabled: true, Rules: []Rule{
		{Priority: 10, Percentage: 50, Salt: "different-salt"},
	}}}
}

// TestManagerReplaceAtomicAndVersion 验证规则整体替换、版本递增，
// 非法规则被拒且当前规则集不变、错误分类与 Compile 一致。
func TestManagerReplaceAtomicAndVersion(t *testing.T) {
	m, err := NewManager(rulesV1())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.Version() != 1 {
		t.Fatalf("initial version: want 1, got %d", m.Version())
	}

	if _, err := m.Replace(rulesV2()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if m.Version() != 2 {
		t.Fatalf("version after replace: want 2, got %d", m.Version())
	}
	log.Printf("[replace] current version=%d", m.Version())

	// 非法规则（百分比越界）：必须拒绝且版本不变。
	bad := []Flag{{Name: "g", Enabled: true, Rules: []Rule{{Priority: 1, Percentage: 150}}}}
	_, err = m.Replace(bad)
	var fe *Error
	if !errors.As(err, &fe) || fe.Reason != ReasonInvalidRule {
		t.Fatalf("invalid replacement must be ReasonInvalidRule, got %v", err)
	}
	log.Printf("[rejected replace] reason=%s detail=%q", fe.Reason, fe.Detail)
	if m.Version() != 2 {
		t.Fatalf("rejected replace must not advance version, got %d", m.Version())
	}
	if m.Current() == nil {
		t.Fatal("current engine must survive rejected replace")
	}
}

// TestManagerReplayConsistency 验证按历史版本重放与当时结论完全一致，
// 不存在版本报 ReasonUnknownVersion。
func TestManagerReplayConsistency(t *testing.T) {
	m, err := NewManager(rulesV1())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// 记录 v1 对一批目标的判定。
	type result struct {
		target  string
		allowed bool
		bucket  int
		rule    int
	}
	var v1Results []result
	for i := 0; i < 200; i++ {
		tgt := "user-" + itoaT(i)
		d, err := m.Evaluate("g", tgt)
		if err != nil {
			t.Fatalf("Evaluate v1: %v", err)
		}
		v1Results = append(v1Results, result{tgt, d.Allowed, d.Bucket, d.RuleIndex})
	}

	if _, err := m.Replace(rulesV2()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	// 当前已为 v2：用 EvaluateAt(1) 重放必须逐字节还原 v1 结论。
	for _, want := range v1Results {
		d, err := m.EvaluateAt(1, "g", want.target)
		if err != nil {
			t.Fatalf("EvaluateAt(1): %v", err)
		}
		if d.Version != 1 || d.Allowed != want.allowed || d.Bucket != want.bucket || d.RuleIndex != want.rule {
			t.Fatalf("replay mismatch for %s: got v%d allowed=%v bucket=%d rule=%d; want v1 %+v",
				want.target, d.Version, d.Allowed, d.Bucket, d.RuleIndex, want)
		}
	}
	sample, _ := m.EvaluateAt(1, "g", "user-7")
	cur, _ := m.Evaluate("g", "user-7")
	log.Printf("[replay] v1: allowed=%v bucket=%d rule=%d; [current v2] allowed=%v bucket=%d",
		sample.Allowed, sample.Bucket, sample.RuleIndex, cur.Allowed, cur.Bucket)

	// 必须存在两版结论不同的目标，且重放各自还原本版结论。
	diffFound := false
	for i := 0; i < 1000 && !diffFound; i++ {
		tgt := "user-" + itoaT(i)
		a, _ := m.EvaluateAt(1, "g", tgt)
		b, _ := m.EvaluateAt(2, "g", tgt)
		if a.Allowed != b.Allowed || a.Bucket != b.Bucket {
			diffFound = true
			log.Printf("[version-diff] target=%s v1(allowed=%v bucket=%d) v2(allowed=%v bucket=%d)",
				tgt, a.Allowed, a.Bucket, b.Allowed, b.Bucket)
		}
	}
	if !diffFound {
		t.Fatal("expected some target to differ across rule versions")
	}

	// v2 自身也可重放。
	d, err := m.EvaluateAt(2, "g", "user-7")
	if err != nil || d.Version != 2 {
		t.Fatalf("EvaluateAt(2): %v d=%+v", err, d)
	}

	// 不存在的版本：明确报错，不回退当前版本。
	_, err = m.EvaluateAt(99, "g", "user-7")
	var fe *Error
	if !errors.As(err, &fe) || fe.Reason != ReasonUnknownVersion {
		t.Fatalf("EvaluateAt(99): want ReasonUnknownVersion, got %v", err)
	}
	log.Printf("[replay unknown] %v", err)
}

// TestManagerHotSwapConcurrent 竞态检测下并发判定与整体热替换交叉：
// 每次判定的版本必须与某个完整规则集自洽，且同一 (版本,目标) 结论稳定。
func TestManagerHotSwapConcurrent(t *testing.T) {
	m, err := NewManager(rulesV1())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 判定者：每次结论的 RuleIndex/Bucket 必须与其所报 Version 的规则一致，
	// 这里通过“同目标同版本重复重放”交叉验证当前判定没有读到两版混合状态。
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				tgt := "user-" + itoaT((w*31+i)%500)
				d, err := m.Evaluate("g", tgt)
				if err != nil {
					t.Errorf("Evaluate: %v", err)
					return
				}
				// 用该版本历史引擎重放：必须与本次判定逐项相同（证明读到的是整版规则）。
				rep, err := m.EvaluateAt(d.Version, "g", tgt)
				if err != nil {
					t.Errorf("EvaluateAt(%d): %v", d.Version, err)
					return
				}
				if rep != d {
					t.Errorf("mixed-version read: live=%+v replay=%+v", d, rep)
					return
				}
				if i > 400 {
					return
				}
			}
		}(w)
	}

	// 替换者：在 v1/v2 两版之间来回整体替换，版本持续递增。
	wg.Add(1)
	go func() {
		defer wg.Done()
		v1, v2 := rulesV1(), rulesV2()
		for i := 0; i < 50; i++ {
			var flags []Flag
			if i%2 == 0 {
				flags = v2
			} else {
				flags = v1
			}
			if _, err := m.Replace(flags); err != nil {
				t.Errorf("Replace: %v", err)
				return
			}
		}
		close(stop)
	}()

	wg.Wait()
	log.Printf("[hot-swap] final current version=%d; each live decision matched its version replay", m.Version())
}

// TestManagerSameVersionStable 验证同一目标在同一版规则下稳定可复现。
func TestManagerSameVersionStable(t *testing.T) {
	m, err := NewManager(rulesV1())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	first, err := m.Evaluate("g", "user-42")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for i := 0; i < 1000; i++ {
		d, err := m.Evaluate("g", "user-42")
		if err != nil || d != first {
			t.Fatalf("unstable same-version decision: %+v err=%v", d, err)
		}
	}
	log.Printf("[stable] version=%d allowed=%v bucket=%d rule=%d",
		first.Version, first.Allowed, first.Bucket, first.RuleIndex)
}
