package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// FieldType 声明配置字段的类型。
type FieldType string

const (
	TypeString FieldType = "string"
	TypeInt    FieldType = "int"
	TypeFloat  FieldType = "float"
	TypeBool   FieldType = "bool"
)

// Field 描述单个配置键的约束。
type Field struct {
	Type      FieldType
	Required  bool
	Sensitive bool // 敏感字段：值不出现在日志与错误信息中
	// Validate 对解码后的值做额外校验，返回非空字符串表示非法原因。
	// 注意：敏感字段的非法原因中不得包含值本身。
	Validate func(v any) string
}

// Schema 是一组字段约束的集合。
type Schema map[string]Field

// Snapshot 是一份已生效的、不可变的配置快照。
type Snapshot struct {
	Version uint64
	values  map[string]any
	origins map[string]string // 每个键最终生效的来源名
}

// Get 读取配置键，返回 (值, 是否存在)。
func (s *Snapshot) Get(key string) (any, bool) {
	v, ok := s.values[key]
	return v, ok
}

// Origin 返回该键最终生效的来源名；键不存在时返回空串与 false。
func (s *Snapshot) Origin(key string) (string, bool) {
	o, ok := s.origins[key]
	return o, ok
}

// MustString 读取字符串键，不存在或类型不符时 panic（仅用于已校验配置）。
func (s *Snapshot) MustString(key string) string { return s.values[key].(string) }

// MustInt 读取整型键。
func (s *Snapshot) MustInt(key string) int64 { return s.values[key].(int64) }

// MustFloat 读取浮点键。
func (s *Snapshot) MustFloat(key string) float64 { return s.values[key].(float64) }

// MustBool 读取布尔键。
func (s *Snapshot) MustBool(key string) bool { return s.values[key].(bool) }

// Keys 返回快照中所有键（排序后，用于调试/测试）。
func (s *Snapshot) Keys() []string {
	keys := make([]string, 0, len(s.values))
	for k := range s.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// String 输出快照内容，敏感字段以 "<redacted>" 代替。
func (s *Snapshot) String(schema Schema) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "snapshot v%d {", s.Version)
	for i, k := range s.Keys() {
		if i > 0 {
			buf.WriteString(", ")
		}
		if f, ok := schema[k]; ok && f.Sensitive {
			fmt.Fprintf(&buf, "%s=<redacted>", k)
		} else {
			fmt.Fprintf(&buf, "%s=%v", k, s.values[k])
		}
	}
	buf.WriteString("}")
	return buf.String()
}

// decodeValue 把单个原始 JSON 值按字段类型解码并校验。
func decodeValue(source, key string, f Field, raw []byte) (any, error) {
	reject := func(reason Reason, detail string) error {
		return &RejectError{Reason: reason, Source: source, Key: key, Detail: detail}
	}
	var v any
	switch f.Type {
	case TypeString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, reject(ReasonTypeMismatch, "expected string")
		}
		v = s
	case TypeInt:
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, reject(ReasonTypeMismatch, "expected int")
		}
		i, err := n.Int64()
		if err != nil {
			return nil, reject(ReasonTypeMismatch, "expected int, got non-integer number")
		}
		v = i
	case TypeFloat:
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, reject(ReasonTypeMismatch, "expected float")
		}
		f64, err := n.Float64()
		if err != nil {
			return nil, reject(ReasonTypeMismatch, "expected float")
		}
		v = f64
	case TypeBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, reject(ReasonTypeMismatch, "expected bool")
		}
		v = b
	default:
		return nil, reject(ReasonInvalidValue, "unknown field type "+string(f.Type))
	}
	if f.Validate != nil {
		if msg := f.Validate(v); msg != "" {
			return nil, reject(ReasonInvalidValue, msg)
		}
	}
	return v, nil
}
