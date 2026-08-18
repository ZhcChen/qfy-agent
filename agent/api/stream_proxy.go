package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/qfy-agent/qfy-agent/agent/audit"
	"github.com/qfy-agent/qfy-agent/agent/backend"
)

// DefaultSummaryRunes 流式输出摘要（content 与工具参数）保留的 rune 数上限
// （评审修正：统一引用 audit 包常量，与 loop 摘要单一来源）。
const DefaultSummaryRunes = audit.DefaultSummaryMaxRunes

// ProxyOptions 透传流配置（R11）。
type ProxyOptions struct {
	// Model 注册表模型 ID（审计字段）。
	Model string
	// Strategy 采用的策略（审计字段；流式透传通常为 "direct"）。
	Strategy string
	// Round 轮次（审计字段）。
	Round int
	// Input 输入摘要（审计字段，由调用方提供）。
	Input audit.InputSummary
	// OnCall 审计回调；nil 时不触发（正常结束与截断均触发，KTD9/G2）。
	OnCall audit.OnCall
	// SummaryRunes 输出摘要（content / 工具参数）保留的 rune 数；0 取默认 500。
	SummaryRunes int
	// HeartbeatInterval 心跳间隔；0 取默认 15s，负值关闭心跳。
	HeartbeatInterval time.Duration
	// WriteTimeout 写超时续期间隔；0 取默认 30s，负值禁用续期。
	WriteTimeout time.Duration
}

// ErrStreamTruncated 上游流提前结束（未收到 [DONE]）——已向客户端发出错误事件与 [DONE]。
var ErrStreamTruncated = errors.New("上游流截断（缺 [DONE]）")

// 流中错误事件的消息与错误码（KTD8 统一错误体）。
const (
	streamErrorType = "stream_error"
	streamErrorCode = "stream_truncated"

	errMsgClientDisconnected = "客户端断开连接"
	errMsgWriteFailed        = "SSE 写出失败"
)

// ProxyStream 透传上游 SSE 流（R11/KTD7）：
// bufio 逐事件读取 → KTD8 白名单读改写（object/finish_reason 规范化、未知字段丢弃、
// usage 原样保留）→ 写出并立即 Flush；心跳与写超时续期走 sse.go（KTD7）。
//
// 调用方须已确认上游 2xx 且 body 为 SSE 流（非 2xx 的错误体透传由调用方处理，不伪流式）。
// ctx 取消（客户端断开）时终止读取与写出，并以部分记录触发审计（F4）。
// 正常结束（收到 [DONE]）与截断（缺 [DONE]）均触发审计：截断时先向客户端发出标准
// 错误事件与 [DONE]，再返回包裹底层原因的 ErrStreamTruncated。
func ProxyStream(ctx context.Context, w http.ResponseWriter, upstream io.ReadCloser, opts ProxyOptions) error {
	defer upstream.Close()

	sse, err := NewSSEWriter(w, WithHeartbeatInterval(opts.HeartbeatInterval), WithWriteTimeout(opts.WriteTimeout))
	if err != nil {
		return err
	}
	defer sse.Stop()
	sse.StartHeartbeat(ctx)

	limit := opts.SummaryRunes
	if limit <= 0 {
		limit = DefaultSummaryRunes
	}
	p := &streamProxy{
		opts:    opts,
		start:   time.Now(),
		limit:   limit,
		content: runePrefix{limit: limit},
		calls:   map[int]*streamCallAccum{},
	}

	r := bufio.NewReader(upstream)
	for {
		data, rerr := readSSEEvent(r)
		if data != "" {
			if strings.TrimSpace(data) == "[DONE]" {
				p.sawDONE = true
				if p.sawReasoning && !p.sawVisible && p.sawLength {
					incomplete := &backend.IncompleteResponseError{}
					msg := stableErrorMessage(incomplete)
					if err := sse.WriteErrorEvent(msg, errTypeUpstream, "", errCodeIncomplete); err != nil {
						return p.emitAndReturn(false, errMsgWriteFailed, err)
					}
					if err := sse.WriteDONE(); err != nil {
						return p.emitAndReturn(false, errMsgWriteFailed, err)
					}
					return p.emitAndReturn(false, msg, incomplete)
				}
				if err := sse.WriteDONE(); err != nil {
					return p.emitAndReturn(false, errMsgWriteFailed, err)
				}
				continue
			}
			if err := p.forward(sse, data); err != nil {
				var malformed *backend.MalformedError
				if errors.As(err, &malformed) {
					msg := "上游流包含畸形响应"
					if writeErr := sse.WriteErrorEvent(msg, errTypeUpstream, "", "upstream_malformed_response"); writeErr != nil {
						return p.emitAndReturn(false, errMsgWriteFailed, writeErr)
					}
					if writeErr := sse.WriteDONE(); writeErr != nil {
						return p.emitAndReturn(false, errMsgWriteFailed, writeErr)
					}
					return p.emitAndReturn(false, msg, err)
				}
				return p.emitAndReturn(false, errMsgWriteFailed, err)
			}
		}
		if rerr != nil {
			switch {
			case rerr == io.EOF && p.sawDONE:
				// 正常结束：触发审计（含耗时、输出摘要）。
				return p.emitAndReturn(false, "", nil)
			case ctx.Err() != nil:
				// 客户端断开：context 取消已传播到上游请求，终止读取与写出，
				// 以部分记录触发审计（F4）。
				return p.emitAndReturn(false, errMsgClientDisconnected, ctx.Err())
			default:
				// 上游提前结束或异常中断（缺 [DONE]）：截断。
				return p.truncated(sse, rerr)
			}
		}
	}
}

// streamProxy 透传流的审计与摘要累积状态。
type streamProxy struct {
	opts    ProxyOptions
	start   time.Time
	limit   int
	content runePrefix
	calls   map[int]*streamCallAccum
	sawDONE bool
	// reasoningOnlyLength 在流中发现 reasoning_content，且尚无可见内容/工具调用，
	// 并以 length 结束。推理内容本身始终不透传。
	sawReasoning bool
	sawVisible   bool
	sawLength    bool
}

// streamCallAccum 单次工具调用的摘要累积（按 index 组织）。
type streamCallAccum struct {
	name string
	args runePrefix
}

// forward 读改写单个 chunk 事件：解析 data 的 chunk JSON → 白名单化（KTD8）→
// 重序列化写出，边转发边累积输出摘要。畸形 JSON 返回明确错误并终止流。
func (p *streamProxy) forward(sse *SSEWriter, data string) error {
	var c streamChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return &backend.MalformedError{Phase: "decode stream chunk", Err: err}
	}
	p.observeExtensions(c)
	rewriteChunk(&c, p.opts.Model)
	p.accumulate(c)
	b, err := json.Marshal(&c)
	if err != nil {
		return err
	}
	return sse.WriteEvent(b)
}

func (p *streamProxy) observeExtensions(c streamChunk) {
	for _, ch := range c.Choices {
		if ch.Delta.Content != nil && strings.TrimSpace(*ch.Delta.Content) != "" {
			p.sawVisible = true
		}
		if len(ch.Delta.ToolCalls) > 0 {
			p.sawVisible = true
		}
		if hasReasoningDelta(ch.Delta.ReasoningContent) {
			p.sawReasoning = true
		}
		if ch.FinishReason != nil {
			p.sawLength = p.sawLength || backend.NormalizeFinishReason(*ch.FinishReason) == "length"
		}
	}
}

func hasReasoningDelta(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text) != ""
	}
	return true
}

// accumulate 从白名单化后的 chunk 累积输出摘要：content 与各工具调用的 arguments
// 各保留前 limit 个 rune，工具名记录最近一次出现的名称。
func (p *streamProxy) accumulate(c streamChunk) {
	for _, ch := range c.Choices {
		if ch.Delta.Content != nil {
			p.content.add(*ch.Delta.Content)
		}
		for _, tc := range ch.Delta.ToolCalls {
			call := p.calls[tc.Index]
			if call == nil {
				call = &streamCallAccum{args: runePrefix{limit: p.limit}}
				p.calls[tc.Index] = call
			}
			if tc.Function.Name != "" {
				call.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				call.args.add(tc.Function.Arguments)
			}
		}
	}
}

// outputSummary 汇总输出摘要：content 前缀 + 按 index 排序的工具调用概要。
func (p *streamProxy) outputSummary() audit.OutputSummary {
	s := audit.OutputSummary{Content: p.content.String()}
	if len(p.calls) == 0 {
		return s
	}
	idx := make([]int, 0, len(p.calls))
	for i := range p.calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		s.ToolCalls = append(s.ToolCalls, audit.ToolCallSummary{
			Name:      p.calls[i].name,
			Arguments: p.calls[i].args.String(),
		})
	}
	return s
}

// emitAndReturn 触发审计（成功时 errMsg 为空）并返回 err。
func (p *streamProxy) emitAndReturn(truncated bool, errMsg string, err error) error {
	p.emitRecord(truncated, errMsg)
	return err
}

// emitRecord 构造流式透传审计记录并触发回调（KTD9/G2：透传流的 CallRecord 由 api 层产出）。
func (p *streamProxy) emitRecord(truncated bool, errMsg string) {
	if p.opts.OnCall == nil {
		return
	}
	rec := audit.CallRecord{
		Timestamp: p.start,
		Model:     p.opts.Model,
		Strategy:  p.opts.Strategy,
		Input:     p.opts.Input,
		Output:    p.outputSummary(),
		Duration:  time.Since(p.start),
		Error:     errMsg,
		Round:     p.opts.Round,
		Stream:    true,
		Truncated: truncated,
	}
	// 回调以 recover 包裹（评审修正：与 audit.Notifier.Notify 的 panic 安全
	// 契约一致——回调 panic 不影响流式透传本身）。
	defer func() { _ = recover() }()
	p.opts.OnCall(rec)
}

// truncated 处理上游截断：向客户端发标准错误事件与 [DONE]（不直接断连），
// 记录 Truncated=true 的审计并返回包裹底层原因的 ErrStreamTruncated。
func (p *streamProxy) truncated(sse *SSEWriter, cause error) error {
	_ = sse.WriteErrorEvent(ErrStreamTruncated.Error(), streamErrorType, "", streamErrorCode)
	_ = sse.WriteDONE()
	p.emitRecord(true, ErrStreamTruncated.Error())
	return fmt.Errorf("%w: %v", ErrStreamTruncated, cause)
}

// readSSEEvent 从 bufio.Reader 读取一个完整 SSE 事件（以空行分隔），
// 返回 data 行内容（多行以 \n 连接）。事件数据与错误可同时返回（EOF 前的最后事件）。
// 注释行与 event/id/retry 字段被忽略；`data:` 后的可选空格被剥离。
func readSSEEvent(r *bufio.Reader) (string, error) {
	var data []string
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				// 空行 = 事件结束。
				return strings.Join(data, "\n"), err
			case strings.HasPrefix(line, ":"):
				// 注释行：跳过。
			case strings.HasPrefix(line, "data:"):
				v := strings.TrimPrefix(line, "data:")
				v = strings.TrimPrefix(v, " ")
				data = append(data, v)
			default:
				// 其余字段（event/id/retry）无透传语义，忽略。
			}
		}
		if err != nil {
			return strings.Join(data, "\n"), err
		}
	}
}

// ---- KTD8 流式 chunk 白名单 ----

// streamChunk 白名单 chunk 结构：重序列化只保留规范字段，未知响应字段显式丢弃。
// usage 以 json.RawMessage 原样保留（usage chunk 形态由上游决定，白名单只保留字段本身）。
// 透传与模拟流共用本结构。
type streamChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []streamChoice  `json:"choices"`
	Usage   json.RawMessage `json:"usage,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   *string          `json:"content,omitempty"`
	ToolCalls []streamToolCall `json:"tool_calls,omitempty"`
	Refusal   *string          `json:"refusal,omitempty"`
	// ReasoningContent 仅用于识别 LM Studio reasoning-only 截断，rewriteChunk
	// 会在输出前清空，禁止向 OpenAI-compatible 下游泄漏。
	ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
}

type streamToolCall struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function streamFunction `json:"function,omitempty"`
}

type streamFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// rewriteChunk 白名单化单个 chunk：object 规范化为 chat.completion.chunk，
// finish_reason 白名单化（仅 stop|length|tool_calls|content_filter，未知归 stop，KTD8）。
// model 覆盖为注册表模型 ID（非空时），使透传流与请求/非流式路径的 model 回显一致
// （R3：响应 model 回显请求使用的对外模型 id）。
func rewriteChunk(c *streamChunk, modelID string) {
	if c.Object != "chat.completion.chunk" {
		c.Object = "chat.completion.chunk"
	}
	if modelID != "" {
		c.Model = modelID
	}
	for i := range c.Choices {
		c.Choices[i].Delta.ReasoningContent = nil
		if c.Choices[i].FinishReason != nil {
			fr := backend.NormalizeFinishReason(*c.Choices[i].FinishReason)
			c.Choices[i].FinishReason = &fr
		}
	}
}

// runePrefix 保留前 limit 个 rune 的前缀累积器（内容/工具参数摘要）。
type runePrefix struct {
	limit int
	b     strings.Builder
	n     int // 已保留 rune 数
}

// add 追加字符串，超出 limit 的尾部丢弃（内存有界）。
func (r *runePrefix) add(s string) {
	if r.n >= r.limit {
		return
	}
	rs := []rune(s)
	take := len(rs)
	if r.n+take > r.limit {
		take = r.limit - r.n
	}
	r.b.WriteString(string(rs[:take]))
	r.n += take
}

// String 返回已保留的前缀。
func (r *runePrefix) String() string { return r.b.String() }
