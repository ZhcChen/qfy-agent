package backend

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/qfy-agent/qfy-agent/registry"
)

// normTestModel 构造仅用于归一化测试的注册表模型（无网络）。
func normTestModel(t *testing.T) *registry.Model {
	t.Helper()
	yaml := `
models:
  - id: gemma-4-e4b
    backend: openai-compatible
    base_url: http://backend.local/v1
    api_key: sk-test
    model: google/gemma-4-e4b
    capabilities:
      tool_calling: none
      json_mode: true
      streaming: true
    default_params:
      temperature: 0.2
      max_tokens: 2048
`
	r, err := registry.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("加载测试注册表失败: %v", err)
	}
	m, err := r.Get("gemma-4-e4b")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	return m
}

func TestNormalizeRequestMergesDefaultsAndTranslatesModel(t *testing.T) {
	m := normTestModel(t)
	// 外部请求未传 temperature：应填入 default_params 值（R5 参数抹平）。
	out := NormalizeRequest(m, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if out["temperature"] != 0.2 {
		t.Errorf("未显式传 temperature 时应填入默认值 0.2，得到 %v", out["temperature"])
	}
	if out["max_tokens"] != 2048 {
		t.Errorf("未显式传 max_tokens 时应填入默认值 2048，得到 %v", out["max_tokens"])
	}
	// model 翻译：注册表 ID ≠ 后端 model id 时，归一化结果为后端 id（评审修正 F4）。
	if out["model"] != "google/gemma-4-e4b" {
		t.Errorf("model 应翻译为后端 id google/gemma-4-e4b，得到 %v", out["model"])
	}
	// 只读语义：不修改输入（KTD9/F5）。
	if m.DefaultParams["temperature"] != 0.2 {
		t.Error("NormalizeRequest 不得修改模型默认参数")
	}
}

func TestNormalizeRequestExplicitOverridesDefault(t *testing.T) {
	m := normTestModel(t)
	out := NormalizeRequest(m, map[string]any{"temperature": 0.8})
	if out["temperature"] != 0.8 {
		t.Errorf("显式 temperature 应覆盖默认值，得到 %v", out["temperature"])
	}
}

func TestNormalizeRequestDualTokensAndPassthrough(t *testing.T) {
	m := normTestModel(t)
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	explicit := map[string]any{
		"model":                 "gemma-4-e4b",
		"max_tokens":            100,
		"max_completion_tokens": 50,
		"tools":                 tools,
		"tool_choice":           "auto",
		"response_format":       map[string]any{"type": "json_object"},
		"stream":                true,
	}
	out := NormalizeRequest(m, explicit)
	// KTD8：max_tokens 与 max_completion_tokens 双字段兼容透传。
	if out["max_tokens"] != 100 || out["max_completion_tokens"] != 50 {
		t.Errorf("双 token 字段应原样透传，得到 max_tokens=%v max_completion_tokens=%v", out["max_tokens"], out["max_completion_tokens"])
	}
	// tools/tool_choice/response_format/stream 原样透传（R2/R11）。
	if out["tool_choice"] != "auto" {
		t.Errorf("tool_choice 应原样透传，得到 %v", out["tool_choice"])
	}
	if out["stream"] != true {
		t.Errorf("stream 应原样透传，得到 %v", out["stream"])
	}
	if rf, ok := out["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("response_format 应原样透传，得到 %v", out["response_format"])
	}
	if tl, ok := out["tools"].([]any); !ok || len(tl) != 1 {
		t.Errorf("tools 应原样透传，得到 %v", out["tools"])
	}
	// 未知请求字段显式透传后端（KTD8 白名单语义）。
	explicit["custom_field"] = "keep-me"
	out = NormalizeRequest(m, explicit)
	if out["custom_field"] != "keep-me" {
		t.Errorf("未知请求字段应透传，得到 %v", out["custom_field"])
	}
}

func TestNormalizeResponseFillsMissingFields(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
	c, err := NormalizeResponse(body)
	if err != nil {
		t.Fatalf("NormalizeResponse 失败: %v", err)
	}
	if !strings.HasPrefix(c.ID, "chatcmpl-") {
		t.Errorf("缺失 id 应补齐为 chatcmpl- 前缀，得到 %q", c.ID)
	}
	if c.Created <= 0 {
		t.Errorf("缺失 created 应补齐为当前时间戳，得到 %d", c.Created)
	}
	if c.Object != "chat.completion" {
		t.Errorf("object 应为 chat.completion，得到 %q", c.Object)
	}
	if c.Usage == nil {
		t.Error("缺失 usage 应补齐为零值结构")
	}
	if c.Choices == nil || len(c.Choices) != 1 {
		t.Fatalf("choices 应保留，得到 %+v", c.Choices)
	}
	var content string
	if err := json.Unmarshal(c.Choices[0].Message.Content, &content); err != nil || content != "hi" {
		t.Errorf("content 应保真保留，得到 %s（err=%v）", c.Choices[0].Message.Content, err)
	}
}

func TestNormalizeResponseFinishReasonWhitelist(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"stop", "stop"},
		{"length", "length"},
		{"tool_calls", "tool_calls"},
		{"content_filter", "content_filter"},
		{"", "stop"},           // 缺失归为 stop
		{"max_tokens", "stop"}, // 非规范枚举归为 stop（KTD8）
		{"function_call", "stop"},
	}
	for _, tc := range cases {
		body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` + tc.in + `"}]}`)
		c, err := NormalizeResponse(body)
		if err != nil {
			t.Fatalf("NormalizeResponse(%q) 失败: %v", tc.in, err)
		}
		if got := c.Choices[0].FinishReason; got != tc.want {
			t.Errorf("finish_reason %q 应归一化为 %q，得到 %q", tc.in, tc.want, got)
		}
	}
}

func TestNormalizeResponseMalformed(t *testing.T) {
	cases := [][]byte{
		[]byte(`not-json`),
		[]byte(`{"choices": "oops"}`), // 结构非法：choices 应为数组
		[]byte(``),
	}
	for _, body := range cases {
		_, err := NormalizeResponse(body)
		var me *MalformedError
		if !errors.As(err, &me) {
			t.Errorf("畸形响应 %q 应返回 *MalformedError，得到 %v", body, err)
		}
	}
}

func TestNormalizeErrorBodyParsesOpenAIError(t *testing.T) {
	body := []byte(`{"error":{"message":"bad request","type":"invalid_request_error","param":"temperature","code":"param_range"}}`)
	eb := NormalizeErrorBody(400, body)
	if eb.Message != "bad request" || eb.Type != "invalid_request_error" || eb.Param != "temperature" || eb.Code != "param_range" {
		t.Errorf("应提取统一错误体字段，得到 %+v", eb)
	}
	// 统一错误体形状：{"error":{message,type,param,code}}（KTD8）。
	var shape struct {
		Error ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(eb.JSON(), &shape); err != nil {
		t.Fatalf("错误体编码不是合法 JSON: %v", err)
	}
	if shape.Error.Message != "bad request" || shape.Error.Code != "param_range" {
		t.Errorf("统一错误体形状错误，得到 %+v", shape.Error)
	}
}

func TestNormalizeErrorBodyFallbackToRawText(t *testing.T) {
	eb := NormalizeErrorBody(502, []byte(`<html>bad gateway at https://internal.example.com/proxy</html>`))
	if !strings.Contains(eb.Message, "bad gateway") {
		t.Errorf("非 JSON 错误体应以原始文本为 message，得到 %q", eb.Message)
	}
	if strings.Contains(eb.Message, "https://internal.example.com") {
		t.Errorf("message 应剥离内部 URL，得到 %q", eb.Message)
	}
}

func TestNormalizeErrorBodyEmptyBody(t *testing.T) {
	eb := NormalizeErrorBody(429, nil)
	if eb.Message != http.StatusText(http.StatusTooManyRequests) {
		t.Errorf("空错误体应以状态文本兜底，得到 %q", eb.Message)
	}
}

func TestSanitizeMessageStripsURLsAndPaths(t *testing.T) {
	msg := "failed: https://192.168.1.91:1234/v1/internal/x?y=1 and /var/log/qfy/backend.log and C:\\Users\\ops\\log.txt"
	got := SanitizeMessage(msg)
	for _, bad := range []string{"https://192.168.1.91", "/var/log/qfy", "C:\\Users"} {
		if strings.Contains(got, bad) {
			t.Errorf("message 应剥离疑似 URL/路径 %q，得到 %q", bad, got)
		}
	}
}

func TestSanitizeMessageKeepsShortText(t *testing.T) {
	if got := SanitizeMessage("model not found"); got != "model not found" {
		t.Errorf("短消息应原样保留，得到 %q", got)
	}
}

func TestSanitizeMessageTruncates(t *testing.T) {
	long := strings.Repeat("密", 700)
	got := SanitizeMessage(long)
	if n := len([]rune(got)); n > 500 {
		t.Errorf("message 应截断为不超过 500 字符，得到 %d", n)
	}
	if !strings.HasPrefix(got, strings.Repeat("密", 497)) || !strings.HasSuffix(got, "...") {
		t.Errorf("截断应保留前缀并加 ... 标记，得到 %q", got)
	}
}
