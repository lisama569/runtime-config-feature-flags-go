package featureflag

import "fmt"

// Rule 是一条灰度规则：按目标 ID 的稳定哈希分流。
type Rule struct {
	Name       string
	Priority   int      // 同一开关内优先级必须唯一，数值小者先评估
	Percentage int      // 0-100，命中百分比
	Allowlist  []string // 白名单目标直接命中
}

// Flag 是一个功能开关。
type Flag struct {
	Name      string
	Enabled   bool
	Version   int64
	Rules     []Rule
	DependsOn []string // 依赖的其他开关，必须存在
}

// Registry 是开关集合，加载时整体校验。
type Registry struct {
	Flags map[string]Flag
}

// NewRegistry 构建并校验开关集合。
// 规则非法、优先级冲突或引用不存在的开关都会明确报错，绝不默认放行。
func NewRegistry(flags []Flag) (*Registry, error) {
	r := &Registry{Flags: make(map[string]Flag, len(flags))}
	for _, f := range flags {
		if f.Name == "" {
			return nil, fmt.Errorf("featureflag: flag with empty name")
		}
		if _, dup := r.Flags[f.Name]; dup {
			return nil, fmt.Errorf("featureflag: duplicate flag %q", f.Name)
		}
		r.Flags[f.Name] = f
	}
	for _, f := range flags {
		if err := r.validateFlag(f); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) validateFlag(f Flag) error {
	seen := make(map[int]string, len(f.Rules))
	for _, rule := range f.Rules {
		if rule.Percentage < 0 || rule.Percentage > 100 {
			return fmt.Errorf("featureflag: flag %q rule %q: percentage %d out of range [0,100]",
				f.Name, rule.Name, rule.Percentage)
		}
		if prev, dup := seen[rule.Priority]; dup {
			return fmt.Errorf("featureflag: flag %q: priority %d conflict between rules %q and %q",
				f.Name, rule.Priority, prev, rule.Name)
		}
		seen[rule.Priority] = rule.Name
	}
	for _, dep := range f.DependsOn {
		if _, ok := r.Flags[dep]; !ok {
			return fmt.Errorf("featureflag: flag %q depends on unknown flag %q", f.Name, dep)
		}
	}
	return nil
}
