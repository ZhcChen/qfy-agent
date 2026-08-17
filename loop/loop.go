// Package loop 实现受控推理循环（U5）：模型调用 → tool_calls → 工具执行 →
// tool 消息回填 → 再调用（R14 轮数硬上限、R16 已注册执行器自动执行），
// 并为每次后端调用产出审计记录（R17/KTD9：CallRecord 统一由 loop 产出）。
//
// 循环语义（KTD3）：
//   - 每轮调用经 U3 策略（tooling.Strategies）构造并执行；
//   - 响应无 tool_calls 即最终响应；达到轮数上限（默认 3）停止并返回当前响应（R14）；
//   - tool_calls 全部已注册执行器时串行执行并回填 role=tool 消息（F2：panic 由库
//     recover 转为错误回填，不中断请求；per-tool timeout 默认 30s）；
//   - 任一 tool_call 未注册执行器时整轮返回标准 tool_calls（KTD3 混合场景语义：
//     不部分执行），由消费方执行后以标准 OpenAI 多轮继续；
//   - 输出校验失败（*tooling.Error，按 Kind 判定不做消息匹配）由 retry.go 回喂
//     重试（R15，最多 2 次）。
//
// 并发模型（F5）：Runner 可被并发请求共享（无共享可变状态）；工具执行器注册表
// 以 RWMutex 保护，支持运行期注册；上游调用次数按请求（ctx 携带计数器）统计。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/qfy-agent/qfy-agent/audit"
	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/registry"
	"github.com/qfy-agent/qfy-agent/tooling"
)

// 循环硬性默认值（R14/R15/F2/F3/R17）。
const (
	// DefaultMaxRounds 轮数硬上限（R14）。
	DefaultMaxRounds = 3
	// DefaultMaxValidationRetries 校验失败自动重试次数（R15，最多 2 次）。
	DefaultMaxValidationRetries = 2
	// DefaultMaxUpstreamCalls 单请求上游调用总次数上限（F3：默认 12 =
	// 最多 3 轮 × 每轮最多 4 次：1 次原生 + 1 次降级 + 首次注入 + 最多 2 次校验重试）。
	DefaultMaxUpstreamCalls = 12
	// DefaultToolTimeout per-tool 执行超时（F2）。
	DefaultToolTimeout = 30 * time.Second
	// DefaultSummaryMaxRunes 审计摘要前 N 字符（R17）。
	DefaultSummaryMaxRunes = 200
)

// Config 循环配置（硬性默认值均可通过 Option 覆盖；非正值忽略保留默认）。
type Config struct {
	// MaxRounds 轮数硬上限（R14）。
	MaxRounds int
	// MaxValidationRetries 校验失败自动重试次数（R15）。
	MaxValidationRetries int
	// MaxUpstreamCalls 单请求上游调用总次数上限（F3）。
	MaxUpstreamCalls int
	// ToolTimeout per-tool 执行超时（F2）。
	ToolTimeout time.Duration
	// SummaryMaxRunes 审计摘要前 N 字符（R17）。
	SummaryMaxRunes int
}

// DefaultConfig 返回循环默认配置。
func DefaultConfig() Config {
	return Config{
		MaxRounds:            DefaultMaxRounds,
		MaxValidationRetries: DefaultMaxValidationRetries,
		MaxUpstreamCalls:     DefaultMaxUpstreamCalls,
		ToolTimeout:          DefaultToolTimeout,
		SummaryMaxRunes:      DefaultSummaryMaxRunes,
	}
}

// Option 配置 Runner。
type Option func(*Runner)

// WithMaxRounds 覆盖轮数硬上限（R14）；非正值忽略。
func WithMaxRounds(n int) Option {
	return func(r *Runner) {
		if n > 0 {
			r.cfg.MaxRounds = n
		}
	}
}

// WithMaxValidationRetries 覆盖校验失败重试次数（R15）；负值忽略（0 表示不重试）。
func WithMaxValidationRetries(n int) Option {
	return func(r *Runner) {
		if n >= 0 {
			r.cfg.MaxValidationRetries = n
		}
	}
}

// WithMaxUpstreamCalls 覆盖单请求上游调用总次数上限（F3）；非正值忽略。
func WithMaxUpstreamCalls(n int) Option {
	return func(r *Runner) {
		if n > 0 {
			r.cfg.MaxUpstreamCalls = n
		}
	}
}

// WithToolTimeout 覆盖 per-tool 执行超时（F2）；非正值忽略。
func WithToolTimeout(d time.Duration) Option {
	return func(r *Runner) {
		if d > 0 {
			r.cfg.ToolTimeout = d
		}
	}
}

// WithSummaryMaxRunes 覆盖审计摘要前 N 字符（R17）；非正值忽略。
func WithSummaryMaxRunes(n int) Option {
	return func(r *Runner) {
		if n > 0 {
			r.cfg.SummaryMaxRunes = n
		}
	}
}

// WithOnCall 配置审计回调（R17/KTD9）：回调 panic 由 audit.Notifier recover，
// 不影响请求。
func WithOnCall(fn audit.OnCall) Option {
	return func(r *Runner) {
		r.notifier.SetOnCall(fn)
	}
}

// WithHTTPClient 注入自定义 http.Client（如超时、代理配置）。注入后其
// Transport 会被包装以统计上游调用次数（F3）；nil 忽略。
func WithHTTPClient(hc *http.Client) Option {
	return func(r *Runner) {
		if hc != nil {
			r.hc = hc
		}
	}
}

// ToolExecutor 工具执行函数（R16）：ctx 携带 per-tool 超时（到点后 Done 关闭），
// call 为标准工具调用（含 id 与参数）。返回的字符串作为 role=tool 消息的
// content 回填；返回 error 时错误文本回填（不中断请求，F2）。
type ToolExecutor func(ctx context.Context, call backend.ToolCall) (string, error)

// ToolEntry 注册的工具条目：定义（含参数 schema，供 U3 校验）与执行函数。
type ToolEntry struct {
	// Def 工具定义（对外形态，含参数 schema）。
	Def tooling.Tool
	// Exec 执行函数（nil 表示仅定义未注册执行器，KTD3）。
	Exec ToolExecutor
	// Timeout per-tool 执行超时覆盖；0 使用全局默认（30s）。
	Timeout time.Duration
}

// Tools 工具执行器注册表（F5）：以 RWMutex 保护，支持运行期注册/注销。
type Tools struct {
	mu      sync.RWMutex
	entries map[string]ToolEntry
}

// NewTools 构造空注册表。
func NewTools() *Tools {
	return &Tools{entries: map[string]ToolEntry{}}
}

// Register 注册工具定义与执行函数（R16）。name 必须与 def.Function.Name 一致
// 且非空；重复注册返回错误。注册表可运行期动态扩充（F5）。
func (t *Tools) Register(name string, def tooling.Tool, exec ToolExecutor) error {
	return t.register(name, def, exec, 0)
}

// RegisterWithTimeout 注册工具并指定 per-tool 执行超时（F2）；timeout 非正值
// 等价于 Register（使用全局默认）。
func (t *Tools) RegisterWithTimeout(name string, def tooling.Tool, exec ToolExecutor, timeout time.Duration) error {
	return t.register(name, def, exec, timeout)
}

func (t *Tools) register(name string, def tooling.Tool, exec ToolExecutor, timeout time.Duration) error {
	if name == "" || def.Function.Name != name {
		return fmt.Errorf("注册名 %q 与工具定义名 %q 不一致", name, def.Function.Name)
	}
	if exec == nil {
		return fmt.Errorf("工具 %q 执行函数不能为空", name)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.entries[name]; ok {
		return fmt.Errorf("工具 %q 已注册", name)
	}
	t.entries[name] = ToolEntry{Def: def, Exec: exec, Timeout: timeout}
	return nil
}

// Unregister 注销工具执行器（未注册时无操作）。
func (t *Tools) Unregister(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, name)
}

// Get 查询工具执行器；未注册返回 (零值, false)。
func (t *Tools) Get(name string) (ToolEntry, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[name]
	return e, ok
}

// Names 返回全部已注册工具名（顺序不确定；用于审计摘要）。
func (t *Tools) Names() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.entries))
	for name := range t.entries {
		out = append(out, name)
	}
	return out
}

// UpstreamLimitError 单请求上游调用总次数超限的稳定错误（F3）。
type UpstreamLimitError struct {
	// Limit 配置的上限。
	Limit int
	// Used 已发生的调用次数。
	Used int
}

func (e *UpstreamLimitError) Error() string {
	return fmt.Sprintf("单请求上游调用次数超过上限 %d（已发生 %d 次），已中止", e.Limit, e.Used)
}

// Runner 受控推理循环执行器（R14/R15/R16）。可被并发请求共享。
type Runner struct {
	tools      *Tools
	cfg        Config
	hc         *http.Client
	client     *backend.Client
	strategies *tooling.Strategies
	notifier   *audit.Notifier
}

// NewRunner 构造推理循环执行器。tools 为工具执行器注册表（可为 nil，此时全部
// 工具视为未注册，响应永远返回标准 tool_calls 由消费方编排，KTD3）。
// 默认使用内部 http.Client（Transport 自动包装以统计上游调用次数，F3），
// 可通过 WithHTTPClient 注入自定义 client（其 Transport 同样被包装）。
func NewRunner(tools *Tools, opts ...Option) *Runner {
	r := &Runner{
		tools:    tools,
		cfg:      DefaultConfig(),
		notifier: audit.NewNotifier(),
	}
	for _, o := range opts {
		o(r)
	}
	if r.hc == nil {
		r.hc = &http.Client{}
	}
	base := r.hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	r.hc.Transport = &countingTransport{base: base}
	r.client = backend.NewClient(backend.WithHTTPClient(r.hc))
	r.strategies = tooling.NewStrategies(r.client)
	return r
}

// Run 执行受控推理循环并返回最终响应（R14/R15/R16）。
// params 为外部 OpenAI 格式请求参数（含 messages、tools 等）；本入口为非流式，
// 流式请求由 api 层处理（CallRecord.Stream 恒为 false）。
// 不修改消费方传入的 params/messages（拷贝语义，KTD9）。
//
// 返回规则：
//   - 无 tool_calls → 直接返回该响应；
//   - 达到轮数上限（默认 3）→ 停止并返回当前响应（R14）；
//   - 任一 tool_call 未注册执行器 → 整轮返回标准 tool_calls（KTD3 混合场景）；
//   - 校验重试耗尽 → *ValidationExhaustedError（R15 稳定错误）；
//   - 上游调用总次数超限 → *UpstreamLimitError（F3）。
func (r *Runner) Run(ctx context.Context, m *registry.Model, params map[string]any) (*backend.ChatCompletion, error) {
	msgs, ok := toolingMessages(params["messages"])
	if !ok {
		return nil, fmt.Errorf("messages 应为数组，实际为 %T", params["messages"])
	}
	messages := make([]any, len(msgs))
	copy(messages, msgs)
	p := make(map[string]any, len(params)+1)
	for k, v := range params {
		p[k] = v
	}
	p["messages"] = messages

	ctx = withCallCounter(ctx, &callCounter{})
	tools := requestTools(p)
	strategy := strategyName(m, len(tools) > 0)

	var last *backend.ChatCompletion
	for round := 0; round < r.cfg.MaxRounds; round++ {
		cc, err := r.callRound(ctx, m, p, round, strategy)
		if err != nil {
			return nil, err
		}
		last = cc
		calls := responseToolCalls(cc)
		if len(calls) == 0 {
			return cc, nil // 无工具调用：最终响应。
		}
		if !r.allExecutorsRegistered(calls) {
			return cc, nil // KTD3：任一未注册 → 整轮返回标准 tool_calls，不部分执行。
		}
		if round == r.cfg.MaxRounds-1 {
			return cc, nil // R14：达到轮数硬上限，停止并返回当前响应。
		}
		// 全部已注册：回填 assistant 消息（原样含 tool_calls），随后串行执行
		// 每条 tool_call 并回填对应 role=tool 消息（R16/F2）。
		messages = append(messages, assistantMessage(cc))
		for _, tc := range calls {
			messages = append(messages, toolMessage(tc, r.executeTool(ctx, tc)))
		}
		p["messages"] = messages
	}
	return last, nil // 理论不可达（循环内必然返回）。
}

// responseToolCalls 提取主 choice（Choices[0]）的工具调用列表；无 choice 返回 nil。
func responseToolCalls(cc *backend.ChatCompletion) []backend.ToolCall {
	if cc == nil || len(cc.Choices) == 0 {
		return nil
	}
	return cc.Choices[0].Message.ToolCalls
}

// allExecutorsRegistered 判断所有 tool_calls 是否均已注册执行函数（KTD3 混合场景：
// 任一未注册 → false，整轮返回标准 tool_calls，不部分执行）。
func (r *Runner) allExecutorsRegistered(calls []backend.ToolCall) bool {
	if r.tools == nil {
		return false
	}
	for _, tc := range calls {
		entry, ok := r.tools.Get(tc.Function.Name)
		if !ok || entry.Exec == nil {
			return false
		}
	}
	return true
}

// executeTool 执行单个工具（R16/F2）：per-tool 超时（条目 Timeout 优先，否则
// 全局默认）+ panic recover。任何失败（错误/panic/超时）都转为 tool 消息内容
// 回填，不中断请求。
func (r *Runner) executeTool(ctx context.Context, tc backend.ToolCall) string {
	name := tc.Function.Name
	entry, ok := r.tools.Get(name)
	if !ok || entry.Exec == nil {
		return fmt.Sprintf("工具 %q 未注册执行器，无法执行", name)
	}
	timeout := r.cfg.ToolTimeout
	if entry.Timeout > 0 {
		timeout = entry.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := safeExec(entry.Exec, execCtx, tc)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || execCtx.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("工具 %q 执行超时（上限 %s）", name, timeout)
		}
		return fmt.Sprintf("工具 %q 执行失败: %v", name, err)
	}
	return result
}

// safeExec 以 recover 包裹执行函数调用：panic 转为 error（F2：执行函数 panic
// 不中断请求，转为错误回填）。
func safeExec(exec ToolExecutor, ctx context.Context, call backend.ToolCall) (result string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return exec(ctx, call)
}

// assistantMessage 构造回填的 assistant 消息（OpenAI 规范）：content 原样回传
// （tool_calls 消息的 content 为 null），tool_calls 原样保留（R10/KTD3）。
func assistantMessage(cc *backend.ChatCompletion) map[string]any {
	msg := cc.Choices[0].Message
	content := msg.Content
	if len(content) == 0 {
		content = json.RawMessage("null")
	}
	return map[string]any{
		"role":       "assistant",
		"content":    content,
		"tool_calls": msg.ToolCalls,
	}
}

// toolMessage 构造 role=tool 消息（OpenAI 规范）：tool_call_id 对应工具调用，
// content 为执行结果字符串。
func toolMessage(tc backend.ToolCall, content string) map[string]any {
	return map[string]any{
		"role":         "tool",
		"tool_call_id": tc.ID,
		"content":      content,
	}
}

// strategyName 返回审计策略名：无工具时 direct（直连），否则按模型能力声明
// 映射 full/partial/none。
func strategyName(m *registry.Model, hasTools bool) string {
	if !hasTools {
		return "direct"
	}
	switch m.Capabilities.ToolCalling {
	case registry.ToolCallingFull:
		return "full"
	case registry.ToolCallingPartial:
		return "partial"
	case registry.ToolCallingNone:
		return "none"
	default:
		return "unknown"
	}
}

// requestTools 解析请求声明的工具列表（审计摘要用）；解析失败返回空。
func requestTools(params map[string]any) []tooling.Tool {
	ts, err := tooling.ParseTools(params["tools"])
	if err != nil {
		return nil
	}
	return ts
}

// requestToolNames 返回请求声明的工具名列表。
func requestToolNames(params map[string]any) []string {
	ts := requestTools(params)
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Function.Name)
	}
	return names
}

// toolingMessages 把 messages 提取为 []any（兼容 []any 与任意切片形态）；
// 非切片返回 (nil, false)。
func toolingMessages(v any) ([]any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case []any:
		return t, true
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice {
			return nil, false
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	}
}

// ---- 审计（KTD9：CallRecord 统一由 loop 产出） ----

// notify 为一次后端调用产出审计记录并触发回调（recover 由 audit.Notifier 包裹）。
func (r *Runner) notify(m *registry.Model, strategy string, msgs []any, params map[string]any,
	cc *backend.ChatCompletion, err error, duration time.Duration, round int) {
	record := audit.CallRecord{
		Timestamp: time.Now(),
		Model:     m.ID,
		Strategy:  strategy,
		Input: audit.InputSummary{
			MessageCount: len(msgs),
			RoleContents: summarizeRoleContents(msgs, r.cfg.SummaryMaxRunes),
			ToolNames:    requestToolNames(params),
			Round:        round,
		},
		Output:   summarizeOutput(cc, r.cfg.SummaryMaxRunes),
		Duration: duration,
		Error:    errorText(err),
		Round:    round,
		Stream:   false,
	}
	r.notifier.Notify(record)
}

// errorText 提取稳定错误文本（成功为空）。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// summarizeRoleContents 按角色聚合 content 摘要（每个角色合并后取前 N 字符）。
func summarizeRoleContents(msgs []any, maxRunes int) map[string]string {
	out := map[string]string{}
	for _, mm := range msgs {
		m, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "unknown"
		}
		content := contentText(m["content"])
		if content == "" {
			continue
		}
		out[role] = truncateRunes(out[role]+content, maxRunes)
	}
	return out
}

// contentText 提取消息 content 的文本形态（审计摘要用）：字符串直接取；
// 其余形态（多模态数组、对象等）序列化为 JSON。
func contentText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.RawMessage:
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			return s
		}
		return string(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// summarizeOutput 构造输出摘要：content 前 N 字符 + tool_calls 概要（R17）。
func summarizeOutput(cc *backend.ChatCompletion, maxRunes int) audit.OutputSummary {
	if cc == nil || len(cc.Choices) == 0 {
		return audit.OutputSummary{}
	}
	msg := cc.Choices[0].Message
	out := audit.OutputSummary{
		Content: truncateRunes(contentText(msg.Content), maxRunes),
	}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, audit.ToolCallSummary{
			Name:      tc.Function.Name,
			Arguments: truncateRunes(string(tc.Function.Arguments), maxRunes),
		})
	}
	return out
}

// truncateRunes 截断为前 n 个 rune（审计摘要用）；n<=0 返回空串。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---- 上游调用次数上限（F3） ----

// callCounter 单请求上游调用计数器（经 ctx 传递，请求间隔离）。
type callCounter struct {
	mu sync.Mutex
	n  int
}

type counterKey struct{}

// withCallCounter 在 ctx 中携带单请求计数器。
func withCallCounter(ctx context.Context, c *callCounter) context.Context {
	return context.WithValue(ctx, counterKey{}, c)
}

// countingTransport 统计经其发出的 HTTP 往返次数（F3 兜底），计数归属到请求
// ctx 携带的 callCounter。partial 降级/校验重试内部的实际 HTTP 调用均被计入。
type countingTransport struct {
	base http.RoundTripper
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if c, ok := req.Context().Value(counterKey{}).(*callCounter); ok {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return t.base.RoundTrip(req)
}

// checkUpstreamBudget 检查单请求上游调用预算（F3）：达到上限返回稳定错误。
// 检查发生在每次策略调用之前；单次策略调用内部（如 partial 降级）的超限会在
// 下一次调用前被拦截。
func (r *Runner) checkUpstreamBudget(ctx context.Context) error {
	c, ok := ctx.Value(counterKey{}).(*callCounter)
	if !ok {
		return nil
	}
	c.mu.Lock()
	n := c.n
	c.mu.Unlock()
	if n >= r.cfg.MaxUpstreamCalls {
		return &UpstreamLimitError{Limit: r.cfg.MaxUpstreamCalls, Used: n}
	}
	return nil
}
