package config

import "fmt"

// Reason 区分配置被拒绝的原因类别。
type Reason string

const (
	ReasonSourceIO        Reason = "source_io"        // 来源读取失败
	ReasonTypeMismatch    Reason = "type_mismatch"    // 值类型与 schema 声明不符
	ReasonMissingRequired Reason = "missing_required" // 必填项缺失
	ReasonInvalidValue    Reason = "invalid_value"    // 取值非法（如越界、枚举外）
)

// RejectError 描述一次配置被拒绝的原因。敏感字段的值不会出现在错误信息中。
type RejectError struct {
	Reason Reason
	Source string // 来源名称
	Key    string // 配置键
	Detail string // 不含敏感值的说明
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("config rejected (%s): source=%q key=%q: %s", e.Reason, e.Source, e.Key, e.Detail)
}
