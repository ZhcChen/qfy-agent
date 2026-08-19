package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhcChen/qfy-agent/agent/audit"
	"github.com/ZhcChen/qfy-agent/agent/backend"
	"github.com/ZhcChen/qfy-agent/agent/loop"
	"github.com/ZhcChen/qfy-agent/agent/registry"
)

// testRegistryYAML 以 mock 后端地址渲染注册表（单条模型，能力与示例配置一致）。
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
      json_mode: false
      streaming: true
    default_params:
      temperature: 0.2
`, baseURL)
}

// newTestServer 组装测试服务（mock 后端 + 工具注册 + 审计存储）。
func newTestServer(t *testing.T, upstream *httptest.Server) (*httptest.Server, *auditStore) {
	t.Helper()
	reg, err := registry.Load([]byte(testRegistryYAML(upstream.URL)))
	if err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	client := backend.NewClient()
	audits := newAuditStore(50)
	notifier := audit.NewNotifier()
	notifier.SetOnCall(audits.OnCall)
	tools := loop.NewTools()
	if err := tools.Register("map_column", mapColumnTool(), mapColumnExecutor); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	runner := loop.NewRunner(tools,
		loop.WithHTTPClient(&http.Client{Transport: upstream.Client().Transport}),
		loop.WithOnCall(notifier.Notify))
	s := &server{reg: reg, client: client, runner: runner, tools: tools, audits: audits, notify: notifier.Notify}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv, audits
}

// mockUpstream 可编程 mock 后端：按脚本序列返回非流式或 SSE 响应。
type mockUpstream struct {
	responses []string // 每个元素一个响应体；以 "SSE:" 前缀表示流式
	calls     int
}

func (m *mockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idx := m.calls
		m.calls++
		if idx >= len(m.responses) {
			http.Error(w, "mock 响应耗尽", 500)
			return
		}
		body := m.responses[idx]
		if strings.HasPrefix(body, "SSE:") {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			_, _ = io.WriteString(w, strings.TrimPrefix(body, "SSE:"))
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func completion(content string) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content)
}

func toolCalls() string {
	args := `{"column":"客户名称","standard_field":"customer_name"}`
	enc, _ := json.Marshal(args)
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_0","type":"function","function":{"name":"map_column","arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, enc)
}

func postJSON(t *testing.T, url, path string, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s 失败: %v", path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return string(b)
}

func chatBody(model, content string, useTool bool) string {
	msgs := fmt.Sprintf(`[{"role":"user","content":%q}]`, content)
	return fmt.Sprintf(`{"model":%q,"messages":%s,"use_tool":%v}`, model, msgs, useTool)
}

func TestBuildParamsDisablesThinking(t *testing.T) {
	disabled := false
	params := buildParams(&chatRequest{
		Model:    "gemma-4-e4b",
		Messages: []any{map[string]any{"role": "user", "content": "hi"}},
		Thinking: &disabled,
	})
	if got := params["reasoning_effort"]; got != "none" {
		t.Fatalf("关闭思考应转换为 reasoning_effort=none，得到 %v", got)
	}
}

func TestIndexPage(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET / 失败: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, "qfy-agent 接入演示台") {
		t.Errorf("首页应渲染演示台页面，得到 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "app.js") || !strings.Contains(body, "alpine.min.js") {
		t.Errorf("页面应引用本地 app.js 与 Alpine.js")
	}
}

func TestStaticAssets(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)
	for _, p := range []string{"/static/vendor/htmx.min.js", "/static/vendor/alpine.min.js", "/static/app.js", "/static/style.css"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s 失败: %v", p, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != 200 || len(body) < 100 {
			t.Errorf("静态资源 %s 应可访问（长度 %d）", p, len(body))
		}
	}
}

func getJSON(t *testing.T, url, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(url + path)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", path, err)
	}
	return resp
}

func TestAPIModels(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)
	resp := getJSON(t, srv.URL, "/api/models")
	body := readBody(t, resp)
	if !strings.Contains(body, "gemma-4-e4b") || !strings.Contains(body, `"tool_calling":"none"`) {
		t.Errorf("/api/models 应含模型与能力声明，得到 %s", body)
	}
}

func TestAPITools(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)
	resp := getJSON(t, srv.URL, "/api/tools")
	body := readBody(t, resp)
	if !strings.Contains(body, "map_column") || !strings.Contains(body, "registered") {
		t.Errorf("/api/tools 应含已注册工具，得到 %s", body)
	}
}

// TestChatNonStream：非流式对话（无工具）→ 标准 JSON 响应。
func TestChatNonStream(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{responses: []string{completion("你好")}}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)
	resp := postJSON(t, srv.URL, "/api/chat", chatBody("gemma-4-e4b", "你好", false))
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("chat 应 200，得到 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "你好") {
		t.Errorf("响应应含模型输出，得到 %s", body)
	}
}

// TestChatToolLoop：工具演示——模型先输出 tool_calls，网关自动执行 map_column
// 并回填，第二轮给出最终答案（网关内循环，R16 演示）。
func TestChatToolLoop(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{responses: []string{toolCalls(), completion("「客户名称」已映射为 customer_name")}}).handler())
	defer up.Close()
	srv, audits := newTestServer(t, up)
	resp := postJSON(t, srv.URL, "/api/chat", chatBody("gemma-4-e4b", "请把列「客户名称」映射到标准字段，调用工具", true))
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("chat 应 200，得到 %d: %s", resp.StatusCode, body)
	}
	// 最终回答应包含工具执行后的映射结论。
	if !strings.Contains(body, "customer_name") {
		t.Errorf("工具循环后应给出最终答案（含映射结论），得到 %s", body)
	}
	// 审计应含两条记录（工具调用轮 + 最终回答轮）。
	recs := audits.List()
	if len(recs) < 2 {
		t.Errorf("工具循环应产生至少 2 条审计记录，得到 %d", len(recs))
	}
	foundTool := false
	for _, r := range recs {
		if len(r.ToolResults) > 0 && r.ToolResults[0].Name == "map_column" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Errorf("审计应含工具执行概要，记录=%+v", recs)
	}
}

// TestChatStreamSimulate：stream=true + 工具（none 模型）→ SSE 模拟流
// （网关内循环后模拟输出，含 content 增量与 [DONE]）。
func TestChatStreamSimulate(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{responses: []string{toolCalls(), completion("已映射为 customer_name")}}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)
	resp := postJSON(t, srv.URL, "/api/chat/stream", chatBody("gemma-4-e4b", "映射列「客户名称」", true))
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("stream 应 200，得到 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("应为 SSE 响应，Content-Type=%q", resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("SSE 应以 [DONE] 结尾，得到 %s", body)
	}
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("SSE 应为 chunk 结构，得到 %s", body)
	}
}

// TestChatStreamPassthrough：stream=true 无工具（streaming 后端）→ 真实透传。
func TestChatStreamPassthrough(t *testing.T) {
	chunks := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"透传"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	up := httptest.NewServer((&mockUpstream{responses: []string{"SSE:" + chunks}}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)
	resp := postJSON(t, srv.URL, "/api/chat/stream", chatBody("gemma-4-e4b", "你好", false))
	body := readBody(t, resp)
	if !strings.Contains(body, "透传") || !strings.Contains(body, "[DONE]") {
		t.Errorf("透传流应含上游内容与 [DONE]，得到 %s", body)
	}
}

// TestChatErrors：缺 model / 未知模型 → 明确错误。
func TestChatErrors(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)

	resp := postJSON(t, srv.URL, "/api/chat", `{"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 {
		t.Errorf("缺 model 应 400，得到 %d", resp.StatusCode)
	}
	resp = postJSON(t, srv.URL, "/api/chat", `{"model":"no-such","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 404 {
		t.Errorf("未知模型应 404，得到 %d", resp.StatusCode)
	}
}

// TestAuditStore：内存审计存储的环形语义（新→旧、上限）。
func TestAuditStore(t *testing.T) {
	s := newAuditStore(3)
	for i := 0; i < 5; i++ {
		s.OnCall(audit.CallRecord{Model: fmt.Sprintf("m%d", i)})
	}
	recs := s.List()
	if len(recs) != 3 {
		t.Fatalf("应保留最近 3 条，得到 %d", len(recs))
	}
	if recs[0].Model != "m4" || recs[2].Model != "m2" {
		t.Errorf("List 应按新→旧，得到 %+v", recs)
	}
}

// TestAuditAPI：对话后 /api/audit 返回留痕记录。
func TestAuditAPI(t *testing.T) {
	up := httptest.NewServer((&mockUpstream{responses: []string{completion("你好")}}).handler())
	defer up.Close()
	srv, _ := newTestServer(t, up)
	_ = postJSON(t, srv.URL, "/api/chat", chatBody("gemma-4-e4b", "你好", false))
	resp := getJSON(t, srv.URL, "/api/audit")
	body := readBody(t, resp)
	if !strings.Contains(body, "gemma-4-e4b") || !strings.Contains(body, "strategy") {
		t.Errorf("/api/audit 应含对话留痕，得到 %s", body)
	}
}

func TestDropStreamRemovesStreamOnlyParameters(t *testing.T) {
	params := map[string]any{
		"model":          "gemma-4-e4b",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	got := dropStream(params)
	if _, ok := got["stream"]; ok {
		t.Fatal("非流式上游请求不应保留 stream")
	}
	if _, ok := got["stream_options"]; ok {
		t.Fatal("非流式上游请求不应保留 stream_options")
	}
	if got["model"] != "gemma-4-e4b" {
		t.Fatalf("其他参数应保留，得到 %v", got)
	}
}
