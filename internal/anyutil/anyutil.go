// Package anyutil 提供跨包共享的通用工具（评审修正：消除 reflect 切片归一化
// 助手的逐字重复）。
package anyutil

import "reflect"

// AsSlice 把切片（任意元素类型，如 []any / []map[string]any）统一转换为 []any；
// nil 返回 (nil, true)；非切片返回 (nil, false)。
func AsSlice(v any) ([]any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case []any:
		return t, true
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice {
			return nil, false
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	}
}
