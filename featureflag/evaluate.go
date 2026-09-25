package featureflag

import (
	"fmt"
	"hash/fnv"
	"sort"
)

// Decision 记录一次判定的结论与依据。
type Decision struct {
	Flag        string
	Target      string
	Enabled     bool
	FlagVersion int64
	Bucket      int    // 目标落入的桶 0-99，由稳定哈希决定
	Reason      string // 判定依据，可解释
}

// bucket 对 (flag, version, target) 做 FNV-1a 哈希并映射到 0-99。
// 同一目标在同一规则版本下结果稳定可复现。
func bucket(flagName string, version int64, target string) int {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s:%d:%s", flagName, version, target)
	return int(h.Sum32() % 100)
}

// Evaluate 判定 target 是否命中 flag，结论稳定可复现。
// 开关不存在时明确报错，绝不默认放行。
func (r *Registry) Evaluate(flagName, target string) (Decision, error) {
	f, ok := r.Flags[flagName]
	if !ok {
		return Decision{}, fmt.Errorf("featureflag: unknown flag %q", flagName)
	}
	d := Decision{Flag: flagName, Target: target, FlagVersion: f.Version}
	if !f.Enabled {
		d.Reason = "flag is globally disabled"
		return d, nil
	}
	// 依赖的开关必须先命中，否则不生效。
	for _, dep := range f.DependsOn {
		depDec, err := r.Evaluate(dep, target)
		if err != nil {
			return Decision{}, err
		}
		if !depDec.Enabled {
			d.Reason = fmt.Sprintf("dependency %q not enabled for target", dep)
			return d, nil
		}
	}
	// 按优先级升序评估规则，先命中者生效。
	rules := make([]Rule, len(f.Rules))
	copy(rules, f.Rules)
	sort.Slice(rules, func(i, j int) bool { return rules[i].Priority < rules[j].Priority })
	if len(rules) == 0 {
		// 无规则的全局开关：Enabled=true 即对全部目标生效。
		d.Enabled = true
		d.Reason = "flag enabled with no rules (on for all targets)"
		return d, nil
	}
	d.Bucket = bucket(f.Name, f.Version, target)
	for _, rule := range rules {
		for _, allowed := range rule.Allowlist {
			if allowed == target {
				d.Enabled = true
				d.Reason = fmt.Sprintf("rule %q: target in allowlist", rule.Name)
				return d, nil
			}
		}
		if rule.Percentage > 0 {
			if d.Bucket < rule.Percentage {
				d.Enabled = true
				d.Reason = fmt.Sprintf("rule %q: bucket %d < percentage %d",
					rule.Name, d.Bucket, rule.Percentage)
			} else {
				d.Reason = fmt.Sprintf("rule %q: bucket %d >= percentage %d",
					rule.Name, d.Bucket, rule.Percentage)
			}
			return d, nil
		}
	}
	d.Reason = "no rule matched"
	return d, nil
}
