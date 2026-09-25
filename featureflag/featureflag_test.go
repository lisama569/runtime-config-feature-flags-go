package featureflag

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := NewRegistry([]Flag{
		{Name: "base", Enabled: true, Version: 1},
		{Name: "new-checkout", Enabled: true, Version: 3, DependsOn: []string{"base"},
			Rules: []Rule{
				{Name: "vip", Priority: 1, Allowlist: []string{"user-vip-1"}},
				{Name: "gradual", Priority: 2, Percentage: 25},
			}},
		{Name: "off-flag", Enabled: false, Version: 1},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return reg
}

func TestBucketStability(t *testing.T) {
	reg := testRegistry(t)
	targets := []string{"user-1", "user-2", "user-42", "user-100", "user-vip-1"}
	first := make(map[string]Decision, len(targets))
	for _, tg := range targets {
		d, err := reg.Evaluate("new-checkout", tg)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		first[tg] = d
		t.Logf("input=(flag=new-checkout target=%s) flagVersion=%d bucket=%d enabled=%v reason=%q",
			tg, d.FlagVersion, d.Bucket, d.Enabled, d.Reason)
	}
	// 同一目标在同一规则版本下重复判定，结论必须稳定可复现。
	for round := 0; round < 100; round++ {
		for _, tg := range targets {
			d, err := reg.Evaluate("new-checkout", tg)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if d != first[tg] {
				t.Fatalf("unstable decision for %s: first=%+v now=%+v", tg, first[tg], d)
			}
		}
	}
	// 白名单目标必须命中。
	if !first["user-vip-1"].Enabled {
		t.Fatal("allowlisted target must be enabled")
	}
	// 版本变化后桶位可重算，但同一版本内必须稳定。
	regV4, err := NewRegistry([]Flag{
		{Name: "base", Enabled: true, Version: 1},
		{Name: "new-checkout", Enabled: true, Version: 4, DependsOn: []string{"base"},
			Rules: []Rule{{Name: "gradual", Priority: 1, Percentage: 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d4, err := regV4.Evaluate("new-checkout", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input=(flag=new-checkout target=user-1) flagVersion=%d bucket=%d enabled=%v reason=%q (version bump re-buckets deterministically)",
		d4.FlagVersion, d4.Bucket, d4.Enabled, d4.Reason)
}

func TestUnknownFlagRejected(t *testing.T) {
	reg := testRegistry(t)
	_, err := reg.Evaluate("no-such-flag", "user-1")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("want unknown flag error, got %v", err)
	}
	t.Logf("input=(flag=no-such-flag target=user-1) rejected: %v", err)
}

func TestInvalidRegistryRejected(t *testing.T) {
	cases := []struct {
		name  string
		flags []Flag
		want  string
	}{
		{"percentage_out_of_range", []Flag{
			{Name: "a", Enabled: true, Rules: []Rule{{Name: "r", Priority: 1, Percentage: 150}}},
		}, "out of range"},
		{"priority_conflict", []Flag{
			{Name: "a", Enabled: true, Rules: []Rule{
				{Name: "r1", Priority: 1, Percentage: 10},
				{Name: "r2", Priority: 1, Percentage: 20},
			}},
		}, "priority 1 conflict"},
		{"unknown_dependency", []Flag{
			{Name: "a", Enabled: true, DependsOn: []string{"ghost"}},
		}, "unknown flag"},
		{"duplicate_flag", []Flag{
			{Name: "a"}, {Name: "a"},
		}, "duplicate flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRegistry(tc.flags)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			t.Logf("input=%+v rejected: %v", tc.flags, err)
		})
	}
}

func TestDependencyGating(t *testing.T) {
	reg, err := NewRegistry([]Flag{
		{Name: "base", Enabled: false, Version: 1},
		{Name: "child", Enabled: true, Version: 1, DependsOn: []string{"base"},
			Rules: []Rule{{Name: "all", Priority: 1, Percentage: 100}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := reg.Evaluate("child", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Enabled {
		t.Fatal("child must be off when dependency is off")
	}
	t.Logf("input=(flag=child target=user-1) flagVersion=%d enabled=%v reason=%q",
		d.FlagVersion, d.Enabled, d.Reason)
}

func TestConcurrentEvaluateNoRace(t *testing.T) {
	reg := testRegistry(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := fmt.Sprintf("user-%d", i%50)
			for j := 0; j < 200; j++ {
				d, err := reg.Evaluate("new-checkout", target)
				if err != nil {
					t.Errorf("evaluate: %v", err)
					return
				}
				d2, _ := reg.Evaluate("new-checkout", target)
				if d != d2 {
					t.Errorf("unstable decision for %s", target)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
