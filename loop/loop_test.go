package loop

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

	"github.com/qfy-agent/qfy-agent/audit"
	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/registry"
	"github.com/qfy-agent/qfy-agent/tooling"
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

// mapColumnTool 标准工具定义（参数 schema 供 U3 校验；执行函数供 R16 执行）。
func mapColumnTool() tooling.Tool {
	return tooling.Tool{
		Type: "function",
		Function: tooling.ToolFunction{
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
	}
}

// appendNoteTool 第二个工具（多 tool_calls 串行执行测试用）。
func appendNoteTool() tooling.Tool {
	return tooling.Tool{
		Type: "function",
		Function: tooling.ToolFunction{
			Name:        "append_note",
			Description: "追加备注",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"note": map[string]any{"type": "string"},
				},
				"required": []any{"note"},
			},
		},
	}
}

// baseParams 外部 OpenAI 格式请求参数（含 map_column 工具）。
func baseParams() map[string]any {
	return paramsWithTools(mapColumnTool())
}

// paramsWithTools 构造带指定工具列表的请求参数。
func paramsWithTools(tools ...tooling.Tool) map[string]any {
	raw := make([]any, 0, len(tools))
	for _, t := range tools {
		raw = append(raw, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  t.Function.Parameters,
			},
		})
	}
	return map[string]any{
		"model": "gemma-4-e4b",
		"messages": []any{
			map[string]any{"role": "user", "content": "请把列\"客户名称\"映射到标准字段"},
		},
		"tools": raw,
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

// reqMessages 返回第 i 次请求的 messages 数组。
func reqMessages(t *testing.T, rb *sequenceBackend, i int) []any {
	t.Helper()
	req := rb.req(i)
	if req == nil {
		t.Fatalf("请求 %d 不存在", i)
	}
	msgs, ok := req["messages"].([]any)
	if !ok {
		t.Fatalf("请求 %d 的 messages 不是数组: %v", i, req["messages"])
	}
	return msgs
}

// findMsg 在消息列表中查找 role 匹配且 content 含子串的消息。
func findMsg(msgs []any, role, contentSubstr string) bool {
	for _, mm := range msgs {
		m, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if m["role"] != role {
			continue
		}
		if c, ok := m["content"].(string); ok && strings.Contains(c, contentSubstr) {
			return true
		}
	}
	return false
}

// completionBody 构造 content 为指定文本的普通补全响应。
func completionBody(content string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content)
}

// toolCallsBody 构造含原生 tool_calls 的补全响应；每个名字生成一条工具调用
// （id=call_i，arguments 为 {"column":"客户名"} 的 JSON 字符串形态）。
func toolCallsBody(names ...string) string {
	enc, _ := json.Marshal(`{"column":"客户名"}`)
	parts := make([]string, 0, len(names))
	for i, name := range names {
		parts = append(parts, fmt.Sprintf(`{"id":"call_%d","type":"function","function":{"name":%q,"arguments":%s}}`, i, name, enc))
	}
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"google/gemma-4-e4b","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[%s]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, strings.Join(parts, ","))
}

// execRecorder 记录执行顺序的执行函数（验证串行执行与调用次数）。
type execRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (er *execRecorder) exec(ctx context.Context, call backend.ToolCall) (string, error) {
	er.mu.Lock()
	er.calls = append(er.calls, call.Function.Name)
	er.mu.Unlock()
	return "executed:" + call.Function.Name, nil
}

func (er *execRecorder) count() int {
	er.mu.Lock()
	defer er.mu.Unlock()
	return len(er.calls)
}

// recordSink 并发安全的审计记录收集器。
type recordSink struct {
	mu  sync.Mutex
	rec []audit.CallRecord
}

func (rs *recordSink) collect(rec audit.CallRecord) {
	rs.mu.Lock()
	rs.rec = append(rs.rec, rec)
	rs.mu.Unlock()
}

func (rs *recordSink) list() []audit.CallRecord {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.rec
}

// assertContent 断言响应 content 文本。
func assertContent(t *testing.T, resp *backend.ChatCompletion, want string) {
	t.Helper()
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatalf("响应缺失 choices: %+v", resp)
	}
	var got string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &got); err != nil {
		t.Fatalf("响应 content 不是字符串: %v（%s）", err, resp.Choices[0].Message.Content)
	}
	if got != want {
		t.Errorf("响应 content 应为 %q，得到 %q", want, got)
	}
}

// ---- 单轮无工具调用 ----

// TestRunSingleTurnNoTools：无工具调用直接返回（R14：无 tool_calls 即最终响应）。
func TestRunSingleTurnNoTools(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: completionBody("你好，有什么可以帮你")}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	sink := &recordSink{}
	r := NewRunner(nil, WithOnCall(sink.collect))

	params := map[string]any{
		"model": "gemma-4-e4b",
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
	}
	resp, err := r.Run(context.Background(), m, params)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if rb.count() != 1 {
		t.Errorf("单轮无工具应只调用一次后端，得到 %d", rb.count())
	}
	assertContent(t, resp, "你好，有什么可以帮你")
	recs := sink.list()
	if len(recs) != 1 {
		t.Fatalf("应产出 1 条审计记录，得到 %d", len(recs))
	}
	if recs[0].Strategy != "direct" {
		t.Errorf("无工具策略应为 direct，得到 %q", recs[0].Strategy)
	}
}

// ---- 工具调用 → 执行 → 回填 → 最终答案 ----

// TestRunToolRoundTrip：工具调用 → 执行 → tool 消息回填 → 模型给出最终答案（两轮完成）。
func TestRunToolRoundTrip(t *testing.T) {
	tools := NewTools()
	er := &execRecorder{}
	if err := tools.Register("map_column", mapColumnTool(), er.exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column")},
		{code: 200, body: completionBody("已映射到 standard_field")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools)

	resp, err := r.Run(context.Background(), m, baseParams())
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if rb.count() != 2 {
		t.Fatalf("两轮完成应调用 2 次后端，得到 %d", rb.count())
	}
	if er.count() != 1 {
		t.Errorf("工具应执行 1 次，得到 %d", er.count())
	}
	assertContent(t, resp, "已映射到 standard_field")

	// 第 2 轮请求消息形态（OpenAI 规范）：
	// [user, assistant(含 tool_calls、content=null), tool(tool_call_id、content=结果)]。
	msgs := reqMessages(t, rb, 1)
	if len(msgs) != 3 {
		t.Fatalf("第 2 轮请求应有 3 条消息（user/assistant/tool），得到 %d: %v", len(msgs), msgs)
	}
	assistant, ok := msgs[1].(map[string]any)
	if !ok {
		t.Fatalf("assistant 消息类型不符: %T", msgs[1])
	}
	if assistant["role"] != "assistant" {
		t.Errorf("回填消息 role 应为 assistant，得到 %v", assistant["role"])
	}
	if c, ok := assistant["content"]; !ok || c != nil {
		t.Errorf("assistant content 应为 null（tool_calls 形态），得到 %v", assistant["content"])
	}
	tcList, ok := assistant["tool_calls"].([]any)
	if !ok || len(tcList) != 1 {
		t.Fatalf("assistant 消息应原样回传 tool_calls，得到 %v", assistant["tool_calls"])
	}
	tool, ok := msgs[2].(map[string]any)
	if !ok {
		t.Fatalf("tool 消息类型不符: %T", msgs[2])
	}
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_0" {
		t.Errorf("tool 消息应含 role=tool 与 tool_call_id=call_0，得到 %v", tool)
	}
	if tool["content"] != "executed:map_column" {
		t.Errorf("tool 消息 content 应为执行结果，得到 %v", tool["content"])
	}
}

// ---- 轮数上限 ----

// TestRunMaxRounds：模型每轮都调用工具，达到轮数上限（默认 3）停止并返回（R14）。
func TestRunMaxRounds(t *testing.T) {
	tools := NewTools()
	er := &execRecorder{}
	if err := tools.Register("map_column", mapColumnTool(), er.exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: toolCallsBody("map_column")}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools)

	resp, err := r.Run(context.Background(), m, baseParams())
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if rb.count() != 3 {
		t.Errorf("达到轮数上限（3 轮）应停止，后端调用 %d 次", rb.count())
	}
	if er.count() != 2 {
		t.Errorf("前两轮执行工具，第三轮达到上限停止，应执行 2 次，得到 %d", er.count())
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Errorf("应返回第三轮含 tool_calls 的响应，得到 %+v", resp.Choices)
	}
}

// ---- 工具执行失败路径（F2） ----

// TestRunToolErrorFeedback：工具执行返回错误 → 错误作为 tool 消息回填，请求继续。
func TestRunToolErrorFeedback(t *testing.T) {
	tools := NewTools()
	exec := func(ctx context.Context, call backend.ToolCall) (string, error) {
		return "", errors.New("映射失败: 列不存在")
	}
	if err := tools.Register("map_column", mapColumnTool(), exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column")},
		{code: 200, body: completionBody("抱歉，我无法映射")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools)

	resp, err := r.Run(context.Background(), m, baseParams())
	if err != nil {
		t.Fatalf("工具执行错误不应中断请求: %v", err)
	}
	if rb.count() != 2 {
		t.Errorf("错误回填后应继续下一轮，调用 2 次，得到 %d", rb.count())
	}
	assertContent(t, resp, "抱歉，我无法映射")
	msgs := reqMessages(t, rb, 1)
	tool, ok := msgs[2].(map[string]any)
	if !ok {
		t.Fatalf("tool 消息类型不符: %T", msgs[2])
	}
	content, _ := tool["content"].(string)
	if !strings.Contains(content, "映射失败") || !strings.Contains(content, "执行失败") {
		t.Errorf("tool 消息应回填错误文本，得到 %q", content)
	}
}

// TestRunToolPanicRecovered：工具执行 panic → 转为 tool 错误消息回填，请求不中断（F2）。
func TestRunToolPanicRecovered(t *testing.T) {
	tools := NewTools()
	exec := func(ctx context.Context, call backend.ToolCall) (string, error) {
		panic("执行器爆炸")
	}
	if err := tools.Register("map_column", mapColumnTool(), exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column")},
		{code: 200, body: completionBody("已恢复")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools)

	resp, err := r.Run(context.Background(), m, baseParams())
	if err != nil {
		t.Fatalf("panic 应被 recover，请求不中断: %v", err)
	}
	if rb.count() != 2 {
		t.Errorf("panic 回填后应继续下一轮，调用 2 次，得到 %d", rb.count())
	}
	assertContent(t, resp, "已恢复")
	msgs := reqMessages(t, rb, 1)
	tool, ok := msgs[2].(map[string]any)
	if !ok {
		t.Fatalf("tool 消息类型不符: %T", msgs[2])
	}
	content, _ := tool["content"].(string)
	if !strings.Contains(content, "panic") || !strings.Contains(content, "执行器爆炸") {
		t.Errorf("tool 消息应回填 panic 错误文本，得到 %q", content)
	}
}

// TestRunToolTimeout：per-tool 超时触发（executor 检查 ctx）→ 超时错误回填（F2）。
func TestRunToolTimeout(t *testing.T) {
	tools := NewTools()
	exec := func(ctx context.Context, call backend.ToolCall) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
			return "done", nil
		}
	}
	if err := tools.Register("map_column", mapColumnTool(), exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column")},
		{code: 200, body: completionBody("超时后继续")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools, WithToolTimeout(50*time.Millisecond))

	resp, err := r.Run(context.Background(), m, baseParams())
	if err != nil {
		t.Fatalf("超时应转为回填，不中断请求: %v", err)
	}
	if rb.count() != 2 {
		t.Errorf("超时回填后应继续下一轮，调用 2 次，得到 %d", rb.count())
	}
	assertContent(t, resp, "超时后继续")
	msgs := reqMessages(t, rb, 1)
	tool, ok := msgs[2].(map[string]any)
	if !ok {
		t.Fatalf("tool 消息类型不符: %T", msgs[2])
	}
	content, _ := tool["content"].(string)
	if !strings.Contains(content, "超时") {
		t.Errorf("tool 消息应回填超时错误，得到 %q", content)
	}
}

// ---- KTD3：未注册执行器 / 混合场景 ----

// TestRunUnregisteredTool：未注册执行器的工具 → 响应含标准 tool_calls，
// 由消费方执行后以标准 OpenAI 多轮继续（R16/KTD3）。
func TestRunUnregisteredTool(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{{code: 200, body: toolCallsBody("map_column")}})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(nil) // 未注册任何执行器

	resp, err := r.Run(context.Background(), m, baseParams())
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if rb.count() != 1 {
		t.Errorf("未注册执行器应立即返回（1 次调用），得到 %d", rb.count())
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("响应应含标准 tool_calls，得到 %+v", resp.Choices)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_0" || tc.Type != "function" || tc.Function.Name != "map_column" {
		t.Errorf("tool_calls 应原样返回，得到 %+v", tc)
	}
}

// TestRunMixedRegisteredUnregistered：混合场景（部分注册）→ 整轮返回标准
// tool_calls，不部分执行（KTD3 混合场景语义）。
func TestRunMixedRegisteredUnregistered(t *testing.T) {
	tools := NewTools()
	er := &execRecorder{}
	if err := tools.Register("map_column", mapColumnTool(), er.exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column", "unregistered_tool")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools)

	resp, err := r.Run(context.Background(), m, paramsWithTools(mapColumnTool(), appendNoteTool()))
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if rb.count() != 1 {
		t.Errorf("混合场景应整轮返回（1 次调用），得到 %d", rb.count())
	}
	if er.count() != 0 {
		t.Errorf("混合场景不应部分执行（即使 map_column 已注册），得到 %d 次执行", er.count())
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("响应应含 2 条标准 tool_calls，得到 %+v", resp.Choices)
	}
}

// ---- 校验失败重试（R15） ----

// TestRunValidationRetrySucceeds：校验失败 1 次后成功（重试生效）——httptest 后端
// 第一次返回非法输出，第二次返回合法输出（R15）。
func TestRunValidationRetrySucceeds(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: completionBody("抱歉，我无法完成该任务。")},
		{code: 200, body: completionBody(`{"name": "map_column", "arguments": {"column": "客户名", "standard_field": "customer_name"}}`)},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	r := NewRunner(nil) // 未注册执行器：校验成功后返回标准 tool_calls

	resp, err := r.Run(context.Background(), m, baseParams())
	if err != nil {
		t.Fatalf("校验重试应成功: %v", err)
	}
	if rb.count() != 2 {
		t.Fatalf("校验失败 1 次后成功应调用 2 次后端，得到 %d", rb.count())
	}
	// 重试请求应包含校验错误回喂消息（含错误类型码 parse_failed，KTD6 不做消息匹配）。
	msgs := reqMessages(t, rb, 1)
	if !findMsg(msgs, "user", "parse_failed") {
		t.Errorf("重试请求应回喂校验错误（含 parse_failed），得到 %v", msgs)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("校验成功后应返回标准 tool_calls，得到 %+v", resp.Choices)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "map_column" {
		t.Errorf("tool_calls 工具名应为 map_column，得到 %+v", resp.Choices[0].Message.ToolCalls[0])
	}
}

// TestRunValidationExhausted：校验连续失败 3 次（含首次）→ 稳定错误（R15/KTD6：
// 不泄漏模型原始输出与堆栈）。
func TestRunValidationExhausted(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: completionBody("抱歉，我无法完成该任务。")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	r := NewRunner(nil)

	_, err := r.Run(context.Background(), m, baseParams())
	if err == nil {
		t.Fatal("校验连续失败应返回错误")
	}
	var ve *ValidationExhaustedError
	if !errors.As(err, &ve) {
		t.Fatalf("应为 *ValidationExhaustedError，得到 %v", err)
	}
	if rb.count() != 3 {
		t.Errorf("校验连续失败应调用 3 次（1 次初始 + 2 次重试），得到 %d", rb.count())
	}
	if !strings.Contains(err.Error(), "parse_failed") {
		t.Errorf("稳定错误应含错误类型码 parse_failed，得到 %q", err.Error())
	}
	if strings.Contains(err.Error(), "抱歉") {
		t.Error("稳定错误不应泄漏模型原始输出")
	}
}

// ---- 上游调用次数上限（F3） ----

// TestRunUpstreamLimit：单请求上游调用总次数超限 → 稳定错误（F3）。
func TestRunUpstreamLimit(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: completionBody("抱歉，我无法完成该任务。")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	r := NewRunner(nil, WithMaxUpstreamCalls(2))

	_, err := r.Run(context.Background(), m, baseParams())
	if err == nil {
		t.Fatal("上游调用超限应返回错误")
	}
	var ue *UpstreamLimitError
	if !errors.As(err, &ue) {
		t.Fatalf("应为 *UpstreamLimitError，得到 %v", err)
	}
	if rb.count() != 2 {
		t.Errorf("上限 2 时应调用 2 次后拦截，得到 %d", rb.count())
	}
	if ue.Limit != 2 {
		t.Errorf("错误应携带上限值，得到 %d", ue.Limit)
	}
}

// TestDefaultLimits：默认硬性上限配置（R14=3、R15=2、F3=12、F2=30s）。
func TestDefaultLimits(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxRounds != 3 || DefaultMaxRounds != 3 {
		t.Errorf("默认轮数上限应为 3，得到 %d", cfg.MaxRounds)
	}
	if cfg.MaxValidationRetries != 2 || DefaultMaxValidationRetries != 2 {
		t.Errorf("默认校验重试应为 2，得到 %d", cfg.MaxValidationRetries)
	}
	if cfg.MaxUpstreamCalls != 12 || DefaultMaxUpstreamCalls != 12 {
		t.Errorf("默认上游调用上限应为 12，得到 %d", cfg.MaxUpstreamCalls)
	}
	if cfg.ToolTimeout != 30*time.Second || DefaultToolTimeout != 30*time.Second {
		t.Errorf("默认 per-tool 超时应为 30s，得到 %v", cfg.ToolTimeout)
	}
	if cfg.SummaryMaxRunes != 200 {
		t.Errorf("默认摘要字符数应为 200，得到 %d", cfg.SummaryMaxRunes)
	}
}

// ---- 审计（R17/KTD9） ----

// TestRunAuditRecords：审计回调收到完整 CallRecord（时间戳、模型、策略、输入摘要、
// 输出摘要、耗时、轮次、是否流式）。
func TestRunAuditRecords(t *testing.T) {
	tools := NewTools()
	er := &execRecorder{}
	if err := tools.Register("map_column", mapColumnTool(), er.exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column")},
		{code: 200, body: completionBody("最终答案")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	sink := &recordSink{}
	r := NewRunner(tools, WithOnCall(sink.collect))

	if _, err := r.Run(context.Background(), m, baseParams()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	recs := sink.list()
	if len(recs) != 2 {
		t.Fatalf("每次后端调用应产出 1 条审计记录，共 2 条，得到 %d", len(recs))
	}
	r0 := recs[0]
	if r0.Timestamp.IsZero() {
		t.Error("记录应含时间戳")
	}
	if r0.Model != "gemma-4-e4b" {
		t.Errorf("记录模型应为 gemma-4-e4b，得到 %q", r0.Model)
	}
	if r0.Strategy != "full" {
		t.Errorf("记录策略应为 full，得到 %q", r0.Strategy)
	}
	if r0.Round != 0 || recs[1].Round != 1 {
		t.Errorf("记录轮次应为 0/1，得到 %d/%d", r0.Round, recs[1].Round)
	}
	if r0.Duration <= 0 || recs[1].Duration <= 0 {
		t.Errorf("记录应含耗时，得到 %v/%v", r0.Duration, recs[1].Duration)
	}
	if r0.Error != "" {
		t.Errorf("成功调用错误字段应为空，得到 %q", r0.Error)
	}
	if r0.Stream {
		t.Error("非流式调用 Stream 应为 false")
	}
	// 输入摘要：消息数、各角色 content、工具列表、轮次。
	if r0.Input.MessageCount != 1 {
		t.Errorf("输入摘要消息数应为 1，得到 %d", r0.Input.MessageCount)
	}
	if len(r0.Input.RoleContents) != 1 || !strings.Contains(r0.Input.RoleContents["user"], "客户名称") {
		t.Errorf("输入摘要应含 user content 概要，得到 %v", r0.Input.RoleContents)
	}
	if len(r0.Input.ToolNames) != 1 || r0.Input.ToolNames[0] != "map_column" {
		t.Errorf("输入摘要工具列表应为 [map_column]，得到 %v", r0.Input.ToolNames)
	}
	// 输出摘要：content 或 tool_calls 概要。
	if len(r0.Output.ToolCalls) != 1 || r0.Output.ToolCalls[0].Name != "map_column" {
		t.Errorf("输出摘要应含 tool_calls 概要，得到 %+v", r0.Output.ToolCalls)
	}
	if recs[1].Output.Content != "最终答案" {
		t.Errorf("末轮输出摘要 content 应为最终答案，得到 %q", recs[1].Output.Content)
	}
}

// TestRunAuditErrorRecord：调用失败时审计记录携带错误字段（稳定错误文本）。
func TestRunAuditErrorRecord(t *testing.T) {
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: completionBody("抱歉，我无法完成该任务。")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingNone)
	sink := &recordSink{}
	r := NewRunner(nil, WithOnCall(sink.collect))

	if _, err := r.Run(context.Background(), m, baseParams()); err == nil {
		t.Fatal("应返回校验耗尽错误")
	}
	recs := sink.list()
	if len(recs) != 3 {
		t.Fatalf("校验重试 3 次调用应产出 3 条记录，得到 %d", len(recs))
	}
	for i, rec := range recs {
		if rec.Error == "" {
			t.Errorf("记录 %d 应携带错误字段", i)
		}
		if !strings.Contains(rec.Error, "parse_failed") {
			t.Errorf("记录 %d 错误应含类型码 parse_failed，得到 %q", i, rec.Error)
		}
		if rec.Round != 0 {
			t.Errorf("记录 %d 轮次应为 0，得到 %d", i, rec.Round)
		}
	}
}

// TestRunAuditCallbackPanic：回调 panic 不影响请求（recover 验证，KTD9）。
func TestRunAuditCallbackPanic(t *testing.T) {
	tools := NewTools()
	if err := tools.Register("map_column", mapColumnTool(), erFunc()); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column")},
		{code: 200, body: completionBody("最终答案")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools, WithOnCall(func(rec audit.CallRecord) {
		panic("审计回调爆炸")
	}))

	resp, err := r.Run(context.Background(), m, baseParams())
	if err != nil {
		t.Fatalf("回调 panic 不应影响请求: %v", err)
	}
	if rb.count() != 2 {
		t.Errorf("回调 panic 后循环应继续，调用 2 次，得到 %d", rb.count())
	}
	assertContent(t, resp, "最终答案")
}

// ---- 多 tool_calls 串行执行 ----

// TestRunMultiToolCallsSerial：一个响应多个 tool_calls 按序串行执行，
// 每条对应一条 role=tool 消息（R16）。
func TestRunMultiToolCallsSerial(t *testing.T) {
	tools := NewTools()
	er := &execRecorder{}
	if err := tools.Register("map_column", mapColumnTool(), er.exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	if err := tools.Register("append_note", appendNoteTool(), er.exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column", "append_note")},
		{code: 200, body: completionBody("完成")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools)

	resp, err := r.Run(context.Background(), m, paramsWithTools(mapColumnTool(), appendNoteTool()))
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	assertContent(t, resp, "完成")
	// 串行执行：按 tool_calls 顺序逐个执行。
	if got := er.calls; len(got) != 2 || got[0] != "map_column" || got[1] != "append_note" {
		t.Errorf("工具应按序串行执行 [map_column append_note]，得到 %v", got)
	}
	// 回填形态：1 条 assistant + 2 条 tool 消息，每条 tool 消息对应一条 tool_call。
	msgs := reqMessages(t, rb, 1)
	if len(msgs) != 4 {
		t.Fatalf("第 2 轮请求应有 4 条消息（user/assistant/tool/tool），得到 %d: %v", len(msgs), msgs)
	}
	assistant, ok := msgs[1].(map[string]any)
	if !ok || assistant["role"] != "assistant" {
		t.Fatalf("messages[1] 应为 assistant 消息，得到 %v", msgs[1])
	}
	tcList, ok := assistant["tool_calls"].([]any)
	if !ok || len(tcList) != 2 {
		t.Fatalf("assistant 消息应回传 2 条 tool_calls，得到 %v", assistant["tool_calls"])
	}
	for i, idx := range []int{2, 3} {
		tool, ok := msgs[idx].(map[string]any)
		if !ok || tool["role"] != "tool" {
			t.Fatalf("messages[%d] 应为 tool 消息，得到 %v", idx, msgs[idx])
		}
		wantID := fmt.Sprintf("call_%d", i)
		if tool["tool_call_id"] != wantID {
			t.Errorf("tool 消息 %d 的 tool_call_id 应为 %s，得到 %v", i, wantID, tool["tool_call_id"])
		}
	}
}

// ---- 消费方输入不被修改 ----

// TestRunDoesNotMutateInput：Run 不修改消费方传入的 params/messages（拷贝语义，KTD9）。
func TestRunDoesNotMutateInput(t *testing.T) {
	tools := NewTools()
	er := &execRecorder{}
	if err := tools.Register("map_column", mapColumnTool(), er.exec); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	rb := newSequenceBackend(t, []cannedResponse{
		{code: 200, body: toolCallsBody("map_column")},
		{code: 200, body: completionBody("最终答案")},
	})
	m := testModel(t, rb.srv.URL, registry.ToolCallingFull)
	r := NewRunner(tools)

	params := baseParams()
	origMsgs := params["messages"].([]any)
	if _, err := r.Run(context.Background(), m, params); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	msgs, ok := params["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Errorf("Run 不应修改消费方传入的 messages，得到 %v", params["messages"])
	}
	if !reflect.DeepEqual(msgs[0], origMsgs[0]) {
		t.Error("messages 内容被修改")
	}
	if _, ok := params["tools"]; !ok {
		t.Error("Run 不应删除消费方传入的 tools")
	}
}

// ---- 工具注册表 ----

// TestToolsRegister：注册表校验（注册名与定义一致、重复注册报错、Get/Unregister）。
func TestToolsRegister(t *testing.T) {
	tools := NewTools()
	exec := func(ctx context.Context, call backend.ToolCall) (string, error) { return "ok", nil }

	if err := tools.Register("map_column", mapColumnTool(), exec); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}
	if err := tools.Register("map_column", mapColumnTool(), exec); err == nil {
		t.Error("重复注册应报错")
	}
	if err := tools.Register("wrong_name", mapColumnTool(), exec); err == nil {
		t.Error("注册名与定义名不一致应报错")
	}
	if err := tools.Register("map_column", mapColumnTool(), nil); err == nil {
		t.Error("执行函数为空应报错")
	}
	entry, ok := tools.Get("map_column")
	if !ok || entry.Exec == nil || entry.Def.Function.Name != "map_column" {
		t.Errorf("Get 应返回注册条目，得到 %+v, %v", entry, ok)
	}
	if _, ok := tools.Get("no_such"); ok {
		t.Error("Get 未注册工具应返回 false")
	}
	tools.Unregister("map_column")
	if _, ok := tools.Get("map_column"); ok {
		t.Error("Unregister 后 Get 应返回 false")
	}
}

// erFunc 返回一个普通执行函数（审计回调 panic 测试用）。
func erFunc() ToolExecutor {
	return func(ctx context.Context, call backend.ToolCall) (string, error) {
		return "executed:" + call.Function.Name, nil
	}
}
