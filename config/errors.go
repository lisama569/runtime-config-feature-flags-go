package config

import "fmt"

// RejectKind 区分配置被拒绝的原因类别。
type RejectKind int

const (
	// KindTypeMismatch 字段类型与 schema 声明不符。
	KindTypeMismatch RejectKind = iota
	// KindMissingRequired 必填字段缺失。
	KindMissingRequired
	// KindInvalidValue 字段取值非法（未通过自定义校验）。
	KindInvalidValue
)

func (k RejectKind) String() string {
	switch k {
	case KindTypeMismatch:
		return "type_mismatch"
	case KindMissingRequired:
		return "missing_required"
	case KindInvalidValue:
		return "invalid_value"
	default:
		return "unknown"
	}
}

// RejectError 表示一次配置校验拒绝，包含可区分的类别与字段名。
// 敏感字段的值不会出现在错误信息中。
type RejectError struct {
	Kind  RejectKind
	Field string
	Msg   string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("config rejected (%s) field=%q: %s", e.Kind, e.Field, e.Msg)
}

// redact 对敏感字段的值做脱敏，保证其不出现在日志与错误中。
func redact(sensitive bool, v any) string {
	if sensitive {
		return "<redacted>"
	}
	return fmt.Sprintf("%v", v)
}
