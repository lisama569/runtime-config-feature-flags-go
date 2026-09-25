package config

import "fmt"

// FieldType 声明字段期望的类型。
type FieldType int

const (
	TypeString FieldType = iota
	TypeInt
	TypeFloat
	TypeBool
)

func (t FieldType) String() string {
	switch t {
	case TypeString:
		return "string"
	case TypeInt:
		return "int"
	case TypeFloat:
		return "float"
	case TypeBool:
		return "bool"
	default:
		return "unknown"
	}
}

// Field 描述一个配置字段的约束。
type Field struct {
	Name      string
	Type      FieldType
	Required  bool
	Sensitive bool // 敏感字段不出现在日志与错误信息中
	Validate  func(v any) error
}

// Schema 是一组字段约束。
type Schema struct {
	Fields []Field
}

// typeMatches 检查值是否符合期望类型。
// JSON 数字一律解析为 float64，因此 TypeInt 要求 float64 且为整数。
func typeMatches(t FieldType, v any) bool {
	switch t {
	case TypeString:
		_, ok := v.(string)
		return ok
	case TypeBool:
		_, ok := v.(bool)
		return ok
	case TypeFloat:
		_, ok := v.(float64)
		return ok
	case TypeInt:
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	default:
		return false
	}
}

// Validate 校验合并后的配置，返回第一个拒绝原因。
// 任何类型不匹配、必填缺失或取值非法都会返回 *RejectError，绝不静默回退默认值。
func (s Schema) Validate(values map[string]any) error {
	for _, f := range s.Fields {
		v, ok := values[f.Name]
		if !ok {
			if f.Required {
				return &RejectError{Kind: KindMissingRequired, Field: f.Name,
					Msg: "required field is absent"}
			}
			continue
		}
		if !typeMatches(f.Type, v) {
			return &RejectError{Kind: KindTypeMismatch, Field: f.Name,
				Msg: fmt.Sprintf("want %s, got %s (value=%s)",
					f.Type, typeNameOf(v), redact(f.Sensitive, v))}
		}
		if f.Validate != nil {
			if err := f.Validate(v); err != nil {
				return &RejectError{Kind: KindInvalidValue, Field: f.Name,
					Msg: fmt.Sprintf("value=%s rejected: %v", redact(f.Sensitive, v), err)}
			}
		}
	}
	return nil
}

func typeNameOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case float64:
		return "float"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}
