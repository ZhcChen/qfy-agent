// Package registry 定义模型注册表：YAML 声明的模型能力与默认参数。
//
// 注册表是适配层的决策依据：能力声明（工具调用、JSON 模式、流式）
// 决定后端调用策略，default_params 提供参数抹平所需的默认值。
package registry

import (
	"fmt"
)

// ToolCalling 声明模型的工具调用能力等级。
type ToolCalling string

const (
	// ToolCallingFull 原生工具调用完整可用，tools 原样透传。
	ToolCallingFull ToolCalling = "full"
	// ToolCallingPartial 原生工具调用部分可用：首轮透传，失败后降级为注入策略。
	ToolCallingPartial ToolCalling = "partial"
	// ToolCallingNone 无原生工具调用：tools 描述改写为 prompt 注入。
	ToolCallingNone ToolCalling = "none"
)

// Backend 后端协议类型。第一版仅支持 OpenAI 兼容端点。
type Backend string

const (
	// BackendOpenAICompatible 一切 OpenAI 兼容端点（LM Studio / Ollama / vLLM 等）。
	BackendOpenAICompatible Backend = "openai-compatible"
)

// Capabilities 声明模型能力，驱动适配层策略选择。
type Capabilities struct {
	ToolCalling ToolCalling `yaml:"tool_calling"`
	JSONMode    bool        `yaml:"json_mode"`
	Streaming   bool        `yaml:"streaming"`
}

// Model 是注册表中的一条模型声明。
type Model struct {
	// ID 是对外暴露的模型标识（/v1/models 与请求 model 字段使用的名字）。
	ID string `yaml:"id"`
	// Backend 后端协议类型，第一版仅 openai-compatible。
	Backend Backend `yaml:"backend"`
	// BaseURL 后端端点，如 http://192.168.1.91:1234/v1。
	BaseURL string `yaml:"base_url"`
	// APIKey 后端认证凭据；库不读取环境变量，由消费方配置注入。
	APIKey string `yaml:"api_key"`
	// Model 后端实际使用的模型 id，可与对外 ID 不同。
	Model string `yaml:"model"`
	// Capabilities 能力声明。
	Capabilities Capabilities `yaml:"capabilities"`
	// DefaultParams 参数抹平默认值（temperature、max_tokens 等任意键），
	// 外部请求未显式指定时合并进后端请求。
	DefaultParams map[string]any `yaml:"default_params"`
}

// validate 校验声明合法性与能力枚举值。
func (m *Model) validate() error {
	if m.ID == "" {
		return fmt.Errorf("模型声明缺少必填字段 id")
	}
	if m.Backend == "" {
		return fmt.Errorf("模型 %s 缺少必填字段 backend", m.ID)
	}
	if m.Backend != BackendOpenAICompatible {
		return fmt.Errorf("模型 %s 的 backend %q 不受支持（第一版仅支持 openai-compatible）", m.ID, m.Backend)
	}
	if m.BaseURL == "" {
		return fmt.Errorf("模型 %s 缺少必填字段 base_url", m.ID)
	}
	if m.Model == "" {
		return fmt.Errorf("模型 %s 缺少必填字段 model", m.ID)
	}
	switch m.Capabilities.ToolCalling {
	case ToolCallingFull, ToolCallingPartial, ToolCallingNone:
	default:
		return fmt.Errorf("模型 %s 的能力 tool_calling %q 非法（允许 full|partial|none）", m.ID, m.Capabilities.ToolCalling)
	}
	return nil
}

// Merge 返回外部请求显式参数与 default_params 合并后的参数表。
// 外部显式设置的键优先；default_params 中未配置的键不被合并。
// 返回新 map，不修改任何输入（只读拷贝语义，KTD9/F5）。
func (m *Model) Merge(explicit map[string]any) map[string]any {
	merged := make(map[string]any, len(m.DefaultParams)+len(explicit))
	for k, v := range m.DefaultParams {
		merged[k] = v
	}
	for k, v := range explicit {
		merged[k] = v
	}
	return merged
}

// ModelID 返回对外模型标识。
func (m *Model) ModelID() string { return m.ID }

// BackendModelID 返回后端实际模型 id。
func (m *Model) BackendModelID() string { return m.Model }
