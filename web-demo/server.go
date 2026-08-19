package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/ZhcChen/qfy-agent/agent/api"
	"github.com/ZhcChen/qfy-agent/agent/audit"
	"github.com/ZhcChen/qfy-agent/agent/backend"
	"github.com/ZhcChen/qfy-agent/agent/loop"
	"github.com/ZhcChen/qfy-agent/agent/registry"
	"github.com/ZhcChen/qfy-agent/agent/tooling"
)

//go:embed static
var staticFS embed.FS

// server web-demo 的 HTTP 处理器：路由 + 各 API 端点（前后端分离：
// 页面由 Go 渲染静态资源，数据经 JSON/SSE 接口交互）。
type server struct {
	reg    *registry.Registry
	client *backend.Client
	runner *loop.Runner
	tools  *loop.Tools
	audits *auditStore
	notify audit.OnCall
}

// routes 组装路由（Go 1.22+ ServeMux 方法路由）。
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	// 静态资源（go:embed，局域网不依赖 CDN）。
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	mux.Handle("GET /static/", noStore(staticHandler))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	// API。
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("POST /api/chat/stream", s.handleChatStream)
	mux.HandleFunc("GET /api/audit", s.handleAudit)
	mux.HandleFunc("GET /api/tools", s.handleTools)
	return logRequests(mux)
}

// logRequests 请求日志中间件（演示形态）。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "页面缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// ---- API：模型 ----

type modelInfo struct {
	ID            string `json:"id"`
	BackendModel  string `json:"backend_model"`
	ContextWindow int    `json:"context_window"`
	ToolCalling   string `json:"tool_calling"`
	JSONMode      bool   `json:"json_mode"`
	Streaming     bool   `json:"streaming"`
}

// handleModels GET /api/models：注册表模型列表（含能力声明，供前端展示）。
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	ms := s.reg.List()
	out := make([]modelInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, modelInfo{
			ID:            m.ID,
			BackendModel:  m.BackendModelID(),
			ContextWindow: m.ContextWindow,
			ToolCalling:   string(m.Capabilities.ToolCalling),
			JSONMode:      m.Capabilities.JSONMode,
			Streaming:     m.Capabilities.Streaming,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// ---- API：对话 ----

type chatRequest struct {
	Model    string `json:"model"`
	Messages []any  `json:"messages"`
	// Tools 可选：演示模式下前端可带 map_column 工具定义；
	// 不带时后端注入 map_column 定义（演示"消费方声明工具"两种形态）。
	Tools []any `json:"tools,omitempty"`
	// UseTool 是否启用工具调用演示（默认 true）。
	UseTool *bool `json:"use_tool,omitempty"`
	// Thinking 是否使用模型默认推理策略；false 转为 OpenAI 风格 reasoning_effort=none。
	Thinking *bool `json:"thinking,omitempty"`
}

// handleChat POST /api/chat：非流式对话（经网关受控推理循环）。
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChat(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	m, err := s.reg.Get(req.Model)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	params := buildParams(req)
	resp, err := s.runner.Run(r.Context(), m, params)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, stableError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content":       messageContent(resp),
		"tool_calls":    resp.Choices[0].Message.ToolCalls,
		"finish_reason": resp.Choices[0].FinishReason,
		"model":         req.Model,
	})
}

// handleChatStream POST /api/chat/stream：SSE 流式对话。
// 前端用 fetch + ReadableStream 解析（POST 语义，EventSource 不支持）。
// 带工具时由网关内循环自动执行并模拟为 SSE 流输出（G1 分流）。
func (s *server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChat(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	m, err := s.reg.Get(req.Model)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	params := buildParams(req)
	params["stream"] = true

	// 复用 api 层 SSE 能力：透传模型支持流式且无工具 → 真实透传；
	// 否则（工具演示/非流式后端）→ 循环 + 模拟流。
	if m.Capabilities.Streaming && !hasTools(params) {
		upstream, err := s.client.Stream(r.Context(), m, params)
		if err != nil {
			writeErr(w, http.StatusBadGateway, stableError(err))
			return
		}
		err = api.ProxyStream(r.Context(), w, upstream, api.ProxyOptions{
			Model:    m.ID,
			Strategy: "direct",
			OnCall:   s.notify,
			Input:    audit.InputSummary{MessageCount: len(req.Messages)},
		})
		if err != nil && r.Context().Err() == nil {
			log.Printf("透传流结束: %v", err)
		}
		return
	}

	// 循环 + 模拟流（工具演示或非流式后端）。
	sse, err := api.NewSSEWriter(w)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sse.StartHeartbeat(r.Context())
	defer sse.Stop()
	p := dropStream(params)
	resp, err := s.runner.Run(r.Context(), m, p)
	if err != nil {
		_ = sse.WriteErrorEvent(stableError(err), "server_error", "", "internal_error")
		_ = sse.WriteDONE()
		return
	}
	resp.Model = m.ID
	_ = api.SimulateStream(r.Context(), w, resp, api.SimulateOptions{ChunkSize: 2})
}

// ---- API：工具与审计 ----

type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Registered  bool   `json:"registered"`
}

// handleTools GET /api/tools：演示工具列表（前端展示可用的工具调用演示）。
func (s *server) handleTools(w http.ResponseWriter, r *http.Request) {
	out := []toolInfo{{
		Name:        "map_column",
		Description: "把非标准 Excel 列名映射到标准字段（模拟已确认映射查询）",
		Registered:  true,
	}}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

// auditView 审计记录的对外 JSON 形态（小写字段，前端消费；R17 演示）。
type auditView struct {
	Timestamp   time.Time          `json:"timestamp"`
	Model       string             `json:"model"`
	Strategy    string             `json:"strategy"`
	Round       int                `json:"round"`
	Stream      bool               `json:"stream"`
	Truncated   bool               `json:"truncated"`
	DurationMS  int64              `json:"duration_ms"`
	Error       string             `json:"error"`
	MessageCnt  int                `json:"messages"`
	ToolNames   []string           `json:"tools"`
	Output      string             `json:"output"`
	ToolResults []audit.ToolResult `json:"tool_results,omitempty"`
}

// handleAudit GET /api/audit：审计留痕列表（内存落库模拟，R17 演示）。
func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	recs := s.audits.List()
	out := make([]auditView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, auditView{
			Timestamp:   rec.Timestamp,
			Model:       rec.Model,
			Strategy:    rec.Strategy,
			Round:       rec.Round,
			Stream:      rec.Stream,
			Truncated:   rec.Truncated,
			DurationMS:  rec.Duration.Milliseconds(),
			Error:       rec.Error,
			MessageCnt:  rec.Input.MessageCount,
			ToolNames:   rec.Input.ToolNames,
			Output:      rec.Output.Content,
			ToolResults: rec.ToolResults,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out})
}

// ---- 请求构造与工具演示 ----

// decodeChat 解析对话请求体。
func decodeChat(r *http.Request) (*chatRequest, error) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		return nil, fmt.Errorf("请求体解析失败: %v", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("缺少 model")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages 不能为空")
	}
	return &req, nil
}

// buildParams 组装网关请求参数：默认启用工具演示（前端未带 tools 时注入
// map_column 定义），模型能力为 none 时由网关注入策略自动处理。
func buildParams(req *chatRequest) map[string]any {
	params := map[string]any{
		"model":    req.Model,
		"messages": req.Messages,
	}
	useTool := true
	if req.UseTool != nil {
		useTool = *req.UseTool
	}
	if useTool {
		params["tools"] = []any{mapColumnTool()}
	} else if len(req.Tools) > 0 {
		params["tools"] = req.Tools
	}
	if req.Thinking != nil && !*req.Thinking {
		params["reasoning_effort"] = "none"
	}
	return params
}

func hasTools(params map[string]any) bool {
	tools, _ := params["tools"].([]any)
	return len(tools) > 0
}

// dropStream 复制参数并剥离仅适用于流式请求的字段。
func dropStream(params map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for k, v := range params {
		if k == "stream" || k == "stream_options" {
			continue
		}
		out[k] = v
	}
	return out
}

// mapColumnTool map_column 工具定义（与 config 无关，web-demo 自带演示工具；
// 供工具注册与请求 tools 声明共用）。
func mapColumnTool() tooling.Tool {
	return tooling.Tool{
		Type: "function",
		Function: tooling.ToolFunction{
			Name:        "map_column",
			Description: "把非标准 Excel 列名映射到标准字段",
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

// knownMappings 模拟"已确认映射"记忆（一期硬编码；二期接 nomic-embed 向量检索）。
var knownMappings = map[string]string{
	"客户名称": "customer_name",
	"客户名":  "customer_name",
	"金额":   "amount",
	"数量":   "quantity",
	"订单日期": "order_date",
	"日期":   "order_date",
}

// mapColumnExecutor map_column 的执行函数：查询模拟映射表并返回结果文本
// （作为 role=tool 消息回填，网关循环继续直到模型给出最终答案，R16 演示）。
//
// 库契约：ToolCall.Function.Arguments 是内容为合法 JSON 的字符串（R10），
// 消费方执行器须先解包字符串再解析参数对象（此处示范标准写法）。
func mapColumnExecutor(ctx context.Context, call backend.ToolCall) (string, error) {
	var raw string
	if err := json.Unmarshal(call.Function.Arguments, &raw); err != nil {
		return "", fmt.Errorf("arguments 解包失败: %v", err)
	}
	var args struct {
		Column        string `json:"column"`
		StandardField string `json:"standard_field"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", fmt.Errorf("参数解析失败: %v", err)
	}
	// 模型可能给出候选标准字段，与已确认映射对比。
	if mapped, ok := knownMappings[args.Column]; ok {
		if args.StandardField == "" || args.StandardField == mapped {
			return fmt.Sprintf("已确认映射：列「%s」→ 标准字段 %s", args.Column, mapped), nil
		}
		return fmt.Sprintf("列「%s」的已确认映射为 %s（候选 %s 不一致，请以已确认映射为准）",
			args.Column, mapped, args.StandardField), nil
	}
	return fmt.Sprintf("未找到列「%s」的已确认映射，建议人工确认后补充映射表", args.Column), nil
}

// ---- 辅助 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// stableError 提取稳定错误文本（不泄漏原始堆栈）。
func stableError(err error) string {
	return err.Error()
}

// messageContent 提取主 choice 的文本内容。
func messageContent(resp *backend.ChatCompletion) string {
	if resp == nil || len(resp.Choices) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &s); err != nil {
		return ""
	}
	return s
}
