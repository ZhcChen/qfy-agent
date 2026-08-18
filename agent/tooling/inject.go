package tooling

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qfy-agent/qfy-agent/agent/internal/anyutil"
)

// Example 一个 few-shot 示例对（KTD4）：用户请求与模型应输出的工具调用 JSON。
type Example struct {
	// User 示例用户请求。
	User string
	// ToolCall 示例模型输出：形如 {"name": "...", "arguments": {...}}。
	ToolCall string
}

// InjectConfig 注入策略的 prompt 模板配置（KTD4 模板可配置）。
// 零值使用默认模板与由工具列表推导的 few-shot 示例。
type InjectConfig struct {
	// SystemTemplate 自定义系统提示模板；占位符 {tools} 与 {examples} 会被替换为
	// 工具列表文本与 few-shot 示例文本。空值使用默认模板。
	SystemTemplate string
	// Examples 自定义 few-shot 示例；非空时覆盖默认示例（默认由工具列表推导，至多 2 条）。
	Examples []Example
}

// 默认系统提示模板（KTD4）：工具列表（JSON Schema 文本）+ 输出约束
// （只输出单个 JSON 对象，不要 markdown/解释/其他内容）+ few-shot 示例。
const defaultSystemTemplate = `你是工具调用代理。根据用户请求，从可用工具中选择合适的工具并输出调用。

可用工具列表（JSON Schema 文本）：
{tools}

输出约束：
- 当需要调用工具时，只输出单个 JSON 对象，格式为 {"name": "<工具名>", "arguments": {<参数>}}。
- 不要使用 markdown 代码围栏，不要解释，不要其他任何内容。
- arguments 必须是所选工具参数 schema 要求的 JSON 对象。

示例：
{examples}`

// BuildSystemMessage 构造注入的 system 消息内容（KTD4）：工具列表（JSON Schema 精简形式）+
// 输出约束 + few-shot 示例。cfg 零值时使用默认模板与由工具列表推导的示例。
// 工具按输入顺序渲染（高频工具在前由消费方负责排序）。
func BuildSystemMessage(tools []Tool, cfg InjectConfig) string {
	tmpl := cfg.SystemTemplate
	if strings.TrimSpace(tmpl) == "" {
		tmpl = defaultSystemTemplate
	}
	exs := cfg.Examples
	if len(exs) == 0 {
		exs = defaultExamples(tools)
	}
	out := strings.ReplaceAll(tmpl, "{tools}", renderTools(tools))
	out = strings.ReplaceAll(out, "{examples}", renderExamples(exs))
	return out
}

// renderTools 渲染工具列表文本：每个工具输出工具名、描述（如有）与参数 schema 的精简 JSON。
func renderTools(tools []Tool) string {
	var b strings.Builder
	for i, t := range tools {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(renderTool(t))
	}
	return b.String()
}

func renderTool(t Tool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "工具名: %s\n", t.Function.Name)
	if t.Function.Description != "" {
		fmt.Fprintf(&b, "描述: %s\n", t.Function.Description)
	}
	if t.Function.Parameters != nil {
		params, err := json.Marshal(t.Function.Parameters)
		if err != nil {
			params = []byte("{}")
		}
		fmt.Fprintf(&b, "参数 schema: %s", params)
	} else {
		b.WriteString("参数: 无")
	}
	return b.String()
}

// renderExamples 渲染 few-shot 示例文本：用户请求 → 模型输出（工具调用 JSON）。
func renderExamples(exs []Example) string {
	var b strings.Builder
	for i, e := range exs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "用户: %s\n模型输出: %s", e.User, e.ToolCall)
	}
	return b.String()
}

// defaultExamples 由工具列表推导 1-2 条 few-shot 示例（KTD4）：取前 1-2 个工具的真实名字与
// 声明参数名，保证示例与工具集一致（避免模型模仿出未知工具名）。
func defaultExamples(tools []Tool) []Example {
	n := len(tools)
	if n > 2 {
		n = 2
	}
	exs := make([]Example, 0, n)
	for i := 0; i < n; i++ {
		fn := tools[i].Function
		argsJSON, _ := json.Marshal(sampleArgs(fn.Parameters))
		exs = append(exs, Example{
			User:     fmt.Sprintf("请调用工具 %s 完成相应处理", fn.Name),
			ToolCall: fmt.Sprintf(`{"name": %q, "arguments": %s}`, fn.Name, argsJSON),
		})
	}
	return exs
}

// sampleArgs 从参数 schema 的 properties 中取前 2 个字段名生成示例参数值
// （仅用于 few-shot 格式演示，不做 schema 语义保证）。
func sampleArgs(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{}
	}
	props, _ := params["properties"].(map[string]any)
	if len(props) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, 2)
	for name, ps := range props {
		if len(out) >= 2 {
			break
		}
		psMap, _ := ps.(map[string]any)
		out[name] = sampleValue(psMap)
	}
	return out
}

func sampleValue(ps map[string]any) any {
	switch ps["type"] {
	case "number", "integer":
		return 1
	case "boolean":
		return true
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	case "string":
		// enum 约束优先：示例值必须落在允许列表内，否则示例本身违反所演示 schema
		// （评审修正 F4）。
		if enum, ok := ps["enum"].([]any); ok && len(enum) > 0 {
			return enum[0]
		}
		return "示例值"
	default: // 未知类型
		return "示例值"
	}
}

// InjectMessages 注入 system 消息（KTD4）：若消费方首条消息已是 system，
// 把注入内容与消费方 system 合并为单条 system 消息（注入内容前置），
// 避免双 system 冲突（评审修正 F3）；否则注入消息前置。返回新列表，不修改输入。
func InjectMessages(systemContent string, messages []any) []any {
	injected := map[string]any{"role": "system", "content": systemContent}
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok && first["role"] == "system" {
			merged := make([]any, 0, len(messages))
			combined := map[string]any{
				"role":    "system",
				"content": systemContent + "\n\n" + stringifyContent(first["content"]),
			}
			merged = append(merged, combined)
			merged = append(merged, messages[1:]...)
			return merged
		}
	}
	out := make([]any, 0, len(messages)+1)
	out = append(out, injected)
	out = append(out, messages...)
	return out
}

// stringifyContent 把 system 消息的 content 归一为字符串（字符串或数组形态均转文本）。
func stringifyContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case nil:
		return ""
	default:
		b, err := json.Marshal(c)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// buildInjectionParams 构造注入策略的上游请求参数（R9：tools 描述改写为 prompt 注入）：
// 移除 tools/tool_choice 字段，注入 system 消息前置到消费方 messages 之前。
// 返回新 map，不修改输入。
func (s *Strategies) buildInjectionParams(params map[string]any, tools []Tool) map[string]any {
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		if k == "tools" || k == "tool_choice" {
			continue
		}
		out[k] = v
	}
	msgs, _ := anyutil.AsSlice(params["messages"])
	out["messages"] = InjectMessages(BuildSystemMessage(tools, s.cfg), msgs)
	return out
}
