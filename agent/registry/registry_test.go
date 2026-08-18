package registry

import (
	"strings"
	"testing"
)

const validYAML = `
models:
  - id: gemma-4-e4b
    backend: openai-compatible
    base_url: http://192.168.1.91:1234/v1
    api_key: ""
    model: google/gemma-4-e4b
    capabilities:
      tool_calling: none
      json_mode: true
      streaming: true
    default_params:
      temperature: 0.2
      max_tokens: 2048
`

func TestLoadValid(t *testing.T) {
	r, err := Load([]byte(validYAML))
	if err != nil {
		t.Fatalf("Load 应成功，得到错误: %v", err)
	}
	m, err := r.Get("gemma-4-e4b")
	if err != nil {
		t.Fatalf("Get 应成功: %v", err)
	}
	if m.Capabilities.ToolCalling != ToolCallingNone {
		t.Errorf("tool_calling 应为 none，得到 %q", m.Capabilities.ToolCalling)
	}
	if !m.Capabilities.JSONMode || !m.Capabilities.Streaming {
		t.Errorf("json_mode/streaming 应为 true，得到 %+v", m.Capabilities)
	}
	if m.BackendModelID() != "google/gemma-4-e4b" {
		t.Errorf("后端 model id 应为 google/gemma-4-e4b，得到 %q", m.BackendModelID())
	}
	if got := m.DefaultParams["temperature"]; got != 0.2 {
		t.Errorf("temperature 默认值应为 0.2，得到 %v", got)
	}
}

func TestLoadInvalidToolCalling(t *testing.T) {
	data := strings.Replace(validYAML, "tool_calling: none", "tool_calling: sometimes", 1)
	_, err := Load([]byte(data))
	if err == nil {
		t.Fatal("非法 tool_calling 枚举应报错")
	}
	if !strings.Contains(err.Error(), "tool_calling") {
		t.Errorf("错误信息应指明 tool_calling，得到: %v", err)
	}
}

func TestLoadMissingBaseURL(t *testing.T) {
	data := strings.Replace(validYAML, "base_url: http://192.168.1.91:1234/v1", "base_url: \"\"", 1)
	_, err := Load([]byte(data))
	if err == nil {
		t.Fatal("缺失 base_url 应报错")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("错误信息应指明 base_url，得到: %v", err)
	}
}

func TestLoadMissingModel(t *testing.T) {
	data := strings.Replace(validYAML, "model: google/gemma-4-e4b", "model: \"\"", 1)
	_, err := Load([]byte(data))
	if err == nil {
		t.Fatal("缺失 model 应报错")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("错误信息应指明 model，得到: %v", err)
	}
}

func TestLoadUnsupportedBackend(t *testing.T) {
	data := strings.Replace(validYAML, "backend: openai-compatible", "backend: anthropic", 1)
	_, err := Load([]byte(data))
	if err == nil {
		t.Fatal("不支持的 backend 应报错")
	}
}

func TestLoadEmpty(t *testing.T) {
	_, err := Load([]byte("models: []"))
	if err == nil {
		t.Fatal("空注册表应报错")
	}
}

func TestLoadDuplicateID(t *testing.T) {
	data := validYAML + `
  - id: gemma-4-e4b
    backend: openai-compatible
    base_url: http://example.com/v1
    model: other-model
    capabilities:
      tool_calling: full
`
	_, err := Load([]byte(data))
	if err == nil {
		t.Fatal("重复模型 id 应报错")
	}
}

func TestGetUnknown(t *testing.T) {
	r, err := Load([]byte(validYAML))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	_, err = r.Get("no-such-model")
	if err == nil {
		t.Fatal("Get 不存在的模型应返回错误")
	}
	if !strings.Contains(err.Error(), "no-such-model") {
		t.Errorf("错误信息应包含模型名，得到: %v", err)
	}
}

func TestListOrder(t *testing.T) {
	data := validYAML + `
  - id: gemma-4-12b
    backend: openai-compatible
    base_url: http://192.168.1.91:1234/v1
    model: google/gemma-4-12b
    capabilities:
      tool_calling: partial
      json_mode: true
      streaming: true
`
	r, err := Load([]byte(data))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("应有 2 个模型，得到 %d", len(list))
	}
	if list[0].ID != "gemma-4-e4b" || list[1].ID != "gemma-4-12b" {
		t.Errorf("List 应按声明顺序返回: %v, %v", list[0].ID, list[1].ID)
	}
}

func TestMergeDefaults(t *testing.T) {
	r, err := Load([]byte(validYAML))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	m, _ := r.Get("gemma-4-e4b")
	merged := m.Merge(map[string]any{"temperature": 0.8})
	if merged["temperature"] != 0.8 {
		t.Errorf("外部显式 temperature 应覆盖默认值，得到 %v", merged["temperature"])
	}
	if merged["max_tokens"] != 2048 {
		t.Errorf("未显式设置的 max_tokens 应取默认值，得到 %v", merged["max_tokens"])
	}
	// 只读拷贝语义：合并不得修改默认参数表（F5 并发安全）。
	if m.DefaultParams["temperature"] != 0.2 {
		t.Errorf("Merge 不得修改模型默认参数，得到 %v", m.DefaultParams["temperature"])
	}
	if _, changed := merged["extra"]; changed {
		t.Error("未在默认参数中的键不应出现在合并结果")
	}
}
