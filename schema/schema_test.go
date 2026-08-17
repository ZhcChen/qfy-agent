package schema

import (
	"strings"
	"testing"
)

func TestValidObject(t *testing.T) {
	doc := []byte(`{"name": "map_column", "arguments": {"column": "客户名"}}`)
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string"},
			"arguments": map[string]any{"type": "object"},
		},
		"required": []any{"name", "arguments"},
	}
	errs, err := Validate(doc, s)
	if err != nil {
		t.Fatalf("不应有解析错误: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("合法对象应通过，得到 %v", errs)
	}
}

func TestMissingRequired(t *testing.T) {
	doc := []byte(`{"name": "map_column"}`)
	s := map[string]any{
		"type":     "object",
		"required": []any{"name", "arguments"},
	}
	errs, err := Validate(doc, s)
	if err != nil {
		t.Fatalf("不应有解析错误: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("应有 1 条错误，得到 %d: %v", len(errs), errs)
	}
	if errs[0].Kind != KindMissing {
		t.Errorf("错误类型应为 missing_required，得到 %q", errs[0].Kind)
	}
	if errs[0].Path != "arguments" {
		t.Errorf("错误路径应为 arguments，得到 %q", errs[0].Path)
	}
	if !strings.Contains(errs[0].Message, "arguments") {
		t.Errorf("错误信息应含字段名，得到 %q", errs[0].Message)
	}
}

func TestWrongType(t *testing.T) {
	doc := []byte(`{"name": 42}`)
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}
	errs, err := Validate(doc, s)
	if err != nil {
		t.Fatalf("不应有解析错误: %v", err)
	}
	if len(errs) != 1 || errs[0].Kind != KindWrongType {
		t.Fatalf("应有 1 条类型错误，得到 %v", errs)
	}
	if errs[0].Path != "name" {
		t.Errorf("错误路径应为 name，得到 %q", errs[0].Path)
	}
}

func TestEnumMismatch(t *testing.T) {
	doc := []byte(`{"level": "high"}`)
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"level": map[string]any{"type": "string", "enum": []any{"low", "medium"}},
		},
	}
	errs, err := Validate(doc, s)
	if err != nil {
		t.Fatalf("不应有解析错误: %v", err)
	}
	if len(errs) != 1 || errs[0].Kind != KindEnum {
		t.Fatalf("应有 1 条 enum 错误，得到 %v", errs)
	}
}

func TestEnumMatch(t *testing.T) {
	doc := []byte(`{"level": "low"}`)
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"level": map[string]any{"type": "string", "enum": []any{"low", "medium"}},
		},
	}
	errs, err := Validate(doc, s)
	if err != nil {
		t.Fatalf("不应有解析错误: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("enum 内值应通过，得到 %v", errs)
	}
}

func TestNestedObject(t *testing.T) {
	doc := []byte(`{"args": {"inner": {"deep": "x"}}}`)
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"inner": map[string]any{
						"type":     "object",
						"required": []any{"deep"},
					},
				},
			},
		},
	}
	// 深层路径正确。
	errs, err := Validate(doc, s)
	if err != nil || len(errs) != 0 {
		t.Fatalf("深层合法应通过: %v %v", errs, err)
	}
	// 缺失深层必填。
	bad := []byte(`{"args": {"inner": {}}}`)
	errs, err = Validate(bad, s)
	if err != nil {
		t.Fatalf("不应有解析错误: %v", err)
	}
	if len(errs) != 1 || errs[0].Path != "args.inner.deep" {
		t.Fatalf("深层缺失应报路径 args.inner.deep，得到 %v", errs)
	}
}

func TestArrayItems(t *testing.T) {
	doc := []byte(`{"tags": ["a", "b"]}`)
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
	}
	errs, err := Validate(doc, s)
	if err != nil || len(errs) != 0 {
		t.Fatalf("合法数组应通过: %v %v", errs, err)
	}
	bad := []byte(`{"tags": ["a", 42]}`)
	errs, err = Validate(bad, s)
	if err != nil {
		t.Fatalf("不应有解析错误: %v", err)
	}
	if len(errs) != 1 || errs[0].Path != "tags[1]" {
		t.Fatalf("数组元素类型错误应报路径 tags[1]，得到 %v", errs)
	}
}

func TestInvalidJSON(t *testing.T) {
	_, err := Validate([]byte(`{"name": `), map[string]any{})
	if err == nil {
		t.Fatal("非法 JSON 应返回解析错误")
	}
	if !strings.Contains(err.Error(), string(KindInvalidJSON)) {
		t.Errorf("解析错误应含 invalid_json 类型码，得到 %v", err)
	}
}

func TestEmptySchema(t *testing.T) {
	// 空 schema：合法 JSON 即通过。
	errs, err := Validate([]byte(`{"anything": [1, "x", true]}`), map[string]any{})
	if err != nil || len(errs) != 0 {
		t.Fatalf("空 schema 下合法 JSON 应通过: %v %v", errs, err)
	}
	// nil schema 等同空 schema。
	errs, err = Validate([]byte(`42`), nil)
	if err != nil || len(errs) != 0 {
		t.Fatalf("nil schema 下合法 JSON 应通过: %v %v", errs, err)
	}
}

func TestIntegerType(t *testing.T) {
	s := map[string]any{"type": "integer"}
	if errs, _ := Validate([]byte(`5`), s); len(errs) != 0 {
		t.Errorf("整数 5 应匹配 integer，得到 %v", errs)
	}
	if errs, _ := Validate([]byte(`5.5`), s); len(errs) != 1 {
		t.Errorf("5.5 不应匹配 integer，得到 %v", errs)
	}
	if errs, _ := Validate([]byte(`"5"`), s); len(errs) != 1 {
		t.Errorf("字符串 \"5\" 不应匹配 integer，得到 %v", errs)
	}
}

func TestUnknownTypeIsLenient(t *testing.T) {
	// 未知类型声明不约束（宽松透传）。
	errs, err := Validate([]byte(`"whatever"`), map[string]any{"type": "bogus"})
	if err != nil || len(errs) != 0 {
		t.Fatalf("未知类型应宽松通过: %v %v", errs, err)
	}
}
