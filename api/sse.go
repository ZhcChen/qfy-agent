// Package api 实现对外 HTTP 网关层（U6 SSE 流式 / U7 服务）：
// SSE 事件写出、上游流透传与缓冲模拟流式，格式严格符合 OpenAI chunk 规范（R11/R12/R13）。
package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/qfy-agent/qfy-agent/backend"
)

// SSE 默认参数（KTD7）：
// 心跳间隔 15s 小于常见代理 idle 超时 60s（留 2 倍余量）；写超时续期间隔 30s
// 规避 http.Server WriteTimeout 截断长流。
const (
	DefaultHeartbeatInterval = 15 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
)

// Option 配置 SSEWriter。
type Option func(*SSEWriter)

// WithHeartbeatInterval 设置心跳间隔：正值生效，0 保留默认 15s，负值关闭心跳。
func WithHeartbeatInterval(d time.Duration) Option {
	return func(s *SSEWriter) {
		switch {
		case d < 0:
			s.heartbeat = 0
		case d > 0:
			s.heartbeat = d
		}
	}
}

// WithWriteTimeout 设置写超时续期间隔：正值生效，0 保留默认 30s，负值禁用续期。
func WithWriteTimeout(d time.Duration) Option {
	return func(s *SSEWriter) {
		switch {
		case d < 0:
			s.writeTimeout = 0
		case d > 0:
			s.writeTimeout = d
		}
	}
}

// SSEWriter 向 http.ResponseWriter 写出标准 SSE 事件流（KTD7/R13）：
// 流式头部、data 行格式、心跳注释行、标准错误事件、[DONE] 结束标志与写超时续期。
// 所有写出经互斥锁串行化，心跳 goroutine 与主写出流可安全并发。
type SSEWriter struct {
	mu           sync.Mutex
	w            http.ResponseWriter
	flusher      http.Flusher
	rc           *http.ResponseController
	heartbeat    time.Duration
	writeTimeout time.Duration

	hbActive bool
	hbStop   chan struct{}
}

// NewSSEWriter 构造 SSE 写出器并立即写出流式头部。
// w 必须实现 http.Flusher（net/http 服务端标准能力），否则返回错误——不静默降级。
func NewSSEWriter(w http.ResponseWriter, opts ...Option) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("SSE 需要 http.Flusher 支持：当前 ResponseWriter 不支持流式刷新")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	s := &SSEWriter{
		w:            w,
		flusher:      flusher,
		rc:           http.NewResponseController(w),
		heartbeat:    DefaultHeartbeatInterval,
		writeTimeout: DefaultWriteTimeout,
	}
	for _, o := range opts {
		o(s)
	}
	// 立即刷新以发出头部，客户端尽早进入流式读取。
	flusher.Flush()
	return s, nil
}

// WriteEvent 写出一个 data 事件：payload 每行加 `data: ` 前缀，空行结束，
// 随后立即 Flush（写前续期写超时，KTD7）。payload 通常为单行 JSON。
func (s *SSEWriter) WriteEvent(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(formatDataEvent(payload))
}

// WriteComment 写出注释行 `: <text>\n\n`（心跳用途），随后 Flush。
func (s *SSEWriter) WriteComment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked([]byte(": " + text + "\n\n"))
}

// WriteDONE 写出流结束标志 `data: [DONE]\n\n`。
func (s *SSEWriter) WriteDONE() error {
	return s.WriteEvent([]byte("[DONE]"))
}

// WriteErrorEvent 写出标准流式错误事件 `data: {"error":{message,type,param,code}}\n\n`
// （KTD8 统一错误体；随后由调用方决定是否以 [DONE] 收尾）。
func (s *SSEWriter) WriteErrorEvent(message, typ, param, code string) error {
	body := backend.ErrorBody{Message: message, Type: typ, Param: param, Code: code}
	return s.WriteEvent(body.JSON())
}

// StartHeartbeat 启动心跳 goroutine：按配置间隔输出 `: keep-alive` 注释行（KTD7），
// 在长任务/工具执行期间保活；ctx 取消或 Stop 时退出。间隔未启用时无操作。
func (s *SSEWriter) StartHeartbeat(ctx context.Context) {
	if s.heartbeat <= 0 {
		return
	}
	s.mu.Lock()
	if s.hbActive {
		s.mu.Unlock()
		return
	}
	s.hbActive = true
	s.hbStop = make(chan struct{})
	s.mu.Unlock()

	go s.heartbeatLoop(ctx, s.hbStop)
}

// Stop 停止心跳 goroutine（幂等；已停止或从未启动时无操作）。
func (s *SSEWriter) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hbActive {
		return
	}
	close(s.hbStop)
	s.hbActive = false
}

// heartbeatLoop 周期发出心跳注释行；写失败（客户端断开等）即退出。
func (s *SSEWriter) heartbeatLoop(ctx context.Context, stop <-chan struct{}) {
	t := time.NewTicker(s.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			if err := s.WriteComment("keep-alive"); err != nil {
				return
			}
		}
	}
}

// writeLocked 续期写超时后写出并立即 Flush；调用方须持有 mu。
func (s *SSEWriter) writeLocked(data []byte) error {
	if s.writeTimeout > 0 {
		// 续期失败（连接不支持 deadline）不阻断写出——续期是尽力而为的防护。
		_ = s.rc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	}
	if _, err := s.w.Write(data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// formatDataEvent 把 payload 格式化为 SSE data 事件：每行加 `data: ` 前缀，
// 以空行结束（单行 JSON 为常规路径）。
func formatDataEvent(payload []byte) []byte {
	var b bytes.Buffer
	for _, ln := range bytes.Split(payload, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(ln)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}
