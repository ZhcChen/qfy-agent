package api

import (
	"encoding/json"
	"net/http"

	"github.com/qfy-agent/qfy-agent/audit"
	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/loop"
	"github.com/qfy-agent/qfy-agent/registry"
)

// HandlerConfig 组装 HTTP 处理器的全部依赖（R18：库不读取文件与环境变量，
// 一切配置由消费方注入；示例组装见 cmd/qfy-agent-server）。
type HandlerConfig struct {
	// Registry 模型注册表：驱动 /v1/models 列表与 model 存在性校验（R1/R6）。必填。
	Registry *registry.Registry
	// Runner 受控推理循环：非流式请求与 stream+tools（模拟流）路径的上游非流式
	// 调用（U5）。必填——消费方以 loop.NewRunner 构造（含工具执行器注册与审计回调）。
	Runner *loop.Runner
	// Client 后端客户端：stream=true 且无 tools 时经 backend.Stream 透传真实上游流
	// （R11）。nil 时该路径返回 500 统一错误体。
	Client *backend.Client
	// Notifier 审计通知器：流式透传路径经 ProxyStream 触发审计（KTD9/G2）；
	// 非流式路径的 CallRecord 由 Runner 内部产出。nil 时透传路径不触发审计。
	Notifier *audit.Notifier
}

// NewHandler 组装路由（标准库 net/http ServeMux，KTD1）：
//
//	GET  /v1/models            模型列表（R1）
//	POST /v1/chat/completions  对话补全（R2/R3，含流式分流与统一错误体）
//	其他路径 / 方法            404/405 OpenAI 风格错误体（R3）
//
// Registry 与 Runner 为必填依赖，缺失时 panic（fail-fast 的程序员错误）。
func NewHandler(cfg HandlerConfig) http.Handler {
	if cfg.Registry == nil {
		panic("api.HandlerConfig.Registry 不能为空")
	}
	if cfg.Runner == nil {
		panic("api.HandlerConfig.Runner 不能为空（以 loop.NewRunner 构造）")
	}
	h := &handler{cfg: cfg}
	mux := http.NewServeMux()
	// 不用方法限定 pattern，而是处理器内显式校验方法：
	// 保证方法不符时输出 OpenAI 风格 405 错误体（ServeMux 内置 405 为纯文本）。
	mux.HandleFunc("/v1/models", h.handleModels)
	mux.HandleFunc("/v1/chat/completions", h.handleChat)
	mux.HandleFunc("/", h.handleNotFound)
	return mux
}

// ServerConfig 组装完整 http.Server（示例服务与消费方直接使用）。
type ServerConfig struct {
	HandlerConfig
	// Addr 监听地址，如 "127.0.0.1:8080"。
	Addr string
}

// NewServer 组装完整 http.Server。
// 刻意不设置 WriteTimeout：SSE 长流的写超时续期由 api 层自行管理（KTD7），
// 消费方如需其它超时/限流配置可自行覆盖返回值的字段。
func NewServer(cfg ServerConfig) *http.Server {
	return &http.Server{
		Addr:    cfg.Addr,
		Handler: NewHandler(cfg.HandlerConfig),
	}
}

// handler 端点处理器：持有组装依赖。
type handler struct {
	cfg HandlerConfig
}

// handleNotFound 未知路径 → 404 OpenAI 风格错误体（R3/KTD8）。
func (h *handler) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, backend.ErrorBody{
		Message: "路径不存在: " + r.URL.Path,
		Type:    errTypeInvalidRequest,
		Code:    errCodeNotFound,
	})
}

// writeJSON 写出 JSON 响应（Content-Type: application/json）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 写出 OpenAI 风格统一错误体 {"error":{message,type,param,code}}（KTD8）。
func writeError(w http.ResponseWriter, status int, eb backend.ErrorBody) {
	writeJSON(w, status, struct {
		Error backend.ErrorBody `json:"error"`
	}{Error: eb})
}
