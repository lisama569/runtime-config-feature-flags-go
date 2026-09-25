package featureflag

import (
	"hash/fnv"
	"sort"
)

// Engine 持有不可变的开关规则集，判定并发安全。
type Engine struct {
	version uint64
	flags   map[string]Flag
}

// Compile 校验并构建规则集（版本为 1）。规则非法、优先级冲突或开关重名时返回错误。
func Compile(flags []Flag) (*Engine, error) {
	return CompileWithVersion(flags, 1)
}

// CompileWithVersion 与 Compile 相同，但显式指定规则集版本，
// 版本参与分流哈希，因此同一目标在同一版本下结论稳定、跨版本可改变。
func CompileWithVersion(flags []Flag, version uint64) (*Engine, error) {
	e := &Engine{version: version, flags: map[string]Flag{}}
	for _, f := range flags {
		if f.Name == "" {
			return nil, &Error{Reason: ReasonInvalidRule, Detail: "empty flag name"}
		}
		if _, exists := e.flags[f.Name]; exists {
			return nil, &Error{Reason: ReasonInvalidRule, Flag: f.Name, Detail: "duplicate flag name"}
		}
		if err := validateFlag(f); err != nil {
			return nil, err
		}
		// 复制并按优先级从高到低排序，保证迭代顺序确定。
		rules := append([]Rule(nil), f.Rules...)
		sort.SliceStable(rules, func(i, j int) bool { return rules[i].Priority > rules[j].Priority })
		f.Rules = rules
		e.flags[f.Name] = f
	}
	return e, nil
}

func validateFlag(f Flag) error {
	seen := map[int]bool{}
	for i, r := range f.Rules {
		if r.Percentage < 0 || r.Percentage > 100 {
			return &Error{Reason: ReasonInvalidRule, Flag: f.Name,
				Detail: "rule percentage out of range [0,100]"}
		}
		if seen[r.Priority] {
			return &Error{Reason: ReasonPriorityConflict, Flag: f.Name,
				Detail: "duplicate rule priority"}
		}
		seen[r.Priority] = true
		_ = i
	}
	return nil
}

// Evaluate 判定 target 是否命中 flag。同一 target 在同一规则版本下结论稳定可复现。
// flag 不存在时返回 ReasonUnknownFlag 错误，绝不默认放行。
func (e *Engine) Evaluate(flag, target string) (Decision, error) {
	f, ok := e.flags[flag]
	if !ok {
		return Decision{}, &Error{Reason: ReasonUnknownFlag, Flag: flag, Detail: "flag not defined"}
	}
	d := Decision{Flag: flag, Target: target, Version: e.version, RuleIndex: -1}
	if !f.Enabled {
		d.Explain = "flag disabled"
		return d, nil
	}
	for i, r := range f.Rules {
		b := bucket(f.Name, target, r.Salt, e.version)
		if b < r.Percentage {
			d.Allowed = true
			d.RuleIndex = i
			d.Bucket = b
			d.Explain = "hit rule priority=" + itoa(r.Priority) +
				" percentage=" + itoa(r.Percentage) +
				" bucket=" + itoa(b)
			return d, nil
		}
	}
	// 未命中任何规则：用最高优先级规则的桶解释（无规则时桶为 -1）。
	if len(f.Rules) > 0 {
		r := f.Rules[0]
		d.Bucket = bucket(f.Name, target, r.Salt, e.version)
		d.Explain = "no rule matched; checked by descending priority"
	} else {
		d.Explain = "flag enabled but no rules defined"
	}
	return d, nil
}

// bucket 以 FNV-1a 64 位哈希把目标确定性地映射到 [0,100)。
func bucket(flag, target, salt string, version uint64) int {
	h := fnv.New64a()
	h.Write([]byte(flag))
	h.Write([]byte{0})
	h.Write([]byte(salt))
	h.Write([]byte{0})
	h.Write([]byte(target))
	h.Write([]byte{0})
	var vb [8]byte
	for i := 0; i < 8; i++ {
		vb[i] = byte(version >> (8 * i))
	}
	h.Write(vb[:])
	return int(h.Sum64() % 100)
}

// Version 返回当前规则集版本。
func (e *Engine) Version() uint64 { return e.version }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
