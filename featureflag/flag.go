// Package featureflag 提供基于规则的功能开关灰度判定。
package featureflag

import "fmt"

// Reason 区分开关判定失败的原因类别。
type Reason string

const (
	ReasonUnknownFlag      Reason = "unknown_flag"      // 引用了不存在的开关
	ReasonInvalidRule      Reason = "invalid_rule"      // 规则本身非法
	ReasonPriorityConflict Reason = "priority_conflict" // 同一开关下规则优先级冲突
)

// Error 描述开关相关错误。
type Error struct {
	Reason Reason
	Flag   string
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("featureflag error (%s): flag=%q: %s", e.Reason, e.Flag, e.Detail)
}

// Rule 是一条灰度规则：将目标哈希到 [0,100) 的桶，桶值小于 Percentage 即命中。
// 同一开关内各规则的 Priority 必须唯一，数值越大优先级越高，
// 判定时按优先级从高到低逐条检查，第一条命中的规则决定结论。
type Rule struct {
	Priority   int
	Percentage int    // [0,100]，灰度百分比
	Salt       string // 参与哈希，调整 Salt 可重新分流
}

// Flag 是一个功能开关。
type Flag struct {
	Name    string
	Enabled bool
	Rules   []Rule
}

// Decision 是一次判定的结论与依据。
type Decision struct {
	Flag      string
	Target    string
	Allowed   bool
	Version   uint64 // 判定所基于的规则集版本
	RuleIndex int    // 命中的规则下标，-1 表示未命中任何规则
	Bucket    int    // 目标在命中规则下的桶值 [0,100)
	Explain   string // 人类可读的判定依据
}
