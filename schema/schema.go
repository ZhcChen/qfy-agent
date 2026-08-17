// Package schema 提供内置轻量 JSON schema 校验器（KTD6）：
// 支持 type/properties/required/enum/items 子集，输出结构化错误（字段路径 + 错误类型码）。
// 不引入第三方校验库；重试判定基于错误类型码，不做错误消息字符串匹配。
package schema

import (
	"encoding/json"
	"fmt"
)

// ErrorKind 校验错误类型码（KTD6：重试与决策基于类型码而非消息文本）。
type ErrorKind string

const (
	// KindInvalidJSON 输入不是合法 JSON 文档。
	KindInvalidJSON ErrorKind = "invalid_json"
	// KindWrongType 字段类型与 schema 声明不符。
	KindWrongType ErrorKind = "wrong_type"
	// KindMissing 必填字段缺失。
	KindMissing ErrorKind = "missing_required"
	// KindEnum 字段值不在 enum 允许列表内。
	KindEnum ErrorKind = "enum_mismatch"
)

// Error 单条校验错误。Path 为字段路径（如 "arguments.column_name"），
// 空字符串表示根文档。
type Error struct {
	Path    string    `json:"path"`
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`
}

func (e Error) Error() string { return e.Message }

// Validate 校验 JSON 文档是否匹配 schema 子集。
// 返回 (校验错误列表, 解析错误)：文档非法 JSON 时解析错误非 nil 且列表为空。
// 空 schema（{}）只校验合法 JSON 与根类型；nil schema 等同于空 schema。
func Validate(doc []byte, s map[string]any) ([]Error, error) {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", KindInvalidJSON, err)
	}
	if s == nil {
		s = map[string]any{}
	}
	var errs []Error
	validateValue("", v, s, &errs)
	return errs, nil
}

// validateValue 递归校验单个值。
func validateValue(path string, v any, s map[string]any, errs *[]Error) {
	// type 缺失时不做类型约束（宽松透传）。
	if t, ok := s["type"].(string); ok && t != "" {
		if !typeMatches(t, v) {
			*errs = append(*errs, Error{
				Path:    path,
				Kind:    KindWrongType,
				Message: fmt.Sprintf("字段 %q 应为 %s，实际为 %s", displayPath(path), t, actualType(v)),
			})
			return // 类型不符时不再深入结构（避免级联噪音）。
		}
	}
	// enum 校验（值相等性，JSON 数字统一为 float64）。
	if enum, ok := s["enum"].([]any); ok && len(enum) > 0 {
		if !enumContains(enum, v) {
			*errs = append(*errs, Error{
				Path:    path,
				Kind:    KindEnum,
				Message: fmt.Sprintf("字段 %q 的值不在允许列表内", displayPath(path)),
			})
		}
	}
	switch t, _ := s["type"].(string); t {
	case "object":
		validateObject(path, v, s, errs)
	case "array":
		validateArray(path, v, s, errs)
	}
}

// validateObject 校验对象：必填字段与嵌套 properties。
func validateObject(path string, v any, s map[string]any, errs *[]Error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return // 类型不符已在 validateValue 报错。
	}
	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			name, ok := r.(string)
			if !ok {
				continue
			}
			if _, present := obj[name]; !present {
				*errs = append(*errs, Error{
					Path:    joinPath(path, name),
					Kind:    KindMissing,
					Message: fmt.Sprintf("字段 %q 为必填", displayPath(joinPath(path, name))),
				})
			}
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for name, propSchema := range props {
			child, present := obj[name]
			if !present {
				continue // 非必填字段缺失不报错。
			}
			sub, ok := propSchema.(map[string]any)
			if !ok {
				continue
			}
			validateValue(joinPath(path, name), child, sub, errs)
		}
	}
}

// validateArray 校验数组：items 子项 schema 递归。
func validateArray(path string, v any, s map[string]any, errs *[]Error) {
	arr, ok := v.([]any)
	if !ok {
		return
	}
	items, ok := s["items"].(map[string]any)
	if !ok {
		return
	}
	for i, item := range arr {
		validateValue(fmt.Sprintf("%s[%d]", path, i), item, items, errs)
	}
}

// typeMatches 判断值是否匹配 schema 类型。
func typeMatches(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	default:
		return true // 未知类型声明不约束（宽松）。
	}
}

// actualType 返回值的实际 JSON 类型名（用于错误信息）。
func actualType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

// enumContains 判断值是否在 enum 列表中（JSON 数字统一为 float64 比较）。
func enumContains(enum []any, v any) bool {
	for _, e := range enum {
		if e == v {
			return true
		}
	}
	return false
}

// joinPath 拼接字段路径（根路径不加前缀）。
func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// displayPath 错误信息中的字段显示名（根路径显示为 "root"）。
func displayPath(path string) string {
	if path == "" {
		return "root"
	}
	return path
}
