// Package tooling 实现工具调用抹平（U3）：按模型能力选择 full/partial/none 策略
// （R7/R8/R9），消费方只见标准 tool_calls 结构（R10）。
//
// none 与 partial 降级路径采用注入策略（KTD4）：注入 system 消息（工具列表 + 输出约束 +
// few-shot 示例）→ 调用后端 → 解析降级链提取 JSON（parse.go）→ 校验（name ∈ 工具集、
// arguments 按声明 schema 用 U4 校验）→ 包装为标准 tool_calls。tooling 只做策略决策与
// 结果包装，不解析后端错误消息文本（KTD9）。
package tooling

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/internal/anyutil"
	"github.com/qfy-agent/qfy-agent/registry"
	"github.com/qfy-agent/qfy-agent/schema"
)

// Tool 是消费方请求 tools 的标准 OpenAI 形态（工具定义输入，供注入模板与校验使用）。
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction 工具函数定义。
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"` // JSON Schema（供 U4 校验 arguments）
}

// ErrorKind 工具调用抹平错误类型码（供上层重试分类；KTD6 语义，不做消息文本匹配）。
type ErrorKind string

const (
	// KindParse 模型输出无法提取/解析为 JSON 对象（KTD4 降级链全部失败）。
	KindParse ErrorKind = "parse_failed"
	// KindValidation 提取到 JSON 但结构或内容校验失败（未知工具名 / arguments 结构非法）。
	KindValidation ErrorKind = "validation_failed"
)

// Error 工具调用抹平稳定错误：携带类型码与可读消息；Details 携带 arguments 的
// 结构化校验错误（U4），供上层重试链路按错误类型码分类（KTD6）。
type Error struct {
	Kind    ErrorKind
	Message string
	Details []schema.Error
}

func (e *Error) Error() string {
	if len(e.Details) > 0 {
		return fmt.Sprintf("tooling %s: %s（%s: %s）", e.Kind, e.Message, e.Details[0].Path, e.Details[0].Kind)
	}
	return fmt.Sprintf("tooling %s: %s", e.Kind, e.Message)
}

// Strategies 是工具调用抹平策略执行器（U3 入口）。
type Strategies struct {
	client *backend.Client
	cfg    InjectConfig
}

// Option 配置 Strategies。
type Option func(*Strategies)

// WithInjectConfig 覆盖注入策略的 prompt 模板与 few-shot 示例（KTD4 模板可配置）。
func WithInjectConfig(cfg InjectConfig) Option {
	return func(s *Strategies) { s.cfg = cfg }
}

// NewStrategies 构造策略执行器；未传 Option 时使用默认注入模板与工具推导示例。
func NewStrategies(client *backend.Client, opts ...Option) *Strategies {
	s := &Strategies{client: client}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Call 按模型能力选择工具调用策略执行一次后端调用（R7/R8/R9），返回标准 OpenAI 响应：
//
//   - full：tools 原样透传，响应 tool_calls 经后端归一化原样返回（R7）；
//   - partial：首轮原生透传，触发 KTD5 降级条件时本轮以注入策略重试一次（R8）；
//   - none：注入 system 消息（工具列表 + 输出约束 + few-shot）→ 后端调用 → 解析降级链
//     提取 JSON → 校验（name ∈ 工具集、arguments 按声明 schema 用 U4 校验）→
//     包装为标准 tool_calls（R9）。
//
// params 为外部 OpenAI 格式请求参数（含 messages、tools 等）；tools 为解析后的工具列表，
// 为空时尝试从 params["tools"] 解析，仍为空则视为普通对话直接透传（不应用注入策略）。
// 返回的响应中消费方永远看到标准 tool_calls 结构（R10）。
func (s *Strategies) Call(ctx context.Context, m *registry.Model, params map[string]any, tools []Tool) (*backend.ChatCompletion, error) {
	if len(tools) == 0 {
		parsed, err := ParseTools(params["tools"])
		if err != nil {
			return nil, err
		}
		tools = parsed
	}
	if len(tools) == 0 {
		// 无工具：工具调用策略无意义，原样直连后端（含 none 模型的普通对话）。
		return s.client.Call(ctx, m, params)
	}
	// tool_choice 语义（评审修正 F2）："none" 按无工具直连（不注入、不调用工具）；
	// 指定函数时裁剪工具列表为仅该函数；auto/required/缺省保持全量注入。
	tools, err := applyToolChoice(params["tool_choice"], tools)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		// tool_choice=none：剥离 tools/tool_choice 后直连（后端不接收工具描述）。
		p := make(map[string]any, len(params))
		for k, v := range params {
			if k == "tools" || k == "tool_choice" {
				continue
			}
			p[k] = v
		}
		return s.client.Call(ctx, m, p)
	}
	switch m.Capabilities.ToolCalling {
	case registry.ToolCallingFull:
		return s.client.Call(ctx, m, params)
	case registry.ToolCallingPartial:
		return s.callPartial(ctx, m, params, tools)
	case registry.ToolCallingNone:
		return s.callInjection(ctx, m, params, tools)
	default:
		return nil, fmt.Errorf("未知工具调用能力 %q", m.Capabilities.ToolCalling)
	}
}

// applyToolChoice 按 tool_choice 语义处理工具列表：
//
//   - 缺省或 "auto"/"required"：原样返回；
//   - "none"：返回空列表（调用方按无工具直连，工具调用不生效）；
//   - 对象形态 {"function":{"name": X}}：裁剪为仅 X；X 不在声明工具集内返回校验错误。
func applyToolChoice(raw any, tools []Tool) ([]Tool, error) {
	switch tc := raw.(type) {
	case nil:
		return tools, nil
	case string:
		switch tc {
		case "auto", "required":
			return tools, nil
		case "none":
			return nil, nil
		default:
			return nil, fmt.Errorf("tool_choice %q 非法（允许 auto|required|none|对象形态）", tc)
		}
	case map[string]any:
		fn, ok := tc["function"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool_choice 对象形态缺少 function 字段")
		}
		name, _ := fn["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tool_choice 对象形态缺少 function.name")
		}
		for _, t := range tools {
			if t.Function.Name == name {
				return []Tool{t}, nil
			}
		}
		return nil, fmt.Errorf("tool_choice 指定的工具 %q 不在请求声明的工具集中", name)
	default:
		return nil, fmt.Errorf("tool_choice 类型非法（%T）", raw)
	}
}

// callPartial 首轮原生透传（R8）；按 KTD5 触发降级条件时本轮以注入策略重试一次。
// 降级条件：后端调用失败（*backend.UnavailableError / *backend.UpstreamError，errors.As 识别）
// 或响应中 tool_calls 结构无法解析（缺 id/name/arguments 或 arguments 非 JSON）。
// 模型合理选择不调用工具（响应无 tool_calls）不算失败，不触发降级。
func (s *Strategies) callPartial(ctx context.Context, m *registry.Model, params map[string]any, tools []Tool) (*backend.ChatCompletion, error) {
	resp, err := s.client.Call(ctx, m, params)
	if err != nil {
		if isDegradable(err) {
			return s.callInjection(ctx, m, params, tools)
		}
		return nil, err
	}
	if firstInvalidToolCall(resp) >= 0 {
		return s.callInjection(ctx, m, params, tools)
	}
	return resp, nil
}

// isDegradable 判定后端错误是否可触发 partial 降级（KTD5）：UpstreamError
// （HTTP 非 2xx）可降级；UnavailableError 中的超时**不**降级（评审修正：超时
// 后端大概率继续超时，降级只会让单轮延迟翻倍、燃烧预算）；一般网络错误与
// MalformedError（响应畸形）原样上抛。
func isDegradable(err error) bool {
	var ue *backend.UnavailableError
	if errors.As(err, &ue) {
		return !ue.Timeout
	}
	var up *backend.UpstreamError
	return errors.As(err, &up)
}

// callInjection 注入策略（KTD4；none 主路径与 partial 降级共用）：
// 注入 system 消息 → 调用后端 → 后端偶发原生 tool_calls 时直接透传归一化（不二次包装），
// 否则解析降级链提取 JSON → 校验 → 包装为标准 tool_calls。
func (s *Strategies) callInjection(ctx context.Context, m *registry.Model, params map[string]any, tools []Tool) (*backend.ChatCompletion, error) {
	injected := s.buildInjectionParams(params, tools)
	resp, err := s.client.Call(ctx, m, injected)
	if err != nil {
		return nil, err
	}
	if hasToolCalls(resp) {
		return resp, nil
	}
	return s.wrapInjected(resp, tools)
}

// hasToolCalls 判断响应中是否存在原生 tool_calls 字段（兼容后端偶发输出，直接透传）。
func hasToolCalls(cc *backend.ChatCompletion) bool {
	for i := range cc.Choices {
		if len(cc.Choices[i].Message.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// firstInvalidToolCall 返回首个结构非法的 tool_call 下标（KTD5 降级条件）：
// 缺 id/name/arguments，或 arguments 非合法 JSON（含字符串值内容非 JSON）。无非法返回 -1。
func firstInvalidToolCall(cc *backend.ChatCompletion) int {
	for ci := range cc.Choices {
		for ti := range cc.Choices[ci].Message.ToolCalls {
			tc := cc.Choices[ci].Message.ToolCalls[ti]
			if tc.ID == "" || tc.Function.Name == "" || !validArguments(tc.Function.Arguments) {
				return ti
			}
		}
	}
	return -1
}

// validArguments 判定 arguments 值是否合法（KTD5）：原始值须为合法 JSON；
// 若为 JSON 字符串值（标准 OpenAI arguments 形态），其内容也须是合法 JSON。
func validArguments(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return json.Valid([]byte(s))
	}
	return true
}

// wrapInjected 解析注入输出并包装为标准 tool_calls（R10）：
// 解析降级链提取 JSON → 校验（name ∈ 工具集、arguments 按声明 schema 用 U4 校验）→
// 生成 id（call_ 前缀 + 随机 hex）、type:"function"、function.arguments 重序列化为 JSON 字符串。
//
// 模型选择不调用工具（输出普通文本，内容不含任何 "{"）时按普通 assistant 消息
// 返回（业界共识 B5 / 工具循环最终答案轮）：不视为解析失败，不进校验重试。
// 内容含 "{" 但无法解析为完整 JSON 对象 → KindParse（畸形输出，走 R15 重试）。
func (s *Strategies) wrapInjected(cc *backend.ChatCompletion, tools []Tool) (*backend.ChatCompletion, error) {
	if len(cc.Choices) == 0 {
		return nil, &Error{Kind: KindParse, Message: "响应中没有 choices，无法解析模型输出"}
	}
	content, err := messageText(cc.Choices[0].Message.Content)
	if err != nil {
		return nil, &Error{Kind: KindParse, Message: fmt.Sprintf("助手消息没有可解析的文本内容: %v", err)}
	}
	raw, err := ParseToolCallJSON(content)
	if err != nil {
		if !strings.Contains(content, "{") {
			// 普通文本：模型选择不调用工具，原样返回（B5）。
			return cc, nil
		}
		return nil, &Error{Kind: KindParse, Message: err.Error()}
	}
	var obj struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, &Error{Kind: KindValidation, Message: fmt.Sprintf("模型输出不是工具调用对象: %v", err)}
	}
	if err := validateToolCall(obj.Name, obj.Arguments, tools); err != nil {
		return nil, err
	}
	args, err := serializeArguments(obj.Arguments)
	if err != nil {
		return nil, &Error{Kind: KindValidation, Message: fmt.Sprintf("arguments 重序列化失败: %v", err)}
	}
	// 包装为标准 tool_calls；带 tool_calls 的消息 content 置空、finish_reason 为 tool_calls
	// （对齐 OpenAI 规范形态）。
	cc.Choices[0].Message.Content = json.RawMessage("null")
	cc.Choices[0].Message.ToolCalls = []backend.ToolCall{{
		ID:       genCallID(),
		Type:     "function",
		Function: backend.Function{Name: obj.Name, Arguments: args},
	}}
	cc.Choices[0].FinishReason = "tool_calls"
	return cc, nil
}

// messageText 提取助手消息的文本内容；content 为 null/缺失/非字符串（如多模态数组）时
// 返回错误（无文本可解析，注入约束要求纯 JSON 文本输出）。
func messageText(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

// serializeArguments 把 arguments 值重序列化为 JSON 字符串（标准 OpenAI tool_calls 形态，
// 消费方契约：function.arguments 是内容为合法 JSON 的字符串）：先紧凑化原始值，
// 再整体编码为 JSON 字符串字面量（arguments 用 json.RawMessage 延迟解析，KTD4）。
func serializeArguments(raw json.RawMessage) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	quoted, err := json.Marshal(buf.String())
	if err != nil {
		return nil, err
	}
	return quoted, nil
}

// validateToolCall 校验解析出的工具调用：name 非空且 ∈ 工具集；arguments 缺失报错；
// arguments 按工具声明的参数 schema 用 U4 校验，结构化校验错误进入 Details（供重试链路分类）。
func validateToolCall(name string, args json.RawMessage, tools []Tool) error {
	if name == "" {
		return &Error{Kind: KindValidation, Message: "工具调用缺少 name 字段"}
	}
	var fn *ToolFunction
	for i := range tools {
		if tools[i].Function.Name == name {
			fn = &tools[i].Function
			break
		}
	}
	if fn == nil {
		return &Error{Kind: KindValidation, Message: fmt.Sprintf("未知工具名 %q（可用工具: %s）", name, toolNames(tools))}
	}
	if len(args) == 0 {
		return &Error{Kind: KindValidation, Message: fmt.Sprintf("工具 %q 的 arguments 缺失", name)}
	}
	errs, err := schema.Validate(args, fn.Parameters)
	if err != nil {
		return &Error{Kind: KindValidation, Message: fmt.Sprintf("工具 %q 的 arguments 不是合法 JSON: %v", name, err)}
	}
	if len(errs) > 0 {
		return &Error{Kind: KindValidation, Message: fmt.Sprintf("工具 %q 的 arguments 校验失败（%d 处）", name, len(errs)), Details: errs}
	}
	return nil
}

// toolNames 拼接工具名列表（用于未知工具名错误信息）。
func toolNames(tools []Tool) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	return strings.Join(names, ", ")
}

// genCallID 生成标准工具调用 id：call_ 前缀 + 随机 hex（R10 注入包装）。
func genCallID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	return "call_" + hex.EncodeToString(b[:])
}

// ParseTools 把外部 OpenAI 格式的 tools 数组（[]any，通常由 JSON 解码得到）解析为 []Tool。
// 元素非对象或缺 function.name 时报错；tools 缺失/为空返回空切片。
func ParseTools(raw any) ([]Tool, error) {
	arr, ok := anyutil.AsSlice(raw)
	if !ok {
		return nil, fmt.Errorf("tools 应为数组，实际为 %T", raw)
	}
	out := make([]Tool, 0, len(arr))
	for i, item := range arr {
		var t Tool
		b, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("tools[%d] 序列化失败: %w", i, err)
		}
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("tools[%d] 结构非法: %w", i, err)
		}
		if t.Function.Name == "" {
			return nil, fmt.Errorf("tools[%d] 缺少 function.name", i)
		}
		out = append(out, t)
	}
	return out, nil
}

