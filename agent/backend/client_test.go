package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZhcChen/qfy-agent/agent/registry"
)

// testModel 构造指向 httptest 后端的注册表模型（ID 与后端 model id 故意不同，
// 用于验证 model 翻译，评审修正 F4）。
func testModel(t *testing.T, baseURL string) *registry.Model {
	t.Helper()
	yaml := fmt.Sprintf(`
models:
  - id: gemma-4-e4b
    backend: openai-compatible
    base_url: %s
    api_key: sk-test
    model: google/gemma-4-e4b
    capabilities:
      tool_calling: full
      json_mode: true
      streaming: true
    default_params:
      temperature: 0.2
      max_tokens: 2048
`, baseURL)
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

const okCompletion = `{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func TestCallRequestHeadersBodyAndNormalization(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type 应为 application/json，得到 %q", ct)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("请求路径应为 BaseURL + /chat/completions，得到 %q", r.URL.Path)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
		}
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okCompletion)
	}))
	defer srv.Close()

	m := testModel(t, srv.URL)
	c := NewClient()
	resp, err := c.Call(context.Background(), m, map[string]any{
		"model":                 "gemma-4-e4b",
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens":            100,
		"max_completion_tokens": 50,
		"tools":                 []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
	})
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization 应为 Bearer sk-test，得到 %q", gotAuth)
	}
	if gotBody["model"] != "google/gemma-4-e4b" {
		t.Errorf("上游请求 model 应为后端 id google/gemma-4-e4b，得到 %v", gotBody["model"])
	}
	if gotBody["temperature"] != 0.2 {
		t.Errorf("未显式传 temperature 时应填入默认值 0.2，得到 %v", gotBody["temperature"])
	}
	if gotBody["max_tokens"] != float64(100) || gotBody["max_completion_tokens"] != float64(50) {
		t.Errorf("双 token 字段应透传，得到 max_tokens=%v max_completion_tokens=%v", gotBody["max_tokens"], gotBody["max_completion_tokens"])
	}
	if tools, ok := gotBody["tools"].([]any); !ok || len(tools) != 1 {
		t.Errorf("tools 应透传，得到 %v", gotBody["tools"])
	}
	// 响应归一化后的结构。
	if resp.ID != "chatcmpl-up" || resp.Choices[0].Message.Content == nil {
		t.Errorf("响应归一化异常: %+v", resp)
	}
	var content string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &content); err != nil || content != "ok" {
		t.Errorf("content 应保真为 ok，得到 %s（err=%v）", resp.Choices[0].Message.Content, err)
	}
}

// TestCallModelTranslationF4：注册表 ID ≠ 后端 model id 时，上游请求 model 字段必须是后端 id。
func TestCallModelTranslationF4(t *testing.T) {
	var upstreamModel any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		upstreamModel = body["model"]
		io.WriteString(w, okCompletion)
	}))
	defer srv.Close()
	m := testModel(t, srv.URL) // ID=gemma-4-e4b，后端 id=google/gemma-4-e4b
	c := NewClient()
	if _, err := c.Call(context.Background(), m, map[string]any{"model": "gemma-4-e4b", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}); err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if upstreamModel != "google/gemma-4-e4b" {
		t.Errorf("上游请求 model 必须为后端 model id（注册表 ID 与后端 id 可不同），得到 %v", upstreamModel)
	}
	if upstreamModel == "gemma-4-e4b" {
		t.Error("上游请求 model 不应是注册表 ID")
	}
}

func TestCallNon2xxReturnsNormalizedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited: see https://internal.example.com/limits or /var/log/qfy/backend.log","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()
	m := testModel(t, srv.URL)
	c := NewClient()
	_, err := c.Call(context.Background(), m, map[string]any{"model": "gemma-4-e4b"})
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("非 2xx 应返回 *UpstreamError，得到 %v", err)
	}
	if ue.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode 应为 429，得到 %d", ue.StatusCode)
	}
	if ue.Body == nil || ue.Body.Type != "rate_limit_error" {
		t.Errorf("应解析出 type 字段，得到 %+v", ue.Body)
	}
	if strings.Contains(ue.Body.Message, "https://internal.example.com") || strings.Contains(ue.Body.Message, "/var/log/qfy") {
		t.Errorf("错误 message 应剥离内部 URL/路径，得到 %q", ue.Body.Message)
	}
	// 统一错误体形状（KTD8）。
	var shape struct {
		Error ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(ue.Body.JSON(), &shape); err != nil {
		t.Fatalf("统一错误体编码非法: %v", err)
	}
	if shape.Error.Type != "rate_limit_error" || shape.Error.Message == "" {
		t.Errorf("统一错误体形状异常: %+v", shape.Error)
	}
}

func TestCallNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 连接拒绝
	m := testModel(t, url)
	c := NewClient()
	_, err := c.Call(context.Background(), m, map[string]any{"model": "gemma-4-e4b"})
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("网络错误应返回 *UnavailableError，得到 %v", err)
	}
	if ue.Timeout {
		t.Error("连接拒绝不应标记为超时")
	}
}

func TestCallTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		io.WriteString(w, okCompletion)
	}))
	defer srv.Close()
	m := testModel(t, srv.URL)
	c := NewClient(WithTimeouts(50*time.Millisecond, 2*time.Second))
	_, err := c.Call(context.Background(), m, map[string]any{"model": "gemma-4-e4b"})
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("超时应返回 *UnavailableError，得到 %v", err)
	}
	if !ue.Timeout {
		t.Error("超时错误应标记 Timeout=true")
	}
}

func TestCallMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "definitely not json")
	}))
	defer srv.Close()
	m := testModel(t, srv.URL)
	c := NewClient()
	_, err := c.Call(context.Background(), m, map[string]any{"model": "gemma-4-e4b"})
	var me *MalformedError
	if !errors.As(err, &me) {
		t.Fatalf("畸形响应应返回 *MalformedError，得到 %v", err)
	}
}

func TestCallNormalizesMinimalResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()
	m := testModel(t, srv.URL)
	c := NewClient()
	resp, err := c.Call(context.Background(), m, map[string]any{"model": "gemma-4-e4b"})
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if !strings.HasPrefix(resp.ID, "chatcmpl-") || resp.Created <= 0 || resp.Usage == nil || resp.Object != "chat.completion" {
		t.Errorf("缺失字段应补齐为合法骨架，得到 %+v", resp)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("缺失 finish_reason 应归一化为 stop，得到 %q", resp.Choices[0].FinishReason)
	}
}

func TestStreamReturnsBodyAndForcesStreamFlag(t *testing.T) {
	const sse = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	var gotAuth, gotAccept string
	var gotStream any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		gotStream = body["stream"]
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer srv.Close()
	m := testModel(t, srv.URL)
	c := NewClient()
	body, err := c.Stream(context.Background(), m, map[string]any{"model": "gemma-4-e4b", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("读取流失败: %v", err)
	}
	if string(data) != sse {
		t.Errorf("流内容应原样透传，得到 %q", data)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization 应为 Bearer sk-test，得到 %q", gotAuth)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept 应为 text/event-stream，得到 %q", gotAccept)
	}
	if gotStream != true {
		t.Errorf("流式入口应强制 stream=true，得到 %v", gotStream)
	}
}

// TestStreamReadTimeoutConfigDiffers：流式读取路径超时与常规超时不同（评审修正 G3），
// 可通过配置字段断言。
func TestStreamReadTimeoutConfigDiffers(t *testing.T) {
	c := NewClient(WithTimeouts(30*time.Second, 5*time.Minute))
	if c.RequestTimeout == c.StreamTimeout {
		t.Error("流式读超时应与常规超时不同（独立放宽，G3）")
	}
	if c.requestClient.Timeout != 30*time.Second {
		t.Errorf("非流式 client 超时应为 RequestTimeout，得到 %v", c.requestClient.Timeout)
	}
	if c.streamClient.Timeout != 5*time.Minute {
		t.Errorf("流式 client 超时应为 StreamTimeout，得到 %v", c.streamClient.Timeout)
	}
	d := NewClient()
	if d.StreamTimeout <= d.RequestTimeout {
		t.Error("默认配置下流式超时应大于常规超时")
	}
}

func TestStreamNon2xxReturnsNormalizedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":{"message":"upstream down"}}`)
	}))
	defer srv.Close()
	m := testModel(t, srv.URL)
	c := NewClient()
	body, err := c.Stream(context.Background(), m, map[string]any{"model": "gemma-4-e4b"})
	if body != nil {
		body.Close()
		t.Error("非 2xx 时不应返回响应体")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("流式非 2xx 应返回 *UpstreamError，得到 %v", err)
	}
	if ue.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode 应为 502，得到 %d", ue.StatusCode)
	}
	if ue.Body == nil || ue.Body.Message != "upstream down" {
		t.Errorf("应解析统一错误体，得到 %+v", ue.Body)
	}
}

func TestStreamNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	m := testModel(t, url)
	c := NewClient()
	_, err := c.Stream(context.Background(), m, map[string]any{"model": "gemma-4-e4b"})
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("流式网络错误应返回 *UnavailableError，得到 %v", err)
	}
}

// TestStreamContextCancelPropagates：ctx 必须注入上游请求（NewRequestWithContext）。
// 客户端取消后 Stream 应立即返回 context.Canceled 相关错误，而不是继续等待后端
// （Go http server 仅在 handler 主动读连接时才感知客户端断开，因此这里验证的是
// 客户端侧取消传播：请求中止且连接由 transport 关闭，不再等待后端响应）。
func TestStreamContextCancelPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second) // 后端很慢；若 ctx 未注入，Stream 会等待到响应返回
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	m := testModel(t, srv.URL)
	c := NewClient()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	body, err := c.Stream(ctx, m, map[string]any{"model": "gemma-4-e4b"})
	if body != nil {
		body.Close()
		t.Error("取消后不应返回响应体")
	}
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("context 取消应返回 *UnavailableError，得到 %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("应能识别底层 context.Canceled，得到 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("取消后应立刻返回（不等待后端），耗时 %v", elapsed)
	}
}
