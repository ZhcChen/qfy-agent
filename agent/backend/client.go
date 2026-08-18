// Package backend 实现 OpenAI 兼容后端适配层（U2）：HTTP 客户端、请求/响应归一化、
// 参数抹平与 typed error taxonomy（KTD9）。上层（tooling/api）依据本层可识别错误
// 类型做策略决策（如 partial 降级，KTD5），不解析错误消息文本。
package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/qfy-agent/qfy-agent/registry"
)

// 默认超时：非流式调用使用常规 RequestTimeout；流式读取路径独立放宽
// 为更长的 StreamTimeout（R13 / 评审修正 G3）。
const (
	DefaultRequestTimeout = 30 * time.Second
	DefaultStreamTimeout  = 5 * time.Minute
	// DefaultMaxResponseBytes 非流式响应体大小上限（8 MiB），防止异常/恶意上游
	// 以超大响应耗尽网关内存（评审修正：资源边界）。
	DefaultMaxResponseBytes = 8 << 20
)

// UnavailableError 表示后端不可用（KTD9 分类一）：网络错误或超时。
// Timeout 区分超时与一般网络错误；上层用 errors.As 识别并据此触发降级（KTD5）。
type UnavailableError struct {
	Op      string
	Err     error
	Timeout bool
}

func (e *UnavailableError) Error() string {
	kind := "网络错误"
	if e.Timeout {
		kind = "超时"
	}
	if e.Err != nil {
		return fmt.Sprintf("后端不可用（%s）%s: %v", kind, e.Op, e.Err)
	}
	return fmt.Sprintf("后端不可用（%s）%s", kind, e.Op)
}

// Unwrap 暴露底层错误，支持 errors.Is/As 链。
func (e *UnavailableError) Unwrap() error { return e.Err }

// UpstreamError 表示上游返回非 2xx（KTD9 分类二），
// 携带状态码与规范化错误体（KTD8：{"error":{message,type,param,code}}）。
type UpstreamError struct {
	StatusCode int
	Body       *ErrorBody
}

func (e *UpstreamError) Error() string {
	msg := ""
	if e.Body != nil {
		msg = e.Body.Message
	}
	return fmt.Sprintf("上游返回 HTTP %d: %s", e.StatusCode, msg)
}

// MalformedError 表示响应畸形（KTD9 分类三）：JSON 解析失败或结构非法。
type MalformedError struct {
	Phase string
	Err   error
}

func (e *MalformedError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("响应畸形（%s）: %v", e.Phase, e.Err)
	}
	return fmt.Sprintf("响应畸形（%s）", e.Phase)
}

// Unwrap 暴露底层错误。
func (e *MalformedError) Unwrap() error { return e.Err }

// IncompleteResponseError 表示上游只产生了非标准推理内容，因长度上限结束，
// 但没有生成任何对外可见的 content 或 tool_calls。推理原文不会写入错误，
// 避免把模型内部推理泄漏给调用方或日志。
type IncompleteResponseError struct{}

func (e *IncompleteResponseError) Error() string {
	return "上游模型输出长度已耗尽，但未生成可见内容"
}

// Option 配置 Client。
type Option func(*Client)

// WithTimeouts 覆盖非流式与流式调用超时；非正值忽略（保留默认值）。
func WithTimeouts(request, stream time.Duration) Option {
	return func(c *Client) {
		if request > 0 {
			c.RequestTimeout = request
		}
		if stream > 0 {
			c.StreamTimeout = stream
		}
	}
}

// WithHTTPClient 注入自定义 http.Client（如测试 transport、代理配置）。
// 注入后两个调用路径共用该 client，实际超时以注入值为准。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.injected = hc }
}

// Client 是 OpenAI 兼容后端的 HTTP 客户端。
// 非流式调用（Call）使用 RequestTimeout；流式调用（Stream）使用独立放宽的
// StreamTimeout（G3：流式读取路径单独放宽读超时）。
type Client struct {
	// RequestTimeout 非流式调用超时（含响应体读取）。
	RequestTimeout time.Duration
	// StreamTimeout 流式读取路径超时（独立于 RequestTimeout）。
	StreamTimeout time.Duration
	// MaxResponseBytes 非流式响应体大小上限；0 取默认 8 MiB。
	MaxResponseBytes int64

	injected      *http.Client
	requestClient *http.Client
	streamClient  *http.Client
}

// NewClient 构造后端客户端；默认超时 RequestTimeout=30s、StreamTimeout=5m，
// 可通过 Option 覆盖。注入的 http.Client 若未设置 Timeout，仍应用本客户端
// 的默认超时（双保险，评审修正：注入路径不得绕过超时契约）。
func NewClient(opts ...Option) *Client {
	c := &Client{RequestTimeout: DefaultRequestTimeout, StreamTimeout: DefaultStreamTimeout, MaxResponseBytes: DefaultMaxResponseBytes}
	for _, o := range opts {
		o(c)
	}
	c.requestClient = &http.Client{Timeout: c.RequestTimeout}
	c.streamClient = &http.Client{Timeout: c.StreamTimeout}
	if c.injected != nil {
		req := *c.injected
		if req.Timeout == 0 {
			req.Timeout = c.RequestTimeout
		}
		stream := *c.injected
		if stream.Timeout == 0 {
			stream.Timeout = c.StreamTimeout
		}
		c.requestClient = &req
		c.streamClient = &stream
	}
	return c
}

// Call 发起非流式 /chat/completions 调用并返回归一化响应。
// params 为外部 OpenAI 格式请求参数（model 字段为注册表 ID，内部翻译为后端
// model id 并合并 default_params 后发出，R2/R5）。错误均为本包可识别类型：
// *UnavailableError（网络/超时）、*UpstreamError（非 2xx）、*MalformedError（响应畸形）。
func (c *Client) Call(ctx context.Context, m *registry.Model, params map[string]any) (*ChatCompletion, error) {
	resp, err := c.roundTrip(ctx, m, params, c.requestClient, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limit := c.MaxResponseBytes
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &UnavailableError{Op: "读取响应体", Err: err, Timeout: isTimeout(err)}
	}
	if int64(len(body)) > limit {
		return nil, &MalformedError{Phase: "response too large", Err: fmt.Errorf("响应体超过上限 %d 字节", limit)}
	}
	return NormalizeResponse(body)
}

// Stream 发起流式 /chat/completions 调用（强制 stream=true），
// 返回上游响应体（io.ReadCloser）供上层逐 SSE 事件读取（R11）。
// 调用方负责关闭返回的 body；请求携带 ctx，客户端断开时经 context 取消上游请求。
func (c *Client) Stream(ctx context.Context, m *registry.Model, params map[string]any) (io.ReadCloser, error) {
	// 浅拷贝参数表，不修改调用方输入；流式标记按入口强制为 true。
	p := make(map[string]any, len(params)+1)
	for k, v := range params {
		p[k] = v
	}
	p["stream"] = true
	resp, err := c.roundTrip(ctx, m, p, c.streamClient, true)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// roundTrip 构造并发送上游请求；2xx 返回响应（流式时 body 交由上层逐事件读取），
// 非 2xx 解析为统一错误体并返回 *UpstreamError，网络/超时返回 *UnavailableError。
func (c *Client) roundTrip(ctx context.Context, m *registry.Model, params map[string]any, hc *http.Client, stream bool) (*http.Response, error) {
	payload, err := BuildRequest(m, params)
	if err != nil {
		return nil, &MalformedError{Phase: "encode request", Err: err}
	}
	url := strings.TrimRight(m.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, &UnavailableError{Op: "构造请求", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if m.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &UnavailableError{Op: "发送请求", Err: err, Timeout: isTimeout(err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: NormalizeErrorBody(resp.StatusCode, raw)}
	}
	return resp, nil
}

// isTimeout 判断错误是否为超时（context deadline 或 net.Error.Timeout）。
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}
