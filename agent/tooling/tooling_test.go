package tooling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/registry"
	"github.com/qfy-agent/qfy-agent/schema"
)

// ---- 测试基础设施 ----

// testModel 构造指向 httptest 后端的注册表模型（能力按参数指定）。
func testModel(t *testing.T, baseURL string, tc registry.ToolCalling) *registry.Model {
	t.Helper()
	yaml := fmt.Sprintf(`
models:
  - id: gemma-4-e4b
    backend: openai-compatible
    base_url: %s
    api_key: sk-test
    model: google/gemma-4-e4b
    capabilities:
      tool_calling: %s
      json_mode: true
      streaming: true
    default_params:
      temperature: 0.2
`, baseURL, tc)
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

// testTools 供注入与校验使用的工具列表（map_column 声明必填 column/standard_field）。
var testTools = []Tool{
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "map_column",
			Description: "将 Excel 列映射到标准字段",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"column":         map[string]any{"type": "string"},
					"standard_field": map[string]any{"type": "string"},
				},
				"required": []any{"column", "standard_field"},
			},
		},
	},
}

// baseParams 外部 OpenAI 格式请求参数（含 tools 与 tool_choice）。
func baseParams() map[string]any {
	return map[string]any{
		"model":       "gemma-4-e4b",
		"tool_choice": "auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "请把列\"客户名称\"映射到标准字段"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "map_column",
					"description": "将 Excel 列映射到标准字段",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"column":         map[string]any{"type": "string"},
							"standard_field": map[string]any{"type": "string"},
						},
						"required": []any{"column", "standard_field"},
					},
				},
			},
		},
	}
}

// cannedResponse 一次预设的后端响应。
type cannedResponse struct {
	code int
	body string
}

// sequenceBackend 记录全部请求体并按顺序返回预设响应（用尽后重复最后一个）。
type sequenceBackend struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs []map[string]any
}

func newSequenceBackend(t *testing.T, resps []cannedResponse) *sequenceBackend {
	t.Helper()
	rb := &sequenceBackend{}
	rb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Errorf("请求体不是合法 JSON: %v（body=%s）", err, b)
		}
		rb.mu.Lock()
		idx := len(rb.reqs)
		rb.reqs = append(rb.reqs, parsed)
		rb.mu.Unlock()
		if idx >= len(resps) {
			idx = len(resps) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resps[idx].code)
		io.WriteString(w, resps[idx].body)
	}))
	t.Cleanup(rb.srv.Close)
	return rb
}

func (rb *sequenceBackend) count() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return len(rb.reqs)
}

func (rb *sequenceBackend) req(i int) map[string]any {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if i < 0 || i >= len(rb.reqs) {
		return nil
	}
	return rb.reqs[i]
}

// completionBody 构造 content 为指定文本的普通补全响应。
func completionBody(content string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content)
}

// toolCallsBody 构造含原生 tool_calls 的补全响应；argumentsJSON 须为 JSON 字符串字面量（含引号）。
func toolCallsBody(argumentsJSON string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_native_1","type":"function","function":{"name":"map_column","arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, argumentsJSON)
}

// assertWrappedToolCall 断言响应为注入策略包装的标准 tool_calls（R10）。
func assertWrappedToolCall(t *testing.T, resp *backend.ChatCompletion) {
	t.Helper()
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("应包装为 1 条标准 tool_calls，得到 choices=%d tool_calls=%d",
			len(resp.Choices), len(resp.Choices[0].Message.ToolCalls))
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if !strings.HasPrefix(tc.ID, "call_") {
		t.Errorf("id 应为 call_ 前缀，得到 %q", tc.ID)
	}
	if tc.Type != "function" {
		t.Errorf("type 应为 function，得到 %q", tc.Type)
	}
	if tc.Function.Name != "map_column" {
		t.Errorf("function.name 应为 map_column，得到 %q", tc.Function.Name)
	}
	if !json.Valid(tc.Function.Arguments) {
		t.Fatalf("function.arguments 应为合法 JSON，得到 %s", tc.Function.Arguments)
	}
	var argStr string
	if err := json.Unmarshal(tc.Function.Arguments, &argStr); err != nil {
		t.Fatalf("function.arguments 应为 JSON 字符串: %v", err)
	}
	var argObj map[string]any
	if err := json.Unmarshal([]byte(argStr), &argObj); err != nil {
		t.Fatalf("function.arguments 内容应为合法 JSON 对象: %v", err)
	}
	if argObj["column"] != "客户名" {
		t.Errorf("arguments 内容不符，得到 %v", argObj)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("包装后 finish_reason 应为 tool_calls，得到 %q", resp.Choices[0].FinishReason)
	}
	if string(resp.Choices[0].Message.Content) != "null" {
		t.Errorf("包装后 content 应为 null（标准 tool_calls 形态），得到 %s", resp.Choices[0].Message.Content)
	}
}

// assertToolingError 断言错误为 *tooling.Error 且类型码匹配。
func assertToolingError(t *testing.T, err error, kind ErrorKind) *Error {
	t.Helper()
	var te *Error
	if !errors.As(err, &te) {
		t.Fatalf("应为 *tooling.Error，得到 %v", err)
	}
	if te.Kind != kind {
		t.Errorf("错误类型码应为 %s，得到 %s", kind, te.Kind)
	}
	return te
}

// ---- full 策略 ----

// TestCallFullPassthrough：full 策略请求含 tools 时原样透传，响应 tool_calls 原样返回（R7/R10）。
func TestCallFullPassthrough(t *testing.T) {
	args := fmt.Sprintf("%q", `{"column":"客户名"}`)
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: toolCallsBody(args)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	s := NewStrategies(backend.NewClient())

	resp, err := s.Call(context.Background(), m, baseParams(), testTools)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if rb.count() != 1 {
		t.Errorf("full 策略应只调用一次后端，得到 %d", rb.count())
	}
	// tools/tool_choice 原样透传后端。
	req := rb.req(0)
	if tl, ok := req["tools"].([]any); !ok || len(tl) != 1 {
		t.Errorf("tools 应原样透传后端，得到 %v", req["tools"])
	}
	if tc, ok := req["tool_choice"].(string); !ok || tc != "auto" {
		t.Errorf("tool_choice 应原样透传后端，得到 %v", req["tool_choice"])
	}
	// tool_calls 原样返回（保留后端 id/type/arguments 字符串形态）。
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls 应原样返回，得到 %+v", resp.Choices)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_native_1" || tc.Type != "function" || tc.Function.Name != "map_column" {
		t.Errorf("tool_calls 应原样返回，得到 %+v", tc)
	}
	var argStr string
	if err := json.Unmarshal(tc.Function.Arguments, &argStr); err != nil {
		t.Fatalf("arguments 应为 JSON 字符串: %v", err)
	}
	var argObj map[string]any
	if err := json.Unmarshal([]byte(argStr), &argObj); err != nil {
		t.Fatalf("arguments 内容应为合法 JSON: %v", err)
	}
	if argObj["column"] != "客户名" {
		t.Errorf("arguments 内容不符，得到 %v", argObj)
	}
}

// ---- none 策略：注入 ----

// TestCallNoneInjectsSystemMessage：none 策略注入 system 消息前置到消费方消息之前，
// 含工具列表（name/描述/参数 schema）与 JSON 输出约束；tools/tool_choice 不送后端（R9）。
func TestCallNoneInjectsSystemMessage(t *testing.T) {
	content := `{"name": "map_column", "arguments": {"column": "客户名", "standard_field": "customer_name"}}`
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(content)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())

	if _, err := s.Call(context.Background(), m, baseParams(), testTools); err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	req := rb.req(0)
	// tools/tool_choice 移除（改写为 prompt 注入）。
	if _, ok := req["tools"]; ok {
		t.Error("tools 不应透传后端（应改写为 prompt 注入）")
	}
	if _, ok := req["tool_choice"]; ok {
		t.Error("tool_choice 不应透传后端")
	}
	// 注入 system 消息前置到消费方消息之前。
	messages, ok := req["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages 应为 [注入 system, 消费方 user]，得到 %v", req["messages"])
	}
	first, ok := messages[0].(map[string]any)
	if !ok || first["role"] != "system" {
		t.Fatalf("messages[0] 应为注入的 system 消息，得到 %v", messages[0])
	}
	sys, _ := first["content"].(string)
	for _, want := range []string{
		"map_column",              // 工具名
		"将 Excel 列映射到标准字段", // 工具描述
		"参数 schema",             // 参数 schema 文本
		"只输出单个 JSON 对象",     // JSON 输出约束
		"不要使用 markdown",       // 约束
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("注入 system 消息应包含 %q，得到：\n%s", want, sys)
		}
	}
	second, ok := messages[1].(map[string]any)
	if !ok || second["role"] != "user" {
		t.Errorf("messages[1] 应为消费方原始 user 消息，得到 %v", messages[1])
	}
}

// ---- none 策略：包装 ----

// TestCallNoneWrapsToolCall：模型输出 {"name","arguments"} → 包装为标准 tool_calls
// （id/type/function.name/function.arguments 为合法 JSON 字符串，R10）。
func TestCallNoneWrapsToolCall(t *testing.T) {
	content := `{"name": "map_column", "arguments": {"column": "客户名", "standard_field": "customer_name"}}`
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(content)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())

	resp, err := s.Call(context.Background(), m, baseParams(), testTools)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	assertWrappedToolCall(t, resp)
}

// TestCallNoneParsesFencedJSON：模型输出带 ```json 围栏 → 解析降级链第二级成功。
func TestCallNoneParsesFencedJSON(t *testing.T) {
	content := "```json\n{\"name\": \"map_column\", \"arguments\": {\"column\": \"客户名\", \"standard_field\": \"customer_name\"}}\n```"
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(content)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())

	resp, err := s.Call(context.Background(), m, baseParams(), testTools)
	if err != nil {
		t.Fatalf("围栏包裹的输出应解析成功: %v", err)
	}
	assertWrappedToolCall(t, resp)
}

// TestCallNoneParsesProse：模型输出带前后散文 → 括号配对提取（降级链第三级）成功。
func TestCallNoneParsesProse(t *testing.T) {
	content := "根据分析，我建议调用工具：{\"name\": \"map_column\", \"arguments\": {\"column\": \"客户名\", \"standard_field\": \"customer_name\"}}，请确认。"
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(content)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())

	resp, err := s.Call(context.Background(), m, baseParams(), testTools)
	if err != nil {
		t.Fatalf("带散文的输出应解析成功: %v", err)
	}
	assertWrappedToolCall(t, resp)
}

// ---- none 策略：错误 ----

// TestCallNoneUnparseableError：模型输出非法 JSON 且无法提取 → 明确错误（KindParse）。
func TestCallNoneUnparseableError(t *testing.T) {
	cases := []string{
		"抱歉，我无法完成该任务。",           // 纯散文，无 JSON
		`{"name": "map_column", "arguments": {broken`, // 非法 JSON 且括号无法配对
		"", // 空内容
	}
	for _, content := range cases {
		rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(content)}})
		m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
		s := NewStrategies(backend.NewClient())

		_, err := s.Call(context.Background(), m, baseParams(), testTools)
		if err == nil {
			t.Errorf("输出 %q 应返回明确错误", content)
			continue
		}
		assertToolingError(t, err, KindParse)
	}
}

// TestCallNoneUnknownToolNameError：模型输出未知工具名 → 校验失败错误（KindValidation）。
func TestCallNoneUnknownToolNameError(t *testing.T) {
	content := `{"name": "no_such_tool", "arguments": {"a": 1}}`
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(content)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())

	_, err := s.Call(context.Background(), m, baseParams(), testTools)
	te := assertToolingError(t, err, KindValidation)
	if !strings.Contains(err.Error(), "no_such_tool") {
		t.Errorf("错误信息应包含未知工具名，得到 %q", err.Error())
	}
	_ = te
}

// TestCallNoneArgumentsSchemaError：arguments 缺必填字段或类型不符（按工具声明的 schema）
// → 结构化校验错误（Details 携带 U4 错误）。
func TestCallNoneArgumentsSchemaError(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		wantKind   schema.ErrorKind
		wantPath   string
	}{
		{
			name:     "缺必填字段 standard_field",
			content:  `{"name": "map_column", "arguments": {"column": "客户名"}}`,
			wantKind: schema.KindMissing,
			wantPath: "standard_field",
		},
		{
			name:     "column 类型不符（number 而非 string）",
			content:  `{"name": "map_column", "arguments": {"column": 123, "standard_field": "customer_name"}}`,
			wantKind: schema.KindWrongType,
			wantPath: "column",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(tc.content)}})
			m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
			s := NewStrategies(backend.NewClient())

			_, err := s.Call(context.Background(), m, baseParams(), testTools)
			te := assertToolingError(t, err, KindValidation)
			if len(te.Details) == 0 {
				t.Fatalf("结构化校验错误应携带 Details，得到 %+v", te)
			}
			found := false
			for _, d := range te.Details {
				if d.Kind == tc.wantKind && d.Path == tc.wantPath {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Details 应包含 %s/%s，得到 %+v", tc.wantKind, tc.wantPath, te.Details)
			}
		})
	}
}

// ---- partial 策略 ----

// TestCallPartialFirstRoundOK：首轮成功（含合法 tool_calls）→ 无降级，响应原样返回（R8）。
func TestCallPartialFirstRoundOK(t *testing.T) {
	args := fmt.Sprintf("%q", `{"column":"客户名"}`)
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: toolCallsBody(args)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingPartial)
	s := NewStrategies(backend.NewClient())

	resp, err := s.Call(context.Background(), m, baseParams(), testTools)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if rb.count() != 1 {
		t.Errorf("首轮成功不应降级重试，调用次数=%d", rb.count())
	}
	// 首轮为原生透传：请求含 tools，无注入 system 消息。
	req := rb.req(0)
	if tl, ok := req["tools"].([]any); !ok || len(tl) != 1 {
		t.Errorf("首轮应原生透传 tools，得到 %v", req["tools"])
	}
	if tc := resp.Choices[0].Message.ToolCalls[0]; tc.ID != "call_native_1" {
		t.Errorf("首轮成功应原样返回 tool_calls，得到 %+v", tc)
	}
}

// TestCallPartialDegradesOnUpstreamError：首轮后端 500（UpstreamError）→ 降级注入重试（KTD5）。
func TestCallPartialDegradesOnUpstreamError(t *testing.T) {
	content := `{"name": "map_column", "arguments": {"column": "客户名", "standard_field": "customer_name"}}`
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 500, body: `{"error":{"message":"boom","type":"server_error"}}`},
		{code: 200, body: completionBody(content)},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingPartial)
	s := NewStrategies(backend.NewClient())

	resp, err := s.Call(context.Background(), m, baseParams(), testTools)
	if err != nil {
		t.Fatalf("降级重试应成功: %v", err)
	}
	if rb.count() != 2 {
		t.Fatalf("应降级重试一次（共 2 次调用），得到 %d", rb.count())
	}
	// 首轮：原生透传（含 tools/tool_choice）。
	if _, ok := rb.req(0)["tools"]; !ok {
		t.Error("首轮应原生透传 tools")
	}
	// 降级轮：注入 system 消息、移除 tools/tool_choice。
	req2 := rb.req(1)
	if _, ok := req2["tools"]; ok {
		t.Error("降级轮不应透传 tools")
	}
	messages, ok := req2["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("降级轮 messages 应含注入 system 消息，得到 %v", req2["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("降级轮 messages[0] 应为注入的 system 消息，得到 %v", messages[0])
	}
	// 注入轮结果包装为标准 tool_calls。
	assertWrappedToolCall(t, resp)
}

// TestCallPartialDegradesOnInvalidToolCalls：首轮 tool_calls 结构非法 → 降级注入重试（KTD5）。
func TestCallPartialDegradesOnInvalidToolCalls(t *testing.T) {
	content := `{"name": "map_column", "arguments": {"column": "客户名", "standard_field": "customer_name"}}`
	invalidBodies := []string{
		toolCallsBody(fmt.Sprintf("%q", "this is not json")), // arguments 字符串内容非 JSON
		`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"x","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"map_column","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`, // 缺 id
		`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"x","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`, // 缺 name
	}
	for i, bad := range invalidBodies {
		t.Run(fmt.Sprintf("invalid_%d", i), func(t *testing.T) {
			rb := newSequenceBackend(t, []cannedResponse{
				{code: 200, body: bad},
				{code: 200, body: completionBody(content)},
			})
			m := testModel(t, rb.srv.URL, registry.ToolCallingPartial)
			s := NewStrategies(backend.NewClient())

			resp, err := s.Call(context.Background(), m, baseParams(), testTools)
			if err != nil {
				t.Fatalf("降级重试应成功: %v", err)
			}
			if rb.count() != 2 {
				t.Fatalf("tool_calls 结构非法应降级重试（共 2 次调用），得到 %d", rb.count())
			}
			assertWrappedToolCall(t, resp)
		})
	}
}

// TestCallPartialPersistentFailure：降级后仍失败 → 错误上抛（原样返回后端错误）。
func TestCallPartialPersistentFailure(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 500, body: `{"error":{"message":"boom","type":"server_error"}}`},
		{code: 500, body: `{"error":{"message":"boom again","type":"server_error"}}`},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingPartial)
	s := NewStrategies(backend.NewClient())

	_, err := s.Call(context.Background(), m, baseParams(), testTools)
	if err == nil {
		t.Fatal("降级后仍失败应返回错误")
	}
	if rb.count() != 2 {
		t.Errorf("应调用 2 次（原生 + 降级），得到 %d", rb.count())
	}
	var ue *backend.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("应上抛后端错误（*backend.UpstreamError），得到 %v", err)
	}
	if ue.StatusCode != 500 {
		t.Errorf("上游错误状态码应为 500，得到 %d", ue.StatusCode)
	}
}

// ---- 原生 tool_calls 兼容 ----

// TestCallNoneNativeToolCallsPassthrough：none 策略下后端偶发输出原生 tool_calls →
// 直接透传归一化，不二次包装。
func TestCallNoneNativeToolCallsPassthrough(t *testing.T) {
	args := fmt.Sprintf("%q", `{"column":"客户名"}`)
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: toolCallsBody(args)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())

	resp, err := s.Call(context.Background(), m, baseParams(), testTools)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if rb.count() != 1 {
		t.Errorf("原生 tool_calls 应直接透传（仅 1 次调用），得到 %d", rb.count())
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_native_1" {
		t.Errorf("原生 tool_calls 应直接透传（保留后端 id），得到 %q", tc.ID)
	}
	if tc.Function.Name != "map_column" || tc.Type != "function" {
		t.Errorf("原生 tool_calls 应原样透传，得到 %+v", tc)
	}
}

// ---- 解析降级链（纯函数） ----

// TestParseToolCallJSON 覆盖解析降级链：直接解析、剥围栏、括号配对提取、失败路径。
func TestParseToolCallJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"直接 JSON 对象", `{"name":"x","arguments":{"a":1}}`, `{"name":"x","arguments":{"a":1}}`, false},
		{"直接 JSON 带空白", "  { \"name\" : \"x\" }  ", `{ "name" : "x" }`, false},
		{"json 围栏", "```json\n{\"name\":\"x\",\"arguments\":{}}\n```", `{"name":"x","arguments":{}}`, false},
		{"无语言标注围栏", "```\n{\"name\":\"x\"}\n```", `{"name":"x"}`, false},
		{"围栏 + 前后散文", "结果是：\n```json\n{\"name\":\"x\"}\n```\n请查收", `{"name":"x"}`, false},
		{"前后散文 + 括号配对", "根据分析，建议调用：{\"name\": \"x\", \"arguments\": {\"a\": 1}}，请确认。", `{"name": "x", "arguments": {"a": 1}}`, false},
		{"嵌套对象取外层", `{"name":"x","arguments":{"deep":{"b":2}}}`, `{"name":"x","arguments":{"deep":{"b":2}}}`, false},
		{"字符串内花括号不干扰", `{"name":"x","arguments":{"a":"} {"}}`, `{"name":"x","arguments":{"a":"} {"}}`, false},
		{"整体为数组（合法 JSON）", `[1,2,3]`, `[1,2,3]`, false},
		{"空输入", "", "", true},
		{"纯散文无 JSON", "抱歉，我无法完成该任务。", "", true},
		{"括号无法配对", `{"name": "x", "arguments": {broken`, "", true},
		{"散文花括号非 JSON", "我认为 {这个} 不是 JSON", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseToolCallJSON(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("应返回错误，得到 %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseToolCallJSON(%q) 失败: %v", tc.in, err)
			}
			var a, b any
			if err := json.Unmarshal(got, &a); err != nil {
				t.Fatalf("提取结果不是合法 JSON: %v（%s）", err, got)
			}
			if err := json.Unmarshal([]byte(tc.want), &b); err != nil {
				t.Fatalf("期望值不是合法 JSON: %v", err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("提取结果不符：得到 %s，期望 %s", got, tc.want)
			}
		})
	}
}

// ---- 工具解析 / 模板 / 无工具直连 ----

// TestParseTools 覆盖外部 OpenAI 形态 tools 数组 → []Tool。
func TestParseTools(t *testing.T) {
	raw := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "a",
			"description": "d",
			"parameters":  map[string]any{"type": "object"},
		},
	}}
	tools, err := ParseTools(raw)
	if err != nil {
		t.Fatalf("ParseTools 失败: %v", err)
	}
	if len(tools) != 1 || tools[0].Function.Name != "a" || tools[0].Type != "function" {
		t.Errorf("解析结果不符，得到 %+v", tools)
	}
	if tools[0].Function.Parameters == nil {
		t.Error("parameters 应解析保留")
	}
	if _, err := ParseTools([]any{map[string]any{"type": "function", "function": map[string]any{}}}); err == nil {
		t.Error("缺 function.name 应报错")
	}
	if _, err := ParseTools("oops"); err == nil {
		t.Error("非数组应报错")
	}
	tools, err = ParseTools(nil)
	if err != nil || len(tools) != 0 {
		t.Errorf("nil 应返回空切片，得到 %v, %v", tools, err)
	}
}

// TestBuildSystemMessage 覆盖默认模板渲染与自定义模板覆盖。
func TestBuildSystemMessage(t *testing.T) {
	msg := BuildSystemMessage(testTools, InjectConfig{})
	for _, want := range []string{
		"map_column",
		"将 Excel 列映射到标准字段",
		"参数 schema",
		"只输出单个 JSON 对象",
		"不要使用 markdown",
		"用户:",
		"模型输出:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("默认 system 消息应包含 %q，得到：\n%s", want, msg)
		}
	}
	// 自定义模板：占位符替换。
	msg = BuildSystemMessage(testTools, InjectConfig{SystemTemplate: "自定义模板\n{tools}\n===示例===\n{examples}"})
	for _, want := range []string{"自定义模板", "map_column", "示例", "模型输出:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("自定义模板渲染应包含 %q，得到：\n%s", want, msg)
		}
	}
	// 自定义 few-shot 覆盖默认示例。
	msg = BuildSystemMessage(testTools, InjectConfig{
		Examples: []Example{{User: "U1", ToolCall: `{"name":"x","arguments":{}}`}},
	})
	if strings.Contains(msg, "请调用工具") || !strings.Contains(msg, "U1") {
		t.Errorf("自定义示例应覆盖默认示例，得到：\n%s", msg)
	}
}

// TestCallNoToolsPlainPassthrough：无工具时（含 none 模型）不注入、不包装，直连后端。
func TestCallNoToolsPlainPassthrough(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody("你好，有什么可以帮你")}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())

	params := map[string]any{
		"model": "gemma-4-e4b",
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
	}
	resp, err := s.Call(context.Background(), m, params, nil)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if rb.count() != 1 {
		t.Errorf("无工具应只调用一次后端，得到 %d", rb.count())
	}
	messages, _ := rb.req(0)["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("无工具不应注入 system 消息，得到 %v", rb.req(0)["messages"])
	}
	if first, _ := messages[0].(map[string]any); first["role"] == "system" {
		t.Error("无工具时不应注入 system 消息")
	}
	var content string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &content); err != nil || content != "你好，有什么可以帮你" {
		t.Errorf("响应应原样透传，得到 %s（err=%v）", resp.Choices[0].Message.Content, err)
	}
}

// TestCallDerivesToolsFromParams：tools 参数为空时从 params["tools"] 推导（none 策略仍注入）。
func TestCallDerivesToolsFromParams(t *testing.T) {
	content := `{"name": "map_column", "arguments": {"column": "客户名", "standard_field": "customer_name"}}`
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(content)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())

	resp, err := s.Call(context.Background(), m, baseParams(), nil)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	messages, _ := rb.req(0)["messages"].([]any)
	if first, _ := messages[0].(map[string]any); first["role"] != "system" {
		t.Error("tools 从 params 推导后应注入 system 消息")
	}
	assertWrappedToolCall(t, resp)
}

// TestCallPartialNoDegradeOnTimeout：首轮超时（UnavailableError{Timeout:true}）→
// 不降级（评审修正：超时后端大概率继续超时，降级只会延迟翻倍、燃烧预算）。
func TestCallPartialNoDegradeOnTimeout(t *testing.T) {
	// 后端 handler 阻塞至 ctx 取消（制造客户端超时）；带兜底退出避免 Close 挂起。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	m := testModel(t, srv.URL, registry.ToolCallingPartial)
	client := backend.NewClient(backend.WithTimeouts(200*time.Millisecond, 5*time.Second))
	s := NewStrategies(client)

	_, err := s.Call(context.Background(), m, baseParams(), testTools)
	var ue *backend.UnavailableError
	if !errors.As(err, &ue) || !ue.Timeout {
		t.Fatalf("应返回超时类错误，得到 %v", err)
	}
}

// TestCallPartialNoDegradeOnMalformed：首轮响应畸形（MalformedError）→ 不降级原样上抛
// （KTD5：MalformedError 不属于降级条件）。
func TestCallPartialNoDegradeOnMalformed(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: "not json at all"}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingPartial)
	s := NewStrategies(backend.NewClient())

	_, err := s.Call(context.Background(), m, baseParams(), testTools)
	var me *backend.MalformedError
	if !errors.As(err, &me) {
		t.Fatalf("应返回 MalformedError，得到 %v", err)
	}
	if rb.count() != 1 {
		t.Errorf("畸形响应不应触发降级重试，调用次数=%d", rb.count())
	}
}

// TestCallToolChoiceNone：tool_choice="none" → 无工具直连（不注入、不调用工具）。
func TestCallToolChoiceNone(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody("我不需要工具")}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())
	params := baseParams()
	params["tool_choice"] = "none"

	resp, err := s.Call(context.Background(), m, params, testTools)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("tool_choice=none 不应产生工具调用，得到 %+v", resp.Choices[0].Message.ToolCalls)
	}
	req := rb.req(0)
	if _, hasTools := req["tools"]; hasTools {
		t.Errorf("tool_choice=none 不应透传 tools，得到 %v", req["tools"])
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("tool_choice=none 不应注入 system 消息，消息数=%d", len(msgs))
	}
}

// TestCallToolChoiceForced：对象形态强制指定工具 → 裁剪工具列表为仅该函数。
func TestCallToolChoiceForced(t *testing.T) {
	content := `{"name": "map_column", "arguments": {"column": "客户名", "standard_field": "customer_name"}}`
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody(content)}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())
	params := baseParams()
	params["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "map_column"}}

	resp, err := s.Call(context.Background(), m, params, testTools)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("强制指定工具应产生工具调用，得到 %+v", resp.Choices[0].Message.ToolCalls)
	}
	// 注入 system 消息只含该工具（唯一工具）。
	req := rb.req(0)
	msgs, _ := req["messages"].([]any)
	injected, _ := msgs[0].(map[string]any)
	contentStr, _ := injected["content"].(string)
	if strings.Count(contentStr, "工具名: map_column") != 1 {
		t.Errorf("注入消息应只含强制指定的工具，得到 %q", contentStr)
	}
	if strings.Contains(contentStr, "工具名: other_tool") {
		t.Errorf("注入消息不应含其他工具")
	}
}

// TestCallToolChoiceForcedUnknown：强制指定不在声明工具集的工具 → 校验错误。
func TestCallToolChoiceForcedUnknown(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody("x")}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	s := NewStrategies(backend.NewClient())
	params := baseParams()
	params["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "ghost_tool"}}

	_, err := s.Call(context.Background(), m, params, testTools)
	if err == nil || !strings.Contains(err.Error(), "ghost_tool") {
		t.Fatalf("强制指定未知工具应报错，得到 %v", err)
	}
	if rb.count() != 0 {
		t.Errorf("非法 tool_choice 不应发起上游调用，调用次数=%d", rb.count())
	}
}

// TestInjectMessagesMergesExistingSystem：消费方首条消息已是 system → 注入内容
// 与消费方 system 合并为单条（评审修正 F3：避免双 system 冲突）。
func TestInjectMessagesMergesExistingSystem(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "你是财务助手"},
		map[string]any{"role": "user", "content": "你好"},
	}
	out := InjectMessages("注入内容", messages)
	if len(out) != 2 {
		t.Fatalf("合并后应仍为 2 条消息，得到 %d", len(out))
	}
	first, _ := out[0].(map[string]any)
	content, _ := first["content"].(string)
	if !strings.Contains(content, "注入内容") || !strings.Contains(content, "你是财务助手") {
		t.Errorf("合并 system 应同时含注入与消费方内容，得到 %q", content)
	}
}

// TestSampleValueHonorsEnum：few-shot 示例值应落在 enum 允许列表内（评审修正 F4）。
func TestSampleValueHonorsEnum(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"level": map[string]any{"type": "string", "enum": []any{"low", "medium", "high"}},
		},
		"required": []any{"level"},
	}
	tool := Tool{Type: "function", Function: ToolFunction{Name: "set_level", Parameters: params}}
	exs := defaultExamples([]Tool{tool})
	if len(exs) != 1 {
		t.Fatalf("应有 1 条示例，得到 %d", len(exs))
	}
	if !strings.Contains(exs[0].ToolCall, `"level":"low"`) {
		t.Errorf("示例值应取 enum 首项 low，得到 %q", exs[0].ToolCall)
	}
}
