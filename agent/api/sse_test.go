package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qfy-agent/qfy-agent/agent/audit"
	"github.com/qfy-agent/qfy-agent/agent/backend"
)

// ---- 测试基础设施 ----

// auditRecorder 并发安全地收集审计回调记录。
type auditRecorder struct {
	mu      sync.Mutex
	records []audit.CallRecord
}

func (r *auditRecorder) onCall(rec audit.CallRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

func (r *auditRecorder) get() []audit.CallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.CallRecord(nil), r.records...)
}

// memWriter 线程安全的内存 ResponseWriter（含 Flusher），用于不依赖真实 HTTP 的断言。
type memWriter struct {
	h       http.Header
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
}

func (m *memWriter) Header() http.Header { return m.h }
func (m *memWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(p)
}
func (m *memWriter) WriteHeader(int) {}
func (m *memWriter) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushes++
}
func (m *memWriter) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.String()
}

// nonFlusher 包装 ResponseWriter 但不暴露 Flush，用于 Flusher 断言失败路径。
type nonFlusher struct{ http.ResponseWriter }

// parseSSE 把原始响应体解析为事件列表（data 行内容，多行以 \n 连接；注释行忽略）。
func parseSSE(t *testing.T, body string) []string {
	t.Helper()
	var events []string
	var data []string
	flush := func() {
		if len(data) > 0 {
			events = append(events, strings.Join(data, "\n"))
			data = nil
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			data = append(data, v)
		}
	}
	flush()
	return events
}

// serveProxy 启动下游服务器：handler 内以上游 URL 发起流式请求并把 body 交给 ProxyStream。
// 返回服务器、审计收集器与 ProxyStream 返回的错误通道。
func serveProxy(t *testing.T, upstreamURL string, opts ProxyOptions) (*httptest.Server, *auditRecorder, <-chan error) {
	t.Helper()
	rec := &auditRecorder{}
	if opts.OnCall == nil {
		opts.OnCall = rec.onCall
	}
	errCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, nil)
		if err != nil {
			errCh <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- ProxyStream(r.Context(), w, resp.Body, opts)
	}))
	return srv, rec, errCh
}

// getBody GET url 并返回响应体（断言 200）。
func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return string(b)
}

// testChunk 测试用的 chunk 结构（覆盖本测试所需的规范字段）。
type testChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

func parseChunk(t *testing.T, data string) testChunk {
	t.Helper()
	var c testChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		t.Fatalf("chunk JSON 解析失败 %q: %v", data, err)
	}
	return c
}

// ---- 透传（R11） ----

// TestProxyStreamPassthrough 透传：模拟上游流 → 客户端收到逐事件 data 行与 [DONE]；
// 正常结束触发审计（含耗时、输出摘要）。
func TestProxyStreamPassthrough(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	opts := ProxyOptions{Model: "gemma-4-e4b", Strategy: "direct", Round: 0}
	srv, rec, errCh := serveProxy(t, upstream.URL, opts)
	defer srv.Close()

	body := getBody(t, srv.URL)
	events := parseSSE(t, body)
	if len(events) != 4 {
		t.Fatalf("应收到 4 个事件（3 chunk + [DONE]），得到 %d: %v", len(events), events)
	}
	if events[3] != "[DONE]" {
		t.Errorf("末事件应为 [DONE]，得到 %q", events[3])
	}
	for i := 0; i < 3; i++ {
		c := parseChunk(t, events[i])
		if c.ID != "chatcmpl-1" || c.Object != "chat.completion.chunk" || c.Model != "gemma-4-e4b" {
			t.Errorf("chunk %d 骨架不符: %+v", i, c)
		}
	}

	if err := <-errCh; err != nil {
		t.Errorf("ProxyStream 正常透传不应返回错误: %v", err)
	}
	recs := rec.get()
	if len(recs) != 1 {
		t.Fatalf("应收到 1 条审计记录，得到 %d", len(recs))
	}
	r := recs[0]
	if r.Truncated || r.Error != "" || !r.Stream {
		t.Errorf("正常结束记录应为 Stream=true、Truncated=false、Error 为空: %+v", r)
	}
	if r.Model != "gemma-4-e4b" || r.Strategy != "direct" {
		t.Errorf("记录应携带 Model/Strategy: %+v", r)
	}
	if r.Output.Content != "Hello world" {
		t.Errorf("输出摘要 content 应为 %q，得到 %q", "Hello world", r.Output.Content)
	}
	if r.Duration <= 0 {
		t.Errorf("记录应含正耗时，得到 %v", r.Duration)
	}
	if r.Timestamp.IsZero() {
		t.Errorf("记录应含时间戳")
	}
}

// TestProxyStreamFinishReasonWhitelist 透传：KTD8 读改写——非规范 object/finish_reason
// 白名单化、未知字段丢弃、usage 原样保留。
func TestProxyStreamFinishReasonWhitelist(t *testing.T) {
	chunks := []string{
		// 中间 chunk：非规范 object 与多余字段（logprobs/system_fingerprint/service_tier）应被规范化/丢弃。
		`{"id":"c1","object":"chat.completion","created":2,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"logprobs":null,"finish_reason":null}],"system_fingerprint":"fp_abc","service_tier":"default"}`,
		// 内容 chunk：未知字段丢弃。
		`{"id":"c1","object":"chat.completion.chunk","created":2,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		// 末 chunk：废弃枚举 function_call → 语义等价映射为 tool_calls（评审修正）。
		`{"id":"c1","object":"chat.completion.chunk","created":2,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`,
		// usage chunk：usage 原样保留（含非白名单 details 子字段）。
		`{"id":"c1","object":"chat.completion.chunk","created":2,"model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"completion_tokens_details":{"reasoning_tokens":2}}}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	srv, _, errCh := serveProxy(t, upstream.URL, ProxyOptions{})
	defer srv.Close()

	events := parseSSE(t, getBody(t, srv.URL))
	if len(events) != 5 {
		t.Fatalf("应收到 5 个事件，得到 %d: %v", len(events), events)
	}

	c0 := parseChunk(t, events[0])
	if c0.Object != "chat.completion.chunk" {
		t.Errorf("chunk0 object 应规范化为 chat.completion.chunk，得到 %q", c0.Object)
	}
	if raw := events[0]; strings.Contains(raw, "system_fingerprint") || strings.Contains(raw, "service_tier") || strings.Contains(raw, "logprobs") {
		t.Errorf("chunk0 应丢弃未知字段，原样输出: %s", raw)
	}
	if c0.Choices[0].FinishReason != nil {
		t.Errorf("chunk0 finish_reason 应为 null，得到 %v", c0.Choices[0].FinishReason)
	}
	if c0.Choices[0].Delta.Role != "assistant" {
		t.Errorf("chunk0 delta.role 应为 assistant，得到 %q", c0.Choices[0].Delta.Role)
	}

	c2 := parseChunk(t, events[2])
	if fr, _ := c2.Choices[0].FinishReason.(string); fr != "tool_calls" {
		t.Errorf("chunk2 function_call 应语义等价映射为 tool_calls，得到 %v", c2.Choices[0].FinishReason)
	}

	c3 := parseChunk(t, events[3])
	if c3.Usage == nil {
		t.Fatal("usage chunk 应保留 usage 字段")
	}
	if got := c3.Usage["prompt_tokens"]; got != float64(5) {
		t.Errorf("usage.prompt_tokens 应为 5，得到 %v", got)
	}
	if len(c3.Choices) != 0 {
		t.Errorf("usage chunk choices 应为空数组，得到 %d 个", len(c3.Choices))
	}
	details, ok := c3.Usage["completion_tokens_details"].(map[string]any)
	if !ok || details["reasoning_tokens"] != float64(2) {
		t.Errorf("usage 子字段应原样保留（透传原样），得到 %v", c3.Usage["completion_tokens_details"])
	}

	if events[4] != "[DONE]" {
		t.Errorf("末事件应为 [DONE]，得到 %q", events[4])
	}
	if err := <-errCh; err != nil {
		t.Errorf("ProxyStream 不应返回错误: %v", err)
	}
}

func TestProxyStreamRejectsReasoningOnlyLengthResponse(t *testing.T) {
	chunks := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":2,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"internal"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":2,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":" reasoning"},"finish_reason":"length"}]}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, rec, errCh := serveProxy(t, upstream.URL, ProxyOptions{Model: "gemma-4-e4b", Strategy: "direct"})
	defer srv.Close()
	body := getBody(t, srv.URL)
	events := parseSSE(t, body)
	if len(events) != 4 {
		t.Fatalf("应收到 2 个白名单 chunk、错误事件和 [DONE]，得到 %d: %v", len(events), events)
	}
	if strings.Contains(body, "internal reasoning") || strings.Contains(body, "reasoning_content") {
		t.Fatalf("不得向客户端泄露 reasoning_content，得到 %s", body)
	}
	var errorEvent struct {
		Error backend.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal([]byte(events[2]), &errorEvent); err != nil {
		t.Fatalf("错误事件应为 JSON: %v", err)
	}
	if errorEvent.Error.Code != "upstream_incomplete_response" {
		t.Errorf("错误码应稳定为 upstream_incomplete_response，得到 %q", errorEvent.Error.Code)
	}
	if events[3] != "[DONE]" {
		t.Errorf("错误事件后应以 [DONE] 结束，得到 %q", events[3])
	}
	var incomplete *backend.IncompleteResponseError
	if err := <-errCh; !errors.As(err, &incomplete) {
		t.Fatalf("应返回 *backend.IncompleteResponseError，得到 %v", err)
	}
	recs := rec.get()
	if len(recs) != 1 || recs[0].Error == "" || recs[0].Truncated {
		t.Fatalf("应记录非截断的稳定错误审计，得到 %+v", recs)
	}
}

func TestProxyStreamRejectsWhitespaceContentWithReasoningOnlyLength(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"content":" \n","reasoning_content":"internal"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":" reasoning"},"finish_reason":"length"}]}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, _, errCh := serveProxy(t, upstream.URL, ProxyOptions{Model: "gemma-4-e4b", Strategy: "direct"})
	defer srv.Close()
	body := getBody(t, srv.URL)
	if !strings.Contains(body, `"code":"upstream_incomplete_response"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("空白正文不应掩盖推理截断错误: %s", body)
	}
	var incomplete *backend.IncompleteResponseError
	if err := <-errCh; !errors.As(err, &incomplete) {
		t.Fatalf("应返回 *backend.IncompleteResponseError，得到 %v", err)
	}
}

func TestProxyStreamRejectsMalformedChunkWithoutLeakingIt(t *testing.T) {
	secret := "private-reasoning"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q},}]}\n\n", secret)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, _, errCh := serveProxy(t, upstream.URL, ProxyOptions{Model: "gemma-4-e4b", Strategy: "direct"})
	defer srv.Close()
	body := getBody(t, srv.URL)
	if strings.Contains(body, secret) || strings.Contains(body, "reasoning_content") {
		t.Fatalf("畸形 chunk 不得原样透传: %s", body)
	}
	if !strings.Contains(body, `"code":"upstream_malformed_response"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("应返回受控错误事件并结束流: %s", body)
	}
	var malformed *backend.MalformedError
	if err := <-errCh; !errors.As(err, &malformed) {
		t.Fatalf("应返回 *backend.MalformedError，得到 %v", err)
	}
}

// TestProxyStreamTruncated 透传：上游缺 [DONE]（提前 EOF）→ 错误事件 + [DONE]，
// Truncated=true 的 CallRecord 到达 OnCall。
func TestProxyStreamTruncated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		// 直接返回：无 [DONE]。
	}))
	defer upstream.Close()

	srv, rec, errCh := serveProxy(t, upstream.URL, ProxyOptions{Model: "m", Strategy: "direct"})
	defer srv.Close()

	events := parseSSE(t, getBody(t, srv.URL))
	if len(events) != 3 {
		t.Fatalf("应收到 3 个事件（chunk + 错误事件 + [DONE]），得到 %d: %v", len(events), events)
	}
	if events[2] != "[DONE]" {
		t.Errorf("截断后应以 [DONE] 收尾，得到 %q", events[2])
	}
	var errBody struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(events[1]), &errBody); err != nil {
		t.Fatalf("错误事件应为标准错误体 JSON: %v（%q）", err, events[1])
	}
	if errBody.Error.Type != "stream_error" || errBody.Error.Code != "stream_truncated" {
		t.Errorf("错误事件 type/code 不符: %+v", errBody.Error)
	}

	err := <-errCh
	if !errors.Is(err, ErrStreamTruncated) {
		t.Errorf("截断应返回 ErrStreamTruncated，得到 %v", err)
	}

	recs := rec.get()
	if len(recs) != 1 {
		t.Fatalf("应收到 1 条审计记录，得到 %d", len(recs))
	}
	r := recs[0]
	if !r.Truncated {
		t.Errorf("截断记录 Truncated 应为 true: %+v", r)
	}
	if !r.Stream {
		t.Errorf("截断记录 Stream 应为 true")
	}
	if r.Error == "" {
		t.Errorf("截断记录 Error 应非空")
	}
	if r.Output.Content != "partial" {
		t.Errorf("截断记录应含已转发内容前缀，得到 %q", r.Output.Content)
	}
}

// TestProxyStreamMalformedForward：畸形 data 行不得绕过白名单原样透传。
func TestProxyStreamMalformedForward(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: not json at all\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	srv, rec, errCh := serveProxy(t, upstream.URL, ProxyOptions{})
	defer srv.Close()

	events := parseSSE(t, getBody(t, srv.URL))
	if len(events) != 2 || !strings.Contains(events[0], `"code":"upstream_malformed_response"`) || events[1] != "[DONE]" {
		t.Fatalf("畸形行应转换为受控错误并结束流: %v", events)
	}
	var malformed *backend.MalformedError
	if err := <-errCh; !errors.As(err, &malformed) {
		t.Errorf("畸形行应返回 *backend.MalformedError: %v", err)
	}
	if recs := rec.get(); len(recs) != 1 || recs[0].Truncated || recs[0].Error == "" {
		t.Errorf("畸形行场景应记录非截断错误: %+v", recs)
	}
}

// TestProxyStreamClientDisconnect 透传：客户端断开 → ctx 取消传播到上游请求 → 终止写出，
// 以部分记录触发审计（F4）。
func TestProxyStreamClientDisconnect(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	var once sync.Once
	chunk := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			once.Do(func() { close(upstreamCancelled) })
		case <-time.After(10 * time.Second):
			t.Error("上游请求未被取消")
		}
	}))
	defer upstream.Close()

	rec := &auditRecorder{}
	errCh := make(chan error, 1)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.URL, nil)
		if err != nil {
			errCh <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- ProxyStream(r.Context(), w, resp.Body, ProxyOptions{Model: "m", OnCall: rec.onCall})
	}))
	defer downstream.Close()

	// 客户端读取部分数据后主动断开。
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET 下游失败: %v", err)
	}
	buf := make([]byte, 256)
	if _, rerr := resp.Body.Read(buf); rerr != nil && rerr != io.EOF {
		t.Logf("预读响应体: %v", rerr)
	}
	cancel() // 客户端断开
	resp.Body.Close()

	select {
	case <-upstreamCancelled:
		// 客户端断开经 context 取消上游请求。
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后上游请求未被取消")
	}

	select {
	case perr := <-errCh:
		if perr == nil {
			t.Errorf("客户端断开后 ProxyStream 应返回错误，得到 nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProxyStream 未返回")
	}

	recs := rec.get()
	if len(recs) != 1 {
		t.Fatalf("应收到 1 条部分审计记录，得到 %d", len(recs))
	}
	r := recs[0]
	if !r.Stream || r.Truncated {
		t.Errorf("客户端断开记录应为 Stream=true、Truncated=false: %+v", r)
	}
	if r.Error == "" {
		t.Errorf("客户端断开记录 Error 应非空")
	}
	if r.Output.Content != "partial" {
		t.Errorf("部分记录应含已转发内容前缀，得到 %q", r.Output.Content)
	}
}

// ---- 模拟流（R12） ----

// serveSimulate 启动下游服务器，其 handler 调用 SimulateStream。
func serveSimulate(t *testing.T, resp *backend.ChatCompletion, opts SimulateOptions) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := SimulateStream(r.Context(), w, resp, opts); err != nil {
			t.Errorf("SimulateStream: %v", err)
		}
	}))
}

// TestSimulateStreamContent 模拟流：内容增量分块、首 chunk 带 role、末 chunk finish_reason、[DONE]。
func TestSimulateStreamContent(t *testing.T) {
	content := "hello world, this is a simulated stream"
	resp := &backend.ChatCompletion{
		ID: "chatcmpl-sim", Object: "chat.completion", Created: 123, Model: "m",
		Choices: []backend.Choice{{
			Index:        0,
			Message:      backend.Message{Role: "assistant", Content: json.RawMessage(`"` + content + `"`)},
			FinishReason: "stop",
		}},
	}
	srv := serveSimulate(t, resp, SimulateOptions{ChunkSize: 8})
	defer srv.Close()

	events := parseSSE(t, getBody(t, srv.URL))
	if len(events) < 3 {
		t.Fatalf("事件数不足: %v", events)
	}
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("末事件应为 [DONE]，得到 %q", events[len(events)-1])
	}

	// 首 chunk：delta.role="assistant"（仅此一次），无 content。
	first := parseChunk(t, events[0])
	if first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("首 chunk delta.role 应为 assistant，得到 %q", first.Choices[0].Delta.Role)
	}
	if first.Choices[0].Delta.Content != nil {
		t.Errorf("首 chunk 不应带 content")
	}

	// 内容增量块：逐块 ≤8 rune，拼接还原原文。
	var joined strings.Builder
	for _, e := range events[1 : len(events)-2] {
		c := parseChunk(t, e)
		d := c.Choices[0].Delta
		if d.Content == nil {
			t.Fatalf("中间 chunk 应只含 content 增量: %q", e)
		}
		if len([]rune(*d.Content)) > 8 {
			t.Errorf("chunk 超过 8 rune: %q", *d.Content)
		}
		if d.Role != "" {
			t.Errorf("内容 chunk 不应重复携带 role")
		}
		joined.WriteString(*d.Content)
	}
	if joined.String() != content {
		t.Errorf("内容增量拼接应为 %q，得到 %q", content, joined.String())
	}

	// 末 chunk：delta={} 且 finish_reason 非 null。
	last := parseChunk(t, events[len(events)-2])
	if len(last.Choices) != 1 {
		t.Fatalf("末 chunk 应有 1 个 choice")
	}
	if last.Choices[0].FinishReason == nil {
		t.Errorf("末 chunk finish_reason 应非 null")
	} else if fr, _ := last.Choices[0].FinishReason.(string); fr != "stop" {
		t.Errorf("末 chunk finish_reason 应为 stop，得到 %v", last.Choices[0].FinishReason)
	}
	if rawLast := events[len(events)-2]; strings.Contains(rawLast, `"content"`) || strings.Contains(rawLast, `"role"`) {
		t.Errorf("末 chunk delta 应为 {}（无 content/role）: %s", rawLast)
	}

	// 全程同 id / object。
	for _, e := range events[:len(events)-1] {
		c := parseChunk(t, e)
		if c.ID != "chatcmpl-sim" || c.Object != "chat.completion.chunk" || c.Created != 123 || c.Model != "m" {
			t.Errorf("chunk 骨架应全程一致: %+v", c)
		}
	}
}

// TestSimulateStreamToolCalls 模拟流：tool_calls delta 按 index 增量（id/name 仅首块）。
func TestSimulateStreamToolCalls(t *testing.T) {
	args1 := `{"city":"beijing","unit":"celsius"}`
	args2 := `{"tz":"asia/shanghai"}`
	args1Raw, _ := json.Marshal(args1)
	args2Raw, _ := json.Marshal(args2)
	resp := &backend.ChatCompletion{
		ID: "chatcmpl-sim", Object: "chat.completion", Created: 1, Model: "m",
		Choices: []backend.Choice{{
			Index: 0,
			Message: backend.Message{
				Role: "assistant",
				ToolCalls: []backend.ToolCall{
					{ID: "call_1", Type: "function", Function: backend.Function{Name: "get_weather", Arguments: args1Raw}},
					{ID: "call_2", Type: "function", Function: backend.Function{Name: "get_time", Arguments: args2Raw}},
				},
			},
			FinishReason: "tool_calls",
		}},
	}
	srv := serveSimulate(t, resp, SimulateOptions{ChunkSize: 8})
	defer srv.Close()

	events := parseSSE(t, getBody(t, srv.URL))
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("末事件应为 [DONE]")
	}

	// 按 index 收集 delta 流。
	type callStream struct {
		firstWithName int // 首个携带 id/name 的 chunk 序号
		id, name      string
		args          strings.Builder
		argChunks     int
	}
	streams := map[int]*callStream{}
	firstRole := -1
	for i, e := range events[:len(events)-1] {
		c := parseChunk(t, e)
		if len(c.Choices) == 0 {
			continue
		}
		d := c.Choices[0].Delta
		if d.Role == "assistant" {
			if firstRole != -1 {
				t.Errorf("role 首 chunk 出现多次（事件 %d 与 %d）", firstRole, i)
			}
			firstRole = i
		}
		for _, tc := range d.ToolCalls {
			cs := streams[tc.Index]
			if cs == nil {
				cs = &callStream{firstWithName: -1}
				streams[tc.Index] = cs
			}
			if tc.ID != "" || tc.Function.Name != "" {
				if cs.firstWithName != -1 {
					t.Errorf("index %d 的 id/name 出现多次（事件 %d 与 %d）", tc.Index, cs.firstWithName, i)
				}
				cs.firstWithName = i
				cs.id, cs.name = tc.ID, tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				cs.argChunks++
				cs.args.WriteString(tc.Function.Arguments)
			}
		}
	}

	if firstRole == -1 {
		t.Errorf("缺少 role 首 chunk")
	}
	if len(streams) != 2 {
		t.Fatalf("应有 2 个工具调用流，得到 %d", len(streams))
	}
	want := map[int][3]string{
		0: {"call_1", "get_weather", args1},
		1: {"call_2", "get_time", args2},
	}
	for idx, w := range want {
		cs := streams[idx]
		if cs == nil {
			t.Fatalf("缺少 index %d 的工具调用流", idx)
		}
		if cs.id != w[0] || cs.name != w[1] {
			t.Errorf("index %d id/name 应为 %v/%v，得到 %v/%v", idx, w[0], w[1], cs.id, cs.name)
		}
		if cs.args.String() != w[2] {
			t.Errorf("index %d arguments 增量拼接应为 %q，得到 %q", idx, w[2], cs.args.String())
		}
		if cs.argChunks < 2 {
			t.Errorf("index %d arguments 应分多块增量发出，得到 %d 块", idx, cs.argChunks)
		}
	}

	// 末 chunk：finish_reason=tool_calls（白名单保留），delta={}。
	last := parseChunk(t, events[len(events)-2])
	if fr, _ := last.Choices[0].FinishReason.(string); fr != "tool_calls" {
		t.Errorf("末 chunk finish_reason 应为 tool_calls，得到 %v", last.Choices[0].FinishReason)
	}
}

// TestSimulateStreamIncludeUsage 模拟流：include_usage 时发 usage chunk（choices 为空数组）后 [DONE]。
func TestSimulateStreamIncludeUsage(t *testing.T) {
	resp := &backend.ChatCompletion{
		ID: "chatcmpl-sim", Created: 1, Model: "m",
		Choices: []backend.Choice{{
			Index:        0,
			Message:      backend.Message{Role: "assistant", Content: json.RawMessage(`"hi"`)},
			FinishReason: "stop",
		}},
		Usage: &backend.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
	}

	srv := serveSimulate(t, resp, SimulateOptions{IncludeUsage: true})
	defer srv.Close()
	events := parseSSE(t, getBody(t, srv.URL))
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("末事件应为 [DONE]")
	}
	usageChunk := parseChunk(t, events[len(events)-2])
	if usageChunk.Usage == nil {
		t.Fatalf("include_usage 时末 chunk 前应发 usage chunk")
	}
	if len(usageChunk.Choices) != 0 {
		t.Errorf("usage chunk choices 应为空数组，得到 %d 个", len(usageChunk.Choices))
	}
	if got := usageChunk.Usage["prompt_tokens"]; got != float64(5) {
		t.Errorf("usage.prompt_tokens 应为 5，得到 %v", got)
	}

	// 未启用 include_usage：不出现 usage。
	srv2 := serveSimulate(t, resp, SimulateOptions{})
	defer srv2.Close()
	body2 := getBody(t, srv2.URL)
	if strings.Contains(body2, `"usage"`) {
		t.Errorf("未启用 include_usage 时不应发 usage chunk: %s", body2)
	}
}

// TestSimulateStreamEmptyChoices 模拟流：空 choices 响应退化为末 chunk + [DONE]，流保持合法。
func TestSimulateStreamEmptyChoices(t *testing.T) {
	resp := &backend.ChatCompletion{ID: "chatcmpl-sim", Created: 1, Model: "m"}
	srv := serveSimulate(t, resp, SimulateOptions{})
	defer srv.Close()

	events := parseSSE(t, getBody(t, srv.URL))
	if len(events) != 2 {
		t.Fatalf("应收到 2 个事件（末 chunk + [DONE]），得到 %d: %v", len(events), events)
	}
	c := parseChunk(t, events[0])
	if fr, _ := c.Choices[0].FinishReason.(string); fr != "stop" {
		t.Errorf("空 choices 的末 chunk finish_reason 应为 stop，得到 %v", c.Choices[0].FinishReason)
	}
	if events[1] != "[DONE]" {
		t.Errorf("末事件应为 [DONE]")
	}
}

// ---- SSE 写出器（KTD7） ----

// TestHeartbeat 心跳：短间隔配置（10ms）验证注释行发出。
func TestHeartbeat(t *testing.T) {
	mw := &memWriter{h: make(http.Header)}
	sse, err := NewSSEWriter(mw, WithHeartbeatInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if got := mw.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type 应为 text/event-stream，得到 %q", got)
	}
	if got := mw.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control 应为 no-cache，得到 %q", got)
	}
	if got := mw.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering 应为 no，得到 %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sse.StartHeartbeat(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(mw.String(), ": keep-alive") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("2s 内未收到 2 个心跳注释行，收到 %d", strings.Count(mw.String(), ": keep-alive"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	sse.Stop()
}

// TestHeartbeatDisabled 心跳：负间隔配置关闭心跳。
func TestHeartbeatDisabled(t *testing.T) {
	mw := &memWriter{h: make(http.Header)}
	sse, err := NewSSEWriter(mw, WithHeartbeatInterval(-1))
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sse.StartHeartbeat(ctx)
	time.Sleep(30 * time.Millisecond)
	if strings.Contains(mw.String(), "keep-alive") {
		t.Errorf("关闭心跳后不应出现注释行: %q", mw.String())
	}
}

// TestErrorEventFormat 错误事件：data 行格式与标准错误体符合规范（KTD8）。
func TestErrorEventFormat(t *testing.T) {
	mw := &memWriter{h: make(http.Header)}
	sse, err := NewSSEWriter(mw)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if err := sse.WriteErrorEvent("boom", "server_error", "", "stream_truncated"); err != nil {
		t.Fatalf("WriteErrorEvent: %v", err)
	}
	raw := mw.String()
	if !strings.HasPrefix(raw, "data: ") {
		t.Errorf("错误事件应以 data: 开头: %q", raw)
	}
	if !strings.HasSuffix(raw, "\n\n") {
		t.Errorf("错误事件应以空行结束: %q", raw)
	}
	events := parseSSE(t, raw)
	if len(events) != 1 {
		t.Fatalf("应解析出 1 个事件，得到 %v", events)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(events[0]), &body); err != nil {
		t.Fatalf("错误事件应为 error 对象 JSON: %v（%q）", err, events[0])
	}
	if body.Error.Message != "boom" || body.Error.Type != "server_error" ||
		body.Error.Param != "" || body.Error.Code != "stream_truncated" {
		t.Errorf("错误体字段不符: %+v", body.Error)
	}
}

// TestWriteDONEFormat [DONE] 结束标志格式。
func TestWriteDONEFormat(t *testing.T) {
	mw := &memWriter{h: make(http.Header)}
	sse, err := NewSSEWriter(mw)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if err := sse.WriteDONE(); err != nil {
		t.Fatalf("WriteDONE: %v", err)
	}
	if raw := mw.String(); raw != "data: [DONE]\n\n" {
		t.Errorf("[DONE] 格式应为 data: [DONE]\\n\\n，得到 %q", raw)
	}
}

// TestFlusherAssertion Flusher 断言失败路径：不静默，返回错误。
func TestFlusherAssertion(t *testing.T) {
	rec := httptest.NewRecorder()
	if _, err := NewSSEWriter(nonFlusher{rec}); err == nil {
		t.Fatal("非 Flusher 的 ResponseWriter 应返回错误")
	}
	// 头部不应在断言失败时写出。
	if got := rec.Header().Get("Content-Type"); got != "" {
		t.Errorf("Flusher 断言失败不应写头部，得到 %q", got)
	}
}
