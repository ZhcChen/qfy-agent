package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qfy-agent/qfy-agent/audit"
	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/loop"
	"github.com/qfy-agent/qfy-agent/registry"
	"github.com/qfy-agent/qfy-agent/tooling"
)

// ---- 测试基础设施 ----
//
// auditRecorder 与 parseSSE 复用 sse_test.go 中的定义（同包）。

// testRegistryYAML 以 mock 后端地址渲染注册表 YAML（两条模型，与
// config/models.example.yaml 能力声明一致）。
func testRegistryYAML(baseURL string) string {
	return fmt.Sprintf(`
models:
  - id: gemma-4-e4b
    backend: openai-compatible
    base_url: %s
    api_key: test-key
    model: google/gemma-4-e4b
    capabilities:
      tool_calling: none
      json_mode: true
      streaming: true
    default_params:
      temperature: 0.2
  - id: gemma-4-12b
    backend: openai-compatible
    base_url: %s
    api_key: test-key
    model: google/gemma-4-12b
    capabilities:
      tool_calling: partial
      json_mode: true
      streaming: true
`, baseURL, baseURL)
}

func mustRegistry(t *testing.T, baseURL string) *registry.Registry {
	t.Helper()
	reg, err := registry.Load([]byte(testRegistryYAML(baseURL)))
	if err != nil {
		t.Fatalf("加载测试注册表失败: %v", err)
	}
	return reg
}

// newTestHandler 组装 httptest 服务：注册表指向 mock 后端；审计回调收集到
// recorder（Runner 与 api 层共享同一 Notifier）。
func newTestHandler(t *testing.T, backendURL string, tools *loop.Tools, opts ...loop.Option) (*httptest.Server, *auditRecorder) {
	t.Helper()
	rec := &auditRecorder{}
	notifier := audit.NewNotifier()
	notifier.SetOnCall(rec.onCall)
	runner := loop.NewRunner(tools, append([]loop.Option{loop.WithOnCall(notifier.Notify)}, opts...)...)
	srv := httptest.NewServer(NewHandler(HandlerConfig{
		Registry: mustRegistry(t, backendURL),
		Runner:   runner,
		Client:   backend.NewClient(),
		Notifier: notifier,
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// neverBackend 断言后端不被调用（校验失败路径）。
func neverBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("不应调用后端: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// postChat 向 /v1/chat/completions 发送 JSON 请求并返回响应。
func postChat(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions 失败: %v", err)
	}
	return resp
}

// postRaw 发送原始字符串请求体（超大请求体测试用）。
func postRaw(t *testing.T, url, raw string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(raw))
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	return resp
}

// bodyString 读取并返回响应体全文。
func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return string(b)
}

func decodeResp(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("解析响应 JSON 失败: %v", err)
	}
}

// chatResp 非流式响应骨架的断言结构（id/object/created/model/choices/usage）。
type chatResp struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string `json:"role"`
			Content   any    `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// apiError OpenAI 风格统一错误体（KTD8）。
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeError(t *testing.T, resp *http.Response) apiError {
	t.Helper()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Fatalf("期望错误响应，得到 %d", resp.StatusCode)
	}
	var e apiError
	decodeResp(t, resp, &e)
	return e
}

// weatherTool 请求中 tools 数组用的工具定义（get_weather）。
func weatherTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "查询天气",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string"},
				},
				"required": []any{"city"},
			},
		},
	}
}

// weatherToolingTool 与 weatherTool 对应的 tooling.Tool（注册执行器用）。
func weatherToolingTool() tooling.Tool {
	return tooling.Tool{
		Type: "function",
		Function: tooling.ToolFunction{
			Name:        "get_weather",
			Description: "查询天气",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string"},
				},
				"required": []any{"city"},
			},
		},
	}
}

// 注入策略期望的模型输出（KTD4：{"name","arguments"}）与响应体。
const injectedToolCallContent = `{"name":"get_weather","arguments":{"city":"beijing"}}`

func injectedBody() string {
	enc, _ := json.Marshal(injectedToolCallContent)
	return fmt.Sprintf(`{"id":"up-1","object":"chat.completion","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, enc)
}

// chatBody 构造 content 为指定文本的普通补全响应。
func chatBody(content string) string {
	return fmt.Sprintf(`{"id":"up-2","object":"chat.completion","created":2,"model":"google/gemma-4-e4b","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`, content)
}

// seqBackend 记录全部请求体并按顺序返回预设响应（用尽后重复最后一个）。
type seqBackend struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []map[string]any
}

func newSeqBackend(t *testing.T, resps []string) *seqBackend {
	t.Helper()
	b := &seqBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("后端收到非法 JSON 请求体: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		idx := len(b.reqs)
		b.reqs = append(b.reqs, req)
		b.mu.Unlock()
		if idx >= len(resps) {
			idx = len(resps) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, resps[idx])
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *seqBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reqs)
}

func (b *seqBackend) req(i int) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i < 0 || i >= len(b.reqs) {
		return nil
	}
	return b.reqs[i]
}

// ---- 场景测试 ----

// TestModelsList GET /v1/models 返回标准 list 结构（R1）。
func TestModelsList(t *testing.T) {
	srv, _ := newTestHandler(t, neverBackend(t).URL, nil)
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models 失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type 应为 application/json，得到 %q", ct)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	decodeResp(t, resp, &list)
	if list.Object != "list" {
		t.Errorf("object 应为 list，得到 %q", list.Object)
	}
	if len(list.Data) != 2 {
		t.Fatalf("应返回 2 个模型，得到 %d", len(list.Data))
	}
	// 声明顺序。
	if list.Data[0].ID != "gemma-4-e4b" || list.Data[1].ID != "gemma-4-12b" {
		t.Errorf("模型顺序应为声明顺序，得到 %v / %v", list.Data[0].ID, list.Data[1].ID)
	}
	for _, item := range list.Data {
		if item.Object != "model" {
			t.Errorf("列表项 object 应为 model，得到 %q", item.Object)
		}
		if item.OwnedBy != "qfy-agent" {
			t.Errorf("列表项 owned_by 应为 qfy-agent，得到 %q", item.OwnedBy)
		}
	}
}

// TestChatNonStream POST 非流式返回标准响应骨架（id/object/choices/usage），
// 且上游收到归一化请求（model 翻译、default_params 合并、Authorization）。
func TestChatNonStream(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("请求体非法: %v", err)
			return
		}
		if req["model"] != "google/gemma-4-e4b" {
			t.Errorf("上游 model 应为后端模型 id，得到 %v", req["model"])
		}
		if req["temperature"] != float64(0.2) {
			t.Errorf("default_params.temperature 应合并为 0.2，得到 %v", req["temperature"])
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization 应为 Bearer test-key，得到 %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, chatBody("你好，世界"))
	}))
	defer mock.Close()

	srv, _ := newTestHandler(t, mock.URL, nil)
	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	var cc chatResp
	decodeResp(t, resp, &cc)
	if cc.ID == "" {
		t.Error("响应骨架应含 id")
	}
	if cc.Object != "chat.completion" {
		t.Errorf("object 应为 chat.completion，得到 %q", cc.Object)
	}
	if cc.Created == 0 {
		t.Error("响应骨架应含 created")
	}
	if cc.Model != "gemma-4-e4b" {
		t.Errorf("响应 model 应回显对外模型 id，得到 %q", cc.Model)
	}
	if len(cc.Choices) != 1 || cc.Choices[0].Message.Role != "assistant" {
		t.Errorf("choices 骨架不符: %+v", cc.Choices)
	}
	if got, _ := cc.Choices[0].Message.Content.(string); got != "你好，世界" {
		t.Errorf("content 应为 %q，得到 %v", "你好，世界", cc.Choices[0].Message.Content)
	}
	if cc.Usage == nil || cc.Usage.TotalTokens == 0 {
		t.Errorf("响应骨架应含 usage: %+v", cc.Usage)
	}
}

// TestChatUnknownModel POST 未知模型 → 404 规范错误体（"模型不存在"语义）。
func TestChatUnknownModel(t *testing.T) {
	srv, _ := newTestHandler(t, neverBackend(t).URL, nil)
	resp := postChat(t, srv.URL, map[string]any{
		"model":    "no-such-model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	e := decodeError(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码应为 404，得到 %d", resp.StatusCode)
	}
	if e.Error.Code != "model_not_found" || e.Error.Type == "" || e.Error.Message == "" {
		t.Errorf("错误体字段不符: %+v", e.Error)
	}
}

// TestChatMissingMessages 缺 messages / 空 messages → 400 规范错误体。
func TestChatMissingMessages(t *testing.T) {
	srv, _ := newTestHandler(t, neverBackend(t).URL, nil)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"缺 messages 字段", map[string]any{"model": "gemma-4-e4b"}},
		{"messages 为空数组", map[string]any{"model": "gemma-4-e4b", "messages": []any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postChat(t, srv.URL, tc.body)
			e := decodeError(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码应为 400，得到 %d", resp.StatusCode)
			}
			if e.Error.Code != "missing_messages" || e.Error.Message == "" {
				t.Errorf("错误体字段不符: %+v", e.Error)
			}
		})
	}
}

// TestChatInvalidJSON 请求体非法 JSON → 400 规范错误体。
func TestChatInvalidJSON(t *testing.T) {
	srv, _ := newTestHandler(t, neverBackend(t).URL, nil)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("这不是 JSON"))
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	e := decodeError(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码应为 400，得到 %d", resp.StatusCode)
	}
	if e.Error.Code != "invalid_json" || e.Error.Message == "" {
		t.Errorf("错误体字段不符: %+v", e.Error)
	}
}

// TestChatMalformedTools tools 结构非法（非数组）→ 400 规范错误体。
func TestChatMalformedTools(t *testing.T) {
	srv, _ := newTestHandler(t, neverBackend(t).URL, nil)
	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    "not-an-array",
	})
	e := decodeError(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码应为 400，得到 %d", resp.StatusCode)
	}
	if e.Error.Code != "invalid_tools" || e.Error.Message == "" {
		t.Errorf("错误体字段不符: %+v", e.Error)
	}
}

// TestChatStreamPassthrough stream=true 无 tools → 真实上游流透传
// （客户端收到 data 行与 [DONE]，R11），审计记录含 direct 策略。
func TestChatStreamPassthrough(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
	}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("请求体非法: %v", err)
			return
		}
		if req["stream"] != true {
			t.Errorf("上游应收到 stream=true，得到 %v", req["stream"])
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer mock.Close()

	srv, rec := newTestHandler(t, mock.URL, nil)
	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
		"stream":   true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type 应为 text/event-stream，得到 %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("读取流失败: %v", err)
	}
	events := parseSSE(t, string(body))
	if len(events) != 4 {
		t.Fatalf("应收到 4 个事件（3 chunk + [DONE]），得到 %d: %v", len(events), events)
	}
	if events[3] != "[DONE]" {
		t.Errorf("末事件应为 [DONE]，得到 %q", events[3])
	}
	// 内容增量拼接还原。
	var joined strings.Builder
	for i := 1; i < 3; i++ {
		c := parseChunk(t, events[i])
		if c.Choices[0].Delta.Content != nil {
			joined.WriteString(*c.Choices[0].Delta.Content)
		}
	}
	if joined.String() != "Hello world" {
		t.Errorf("内容增量拼接应为 %q，得到 %q", "Hello world", joined.String())
	}

	// 审计：透传流由 api 层产出（KTD9/G2）。
	recs := rec.get()
	if len(recs) != 1 {
		t.Fatalf("应收到 1 条审计记录，得到 %d", len(recs))
	}
	r := recs[0]
	if !r.Stream || r.Truncated || r.Error != "" {
		t.Errorf("透传记录应为 Stream=true、Truncated=false、Error 为空: %+v", r)
	}
	if r.Model != "gemma-4-e4b" || r.Strategy != "direct" {
		t.Errorf("记录应携带 Model/Strategy: %+v", r)
	}
	if r.Output.Content != "Hello world" {
		t.Errorf("输出摘要应为 %q，得到 %q", "Hello world", r.Output.Content)
	}
}

// TestChatStreamWithToolsSimulate stream=true 带 tools（none 能力模型）→
// 上游走非流式注入调用，随后模拟 SSE 流（含 tool_calls delta 与 usage chunk，G1）。
func TestChatStreamWithToolsSimulate(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("请求体非法: %v", err)
			return
		}
		// 模拟流路径上游为非流式注入调用：不得出现 stream/stream_options/tools。
		if _, ok := req["stream"]; ok {
			t.Errorf("模拟流上游不应收到 stream 字段: %v", req["stream"])
		}
		if _, ok := req["stream_options"]; ok {
			t.Errorf("模拟流上游不应收到 stream_options 字段")
		}
		if _, ok := req["tools"]; ok {
			t.Errorf("none 模型注入策略应移除 tools 字段")
		}
		// 注入的 system 消息应前置（工具列表 + 输出约束，KTD4）。
		msgs, ok := req["messages"].([]any)
		if !ok || len(msgs) == 0 {
			t.Errorf("上游应收到含注入 system 消息的 messages")
		} else if sys, ok := msgs[0].(map[string]any); !ok || sys["role"] != "system" {
			t.Errorf("注入 system 消息应前置，得到 %v", msgs[0])
		} else if c, _ := sys["content"].(string); !strings.Contains(c, "get_weather") || !strings.Contains(c, "输出约束") {
			t.Errorf("注入 system 消息应包含工具列表与输出约束，得到 %.60s", c)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, injectedBody())
	}))
	defer mock.Close()

	srv, rec := newTestHandler(t, mock.URL, nil)
	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "北京天气？"}},
		"tools":    []any{weatherTool()},
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type 应为 text/event-stream，得到 %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("读取流失败: %v", err)
	}
	events := parseSSE(t, string(body))
	if len(events) < 4 {
		t.Fatalf("事件数不足: %v", events)
	}
	if events[len(events)-1] != "[DONE]" {
		t.Errorf("末事件应为 [DONE]，得到 %q", events[len(events)-1])
	}

	// 收集 tool_calls delta：id/name 仅首块，arguments 按增量拼接。
	var name, tcID string
	var args strings.Builder
	var sawRole, sawUsage, sawFinishToolCalls bool
	for _, e := range events[:len(events)-1] {
		c := parseChunk(t, e)
		if len(c.Choices) == 0 {
			// usage chunk：choices 为空数组。
			if c.Usage != nil {
				sawUsage = true
				if got := c.Usage["prompt_tokens"]; got != float64(1) {
					t.Errorf("usage.prompt_tokens 应为 1，得到 %v", got)
				}
			}
			continue
		}
		d := c.Choices[0].Delta
		if d.Role == "assistant" {
			sawRole = true
		}
		if fr, ok := c.Choices[0].FinishReason.(string); ok && fr == "tool_calls" {
			sawFinishToolCalls = true
		}
		for _, tc := range d.ToolCalls {
			if tc.Function.Name != "" {
				tcID, name = tc.ID, tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if !sawRole {
		t.Error("模拟流应含 role 首 chunk")
	}
	if tcID == "" || name != "get_weather" {
		t.Errorf("tool_calls delta 应含 id 与 name get_weather，得到 id=%q name=%q", tcID, name)
	}
	if got := args.String(); got != `{"city":"beijing"}` {
		t.Errorf("arguments 增量拼接应为 %q，得到 %q", `{"city":"beijing"}`, got)
	}
	if !sawFinishToolCalls {
		t.Error("末 chunk finish_reason 应为 tool_calls")
	}
	if !sawUsage {
		t.Error("include_usage=true 时模拟流应发 usage chunk")
	}

	// 审计：模拟流的上游非流式调用由 loop 产出记录（KTD9）。
	recs := rec.get()
	if len(recs) != 1 {
		t.Fatalf("应收到 1 条审计记录，得到 %d", len(recs))
	}
	r := recs[0]
	if r.Stream || r.Strategy != "none" {
		t.Errorf("模拟流记录应为 Stream=false、Strategy=none: %+v", r)
	}
	if len(r.Output.ToolCalls) != 1 || r.Output.ToolCalls[0].Name != "get_weather" {
		t.Errorf("记录输出摘要应含工具调用: %+v", r.Output.ToolCalls)
	}
}

// TestChatWithToolsStandardToolCalls POST 带 tools（none 模型，未注册执行器）→
// 响应含标准 tool_calls（id/type/function.name/function.arguments，R10/KTD3）。
func TestChatWithToolsStandardToolCalls(t *testing.T) {
	mock := newSeqBackend(t, []string{injectedBody()})
	srv, _ := newTestHandler(t, mock.srv.URL, nil) // tools=nil：无执行器
	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "北京天气？"}},
		"tools":    []any{weatherTool()},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	var cc chatResp
	decodeResp(t, resp, &cc)
	if len(cc.Choices) != 1 {
		t.Fatalf("应返回 1 个 choice，得到 %d", len(cc.Choices))
	}
	if cc.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason 应为 tool_calls，得到 %q", cc.Choices[0].FinishReason)
	}
	calls := cc.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("应返回 1 条标准 tool_call，得到 %d", len(calls))
	}
	tc := calls[0]
	if tc.ID == "" || tc.Type != "function" {
		t.Errorf("tool_call 应含 id 与 type=function: %+v", tc)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("function.name 应为 get_weather，得到 %q", tc.Function.Name)
	}
	// function.arguments 是内容为合法 JSON 的字符串。
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("function.arguments 不是合法 JSON 字符串: %v（%q）", err, tc.Function.Arguments)
	}
	if args["city"] != "beijing" {
		t.Errorf("arguments 应为 {\"city\":\"beijing\"}，得到 %v", args)
	}
}

// TestChatResponseFormatPassthrough response_format json_object → 透传到后端
// （mock 后端断言收到该字段，KTD8 未知字段透传）。
func TestChatResponseFormatPassthrough(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("请求体非法: %v", err)
			return
		}
		rf, ok := req["response_format"].(map[string]any)
		if !ok || rf["type"] != "json_object" {
			t.Errorf("上游应收到 response_format.json_object，得到 %v", req["response_format"])
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}]}`)
	}))
	defer mock.Close()

	srv, _ := newTestHandler(t, mock.URL, nil)
	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "返回 JSON"}},
		"response_format": map[string]any{
			"type": "json_object",
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	var cc chatResp
	decodeResp(t, resp, &cc)
	if got, _ := cc.Choices[0].Message.Content.(string); got != `{"ok":true}` {
		t.Errorf("content 应为 %q，得到 %v", `{"ok":true}`, cc.Choices[0].Message.Content)
	}
}

// TestE2EInjectionStrategyWithAudit 端到端：单请求内注入策略完成（mock 后端两次
// 调用，注入后均返回 {name,arguments}；工具执行器在第 0 轮被执行一次，第 1 轮
// 触达轮数上限返回当前响应）→ 响应含标准 tool_calls；审计记录含策略字段
// （验证 Notifier 收到，R17/KTD9）。
func TestE2EInjectionStrategyWithAudit(t *testing.T) {
	mock := newSeqBackend(t, []string{injectedBody(), injectedBody()})

	// 注册 get_weather 执行器：loop 在单请求内自动执行并回填（R16）。
	tools := loop.NewTools()
	var execCount int32
	if err := tools.Register("get_weather", weatherToolingTool(), func(_ context.Context, call backend.ToolCall) (string, error) {
		atomic.AddInt32(&execCount, 1)
		return "北京晴天", nil
	}); err != nil {
		t.Fatalf("注册执行器失败: %v", err)
	}

	srv, rec := newTestHandler(t, mock.srv.URL, tools, loop.WithMaxRounds(2))
	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "北京天气如何？"}},
		"tools":    []any{weatherTool()},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	var cc chatResp
	decodeResp(t, resp, &cc)
	if cc.ID == "" || cc.Object != "chat.completion" || cc.Model != "gemma-4-e4b" {
		t.Errorf("响应骨架不符: id=%q object=%q model=%q", cc.ID, cc.Object, cc.Model)
	}
	if cc.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason 应为 tool_calls，得到 %q", cc.Choices[0].FinishReason)
	}
	// 响应含标准 tool_calls（R10 消费方可见形态）。
	calls := cc.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("应返回 1 条标准 tool_call，得到 %d", len(calls))
	}
	tc := calls[0]
	if tc.ID == "" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool_call 结构不符: %+v", tc)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("function.arguments 不是合法 JSON 字符串: %v", err)
	}
	if args["city"] != "beijing" {
		t.Errorf("arguments 应为 {\"city\":\"beijing\"}，得到 %v", args)
	}

	// mock 后端两次调用，均为注入形态。
	if got := mock.count(); got != 2 {
		t.Fatalf("后端应被调用 2 次，得到 %d", got)
	}
	for i := 0; i < 2; i++ {
		req := mock.req(i)
		if _, ok := req["tools"]; ok {
			t.Errorf("调用 %d 的注入请求不应含 tools 字段", i)
		}
		msgs, _ := req["messages"].([]any)
		sys, _ := msgs[0].(map[string]any)
		if c, _ := sys["content"].(string); !strings.Contains(c, "get_weather") || !strings.Contains(c, "输出约束") {
			t.Errorf("调用 %d 应含工具列表与约束的 system 消息，得到 %.60s", i, c)
		}
	}
	// 第 2 次调用含 role=tool 回填消息（第 0 轮执行结果，R16）。
	msgs1, _ := mock.req(1)["messages"].([]any)
	var sawToolMsg bool
	for _, mm := range msgs1 {
		m, _ := mm.(map[string]any)
		if m["role"] == "tool" {
			if id, _ := m["tool_call_id"].(string); id != "" {
				sawToolMsg = true
			}
		}
	}
	if !sawToolMsg {
		t.Error("第 2 次调用应含 role=tool 回填消息")
	}
	if atomic.LoadInt32(&execCount) != 1 {
		t.Errorf("工具执行器应执行 1 次，得到 %d", atomic.LoadInt32(&execCount))
	}

	// 审计：两轮调用各一条记录，含策略字段（验证 Notifier 收到）。
	recs := rec.get()
	if len(recs) != 2 {
		t.Fatalf("应收到 2 条审计记录，得到 %d", len(recs))
	}
	for i, r := range recs {
		if r.Model != "gemma-4-e4b" {
			t.Errorf("记录 %d Model 应为 gemma-4-e4b，得到 %q", i, r.Model)
		}
		if r.Strategy != "none" {
			t.Errorf("记录 %d Strategy 应为 none，得到 %q", i, r.Strategy)
		}
		if len(r.Input.ToolNames) != 1 || r.Input.ToolNames[0] != "get_weather" {
			t.Errorf("记录 %d 输入摘要应含工具列表: %v", i, r.Input.ToolNames)
		}
		if len(r.Output.ToolCalls) != 1 || r.Output.ToolCalls[0].Name != "get_weather" {
			t.Errorf("记录 %d 输出摘要应含工具调用: %+v", i, r.Output.ToolCalls)
		}
	}
	if recs[0].Round != 0 || recs[1].Round != 1 {
		t.Errorf("记录轮次应为 0/1，得到 %d/%d", recs[0].Round, recs[1].Round)
	}
}

// TestUnknownPathNotFound 未知路径 → 404 规范错误体。
func TestUnknownPathNotFound(t *testing.T) {
	srv, _ := newTestHandler(t, neverBackend(t).URL, nil)
	resp, err := http.Get(srv.URL + "/v1/unknown")
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	e := decodeError(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码应为 404，得到 %d", resp.StatusCode)
	}
	if e.Error.Code != "not_found" || e.Error.Message == "" {
		t.Errorf("错误体字段不符: %+v", e.Error)
	}
}

// TestWrongMethod 方法不符 → 405 规范错误体。
func TestWrongMethod(t *testing.T) {
	srv, _ := newTestHandler(t, neverBackend(t).URL, nil)

	// POST /v1/models → 405。
	resp, err := http.Post(srv.URL+"/v1/models", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /v1/models 失败: %v", err)
	}
	e := decodeError(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models 状态码应为 405，得到 %d", resp.StatusCode)
	}
	if e.Error.Code != "method_not_allowed" || e.Error.Message == "" {
		t.Errorf("错误体字段不符: %+v", e.Error)
	}

	// GET /v1/chat/completions → 405。
	resp2, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET /v1/chat/completions 失败: %v", err)
	}
	e2 := decodeError(t, resp2)
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/chat/completions 状态码应为 405，得到 %d", resp2.StatusCode)
	}
	if e2.Error.Code != "method_not_allowed" {
		t.Errorf("错误体 code 应为 method_not_allowed，得到 %q", e2.Error.Code)
	}
}

// TestChatStreamingFalseModelSimulates：streaming:false 能力模型 + stream=true
// 无 tools → 走非流式调用 + 模拟流（评审修正：R12 缓冲模拟按能力触发，不静默透传）。
func TestChatStreamingFalseModelSimulates(t *testing.T) {
	content := `{"ok": true}`
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fmt.Sprintf(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content))
	}))
	defer upstream.Close()

	// 注册表：streaming: false 的模型。
	regYAML := fmt.Sprintf(`
models:
  - id: no-stream
    backend: openai-compatible
    base_url: %s
    model: no-stream-backend
    capabilities:
      tool_calling: none
      json_mode: true
      streaming: false
`, upstream.URL)
	reg, err := registry.Load([]byte(regYAML))
	if err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	rec := &auditRecorder{}
	notifier := audit.NewNotifier()
	notifier.SetOnCall(rec.onCall)
	runner := loop.NewRunner(nil, loop.WithOnCall(notifier.Notify))
	srv := httptest.NewServer(NewHandler(HandlerConfig{
		Registry: reg,
		Runner:   runner,
		Client:   backend.NewClient(),
		Notifier: notifier,
	}))
	defer srv.Close()

	resp := postChat(t, srv.URL, map[string]any{
		"model":    "no-stream",
		"messages": []any{map[string]any{"role": "user", "content": "输出 JSON"}},
		"stream":   true,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("应返回 200，得到 %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	events := parseSSE(t, bodyString(t, resp))
	if len(events) < 2 || events[len(events)-1] != "[DONE]" {
		t.Fatalf("应为模拟 SSE 流并以 [DONE] 结尾，事件数=%d", len(events))
	}
	if upstreamCalls.Load() != 1 {
		t.Errorf("上游应为 1 次非流式调用，得到 %d", upstreamCalls.Load())
	}
	// 模拟流应含 content 增量。
	joined := strings.Join(events, "")
	if !strings.Contains(joined, "ok") {
		t.Errorf("模拟流应含模型输出内容，得到 %s", joined[:min(len(joined), 200)])
	}
}

// TestChatStreamStringTrue：stream 字段为字符串 "true" → 按布尔处理（评审修正，
// 不静默降级为非流式）。
func TestChatStreamStringTrue(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: "+`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()
	srv, _ := newTestHandler(t, upstream.URL, nil)
	defer srv.Close()

	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   "true",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("字符串 true 应视为流式，得到 %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("应为 SSE 响应，Content-Type=%q", ct)
	}
}

// TestChatStreamInvalidType：stream 字段为非法类型 → 400（不静默降级）。
func TestChatStreamInvalidType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv, _ := newTestHandler(t, upstream.URL, nil)
	defer srv.Close()

	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   123,
	})
	if resp.StatusCode != 400 {
		t.Fatalf("非法 stream 类型应 400，得到 %d", resp.StatusCode)
	}
}

// TestChatBodyTooLarge：超过 1MiB 请求体 → 413（评审修正：不静默截断解析为 400）。
func TestChatBodyTooLarge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv, _ := newTestHandler(t, upstream.URL, nil)
	defer srv.Close()

	big := `{"model":"gemma-4-e4b","messages":[{"role":"user","content":"` + strings.Repeat("x", 1<<20+100) + `"}]}`
	resp := postRaw(t, srv.URL, big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应 413，得到 %d", resp.StatusCode)
	}
}

// TestChatUpstreamErrorMapped502：mock 后端 500 → 502 且错误体保留上游
// message/type/code（KTD8/writeRunError 映射）。
func TestChatUpstreamErrorMapped502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"上游炸了","type":"server_error","code":"internal"}}`)
	}))
	defer upstream.Close()
	srv, _ := newTestHandler(t, upstream.URL, nil)
	defer srv.Close()

	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游 500 应映射 502，得到 %d", resp.StatusCode)
	}
	body := bodyString(t, resp)
	if !strings.Contains(body, "上游炸了") {
		t.Errorf("502 错误体应保留上游 message，得到 %s", body)
	}
}

// TestChatMissingModelField：缺 model → 400 missing_model（评审 testing 补全）。
func TestChatMissingModelField(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv, _ := newTestHandler(t, upstream.URL, nil)
	defer srv.Close()

	resp := postChat(t, srv.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("缺 model 应 400，得到 %d", resp.StatusCode)
	}
	if !strings.Contains(bodyString(t, resp), "missing_model") {
		t.Errorf("错误码应为 missing_model，得到 %s", bodyString(t, resp))
	}
}

// TestChatStreamFailureAudited：流式启动即失败（上游 500）→ 502 + 审计失败记录
// （评审 testing 补全：auditStreamFailure 路径）。
func TestChatStreamFailureAudited(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"boom","type":"server_error"}}`)
	}))
	defer upstream.Close()
	srv, rec := newTestHandler(t, upstream.URL, nil)
	defer srv.Close()

	resp := postChat(t, srv.URL, map[string]any{
		"model":    "gemma-4-e4b",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("流式启动失败应 502，得到 %d", resp.StatusCode)
	}
	found := false
	for _, r := range rec.get() {
		if r.Stream && r.Error != "" && r.Strategy == "direct" {
			found = true
		}
	}
	if !found {
		t.Errorf("流式启动失败应触发审计（Stream=true、Error 非空），记录=%+v", rec.get())
	}
}
