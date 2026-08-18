package backend

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/qfy-agent/qfy-agent/registry"
)

// NormalizeRequest 参数抹平 + model 翻译（R5）：
// 外部 OpenAI 格式请求参数与注册表 default_params 合并（显式优先，见 registry.Model.Merge），
// 并把 model 字段从注册表 ID 翻译为后端 model id（评审修正：ID 与后端 model id 可不同）。
// 返回新 map，不修改输入；tools/tool_choice/response_format/stream 及未知键原样透传（KTD8）。
func NormalizeRequest(m *registry.Model, explicit map[string]any) map[string]any {
	out := m.Merge(explicit)
	out["model"] = m.BackendModelID()
	return out
}

// BuildRequest 序列化归一化后的上游请求体。
func BuildRequest(m *registry.Model, explicit map[string]any) ([]byte, error) {
	return json.Marshal(NormalizeRequest(m, explicit))
}

// ChatCompletion 非流式补全响应（白名单语义：仅保留规范字段，未知响应字段显式丢弃，KTD8）。
type ChatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice 单条补全选择。
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Message 补全消息。Content 用 json.RawMessage 保真透传：
// 字符串内容、多模态数组等任意形态均不重编码（KTD8 白名单语义）。
type Message struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []ToolCall      `json:"tool_calls,omitempty"`

	reasoningContent json.RawMessage
}

// UnmarshalJSON 捕获 LM Studio 的 reasoning_content 扩展供内部完整性判定，
// 同时保持 Message 的对外 JSON 白名单不包含该字段。
func (m *Message) UnmarshalJSON(data []byte) error {
	type messageWire struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
		ReasoningContent json.RawMessage `json:"reasoning_content"`
	}
	var wire messageWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.Role = wire.Role
	m.Content = wire.Content
	m.ToolCalls = wire.ToolCalls
	m.reasoningContent = wire.ReasoningContent
	return nil
}

// ToolCall 标准工具调用结构（R10 消费方可见形态）。
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function 工具调用函数信息。
type Function struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Usage token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// NormalizeResponse 解析并归一化上游非流式补全响应：
// 补齐缺失字段（id、created、usage），object/finish_reason 白名单化（KTD8）；
// JSON 解析失败或结构非法（字段类型不匹配等）返回 *MalformedError（KTD9）。
func NormalizeResponse(body []byte) (*ChatCompletion, error) {
	var c ChatCompletion
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, &MalformedError{Phase: "decode response", Err: err}
	}
	if c.ID == "" {
		c.ID = genID()
	}
	if c.Object != "chat.completion" { // 缺失或非规范值统一为白名单常量
		c.Object = "chat.completion"
	}
	if c.Created == 0 {
		c.Created = time.Now().Unix()
	}
	if c.Usage == nil {
		c.Usage = &Usage{}
	}
	if c.Choices == nil {
		c.Choices = []Choice{}
	}
	responseHasVisibleOutput := false
	responseHasReasoningOnlyLength := false
	for i := range c.Choices {
		c.Choices[i].FinishReason = NormalizeFinishReason(c.Choices[i].FinishReason)
		for j := range c.Choices[i].Message.ToolCalls {
			if args, err := NormalizeArguments(c.Choices[i].Message.ToolCalls[j].Function.Arguments); err == nil {
				c.Choices[i].Message.ToolCalls[j].Function.Arguments = args
			}
		}
		choiceHasVisibleOutput := len(c.Choices[i].Message.ToolCalls) > 0 || hasVisibleContent(c.Choices[i].Message.Content)
		responseHasVisibleOutput = responseHasVisibleOutput || choiceHasVisibleOutput
		if c.Choices[i].FinishReason == "length" && !choiceHasVisibleOutput &&
			hasReasoningContent(c.Choices[i].Message.reasoningContent) {
			responseHasReasoningOnlyLength = true
		}
	}
	if responseHasReasoningOnlyLength && !responseHasVisibleOutput {
		return nil, &IncompleteResponseError{}
	}
	return &c, nil
}

func hasVisibleContent(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text != ""
	}
	return true
}

func hasReasoningContent(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text) != ""
	}
	return true
}

// NormalizeArguments 把 function.arguments 统一为 JSON 字符串形态（契约：消费方
// 看到的 arguments 是内容为合法 JSON 的字符串，R10）：字符串值原样保留（内容须为
// 合法 JSON）；对象/数组/标量形态紧凑化后整体编码为 JSON 字符串（评审修正：full/
// partial 透传不得把对象形态原样外泄）。无法归一化时返回错误。
func NormalizeArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("arguments 为空")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if !json.Valid([]byte(s)) {
			return nil, fmt.Errorf("arguments 字符串内容不是合法 JSON")
		}
		return raw, nil
	}
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

// finish_reason 白名单（KTD8）：仅输出 stop|length|tool_calls|content_filter。
// 未知值（含缺失）统一归为 stop——语义最接近"正常结束"，且不会被上层误判为
// length/tool_calls/content_filter 等触发重试或降级的特殊语义。
var validFinishReasons = map[string]bool{
	"stop":           true,
	"length":         true,
	"tool_calls":     true,
	"content_filter": true,
}

// NormalizeFinishReason 把 finish_reason 白名单化（KTD8）：仅输出
// stop|length|tool_calls|content_filter。function_call（废弃枚举）显式映射为
// tool_calls——语义等价且与同响应含 tool_calls 的形态一致（评审修正）；
// 其余未知值（含缺失）统一归为 stop——语义最接近"正常结束"，且不会被上层误判为
// length/tool_calls/content_filter 等触发重试或降级的特殊语义。
// 导出供 api 层流式透传复用（单一实现，避免跨包漂移）。
func NormalizeFinishReason(fr string) string {
	switch fr {
	case "function_call":
		return "tool_calls"
	}
	if validFinishReasons[fr] {
		return fr
	}
	return "stop"
}

// genID 生成本地补全 id（上游缺失时补齐，保证响应骨架合法）。
func genID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// ErrorBody 统一错误体内层结构（KTD8）：{"error":{message,type,param,code}}。
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    string `json:"code"`
}

// JSON 返回统一错误体 {"error":{message,type,param,code}} 的完整编码（KTD8）。
func (e *ErrorBody) JSON() []byte {
	b, _ := json.Marshal(struct {
		Error ErrorBody `json:"error"`
	}{Error: *e})
	return b
}

// NormalizeErrorBody 把任意上游非 2xx 响应体规范化为统一错误体：
// 可解析为 {"error":{...}} 时提取其字段，否则以原始文本作为 message；
// message 一律清洗（截断 + 剥离疑似 URL/内部路径，评审修正：不原样透传内部路径）。
func NormalizeErrorBody(statusCode int, body []byte) *ErrorBody {
	eb := &ErrorBody{}
	if len(body) > 0 {
		var parsed struct {
			Error ErrorBody `json:"error"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			eb.Message = parsed.Error.Message
			eb.Type = parsed.Error.Type
			eb.Param = parsed.Error.Param
			eb.Code = parsed.Error.Code
		} else {
			eb.Message = string(body)
		}
	}
	if strings.TrimSpace(eb.Message) == "" {
		eb.Message = http.StatusText(statusCode)
		if eb.Message == "" {
			eb.Message = fmt.Sprintf("HTTP %d", statusCode)
		}
	}
	eb.Message = SanitizeMessage(eb.Message)
	return eb
}

// 错误消息清洗上限：截断为 500 字符（rune）。
const maxErrorRunes = 500

var (
	// urlRe 剥离 http(s)/ftp URL。
	urlRe = regexp.MustCompile(`(?:https?|ftp)://[^\s"'<>\\]+`)
	// winPathRe 剥离 Windows 路径，如 C:\Users\...。
	winPathRe = regexp.MustCompile(`[A-Za-z]:\\[^\s"'<>]+`)
	// pathRe 剥离至少两段的类 Unix 路径，如 /var/log/qfy/x.log；
	// 单段 token（如 /v1）不剥离，避免误伤正常错误文本。
	pathRe = regexp.MustCompile(`/[\w.\-]+(?:/[\w.\-]+)+`)
	// secretRe 剥离常见凭据形态（评审修正 P0）：sk- 前缀 API key、Bearer token、
	// JWT 三段式、常见 key=value 凭据对——防止上游错误体把后端凭据泄漏给消费方。
	secretRe = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{8,}|bearer\s+[A-Za-z0-9._\-]+|eyJ[A-Za-z0-9._\-]{10,}\.eyJ[A-Za-z0-9._\-]{10,}\.[A-Za-z0-9._\-]{10,}|(?:api[_-]?key|token|secret|password)\s*[=:]\s*[^\s"'&,;]+)`)
	// authorizationRe 兜底：Authorization 头形态整体剥离。
	authorizationRe = regexp.MustCompile(`(?i)authorization\s*[:=]\s*[^\s"'&,;]+`)
)

// SanitizeMessage 清洗上游错误 message：剥离疑似 URL、内部路径与凭据形态后
// 截断为 500 字符（评审修正 P0：sk- 前缀/Bearer/JWT 等凭据不得外泄）。
func SanitizeMessage(msg string) string {
	msg = urlRe.ReplaceAllString(msg, "[redacted]")
	msg = winPathRe.ReplaceAllString(msg, "[redacted]")
	msg = pathRe.ReplaceAllString(msg, "[redacted]")
	msg = secretRe.ReplaceAllString(msg, "[redacted]")
	msg = authorizationRe.ReplaceAllString(msg, "[redacted]")
	r := []rune(msg)
	if len(r) > maxErrorRunes {
		msg = string(r[:maxErrorRunes-len([]rune("..."))]) + "..."
	}
	return strings.TrimSpace(msg)
}
