package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qfy-agent/qfy-agent/agent/audit"
	"github.com/qfy-agent/qfy-agent/agent/backend"
	"github.com/qfy-agent/qfy-agent/agent/loop"
	"github.com/qfy-agent/qfy-agent/agent/registry"
	"github.com/qfy-agent/qfy-agent/agent/tooling"
)

// maxRequestBody 请求体大小上限（1 MiB，防止畸形大请求占满内存）。
const maxRequestBody = 1 << 20

// OpenAI 风格错误体常量（KTD8：type/code 稳定，供消费方程序化处理）。
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeServer         = "server_error"
	errTypeUpstream       = "upstream_error"

	errCodeMethodNotAllowed = "method_not_allowed"
	errCodeNotFound         = "not_found"
	errCodeInvalidJSON      = "invalid_json"
	errCodeMissingModel     = "missing_model"
	errCodeModelNotFound    = "model_not_found"
	errCodeMissingMessages  = "missing_messages"
	errCodeInvalidTools     = "invalid_tools"
	errCodeInternal         = "internal_error"
	errCodeValidationFailed = "validation_failed"
	errCodeUpstreamLimit    = "upstream_call_limit"
	errCodeToolingFailed    = "tooling_failed"
	errCodeIncomplete       = "upstream_incomplete_response"
)

// handleChat POST /v1/chat/completions（R2/R3）：
// 请求解析 → model/messages/tools 校验 → stream 分流（G1 评审修正）：
//
//   - stream 非 true → loop.Run（非流式）→ 标准响应骨架 JSON；
//   - stream=true 且请求含 tools → loop.Run（上游非流式调用）→ SimulateStream
//     模拟 SSE 流（G1：带 tools 的 stream 先走 loop 再模拟流式；include_usage 时
//     模拟流发 usage chunk）；
//   - stream=true 且无 tools → backend.Stream（上游真实流）→ ProxyStream 透传
//     （R11；backend.Stream 已保证 2xx，非 2xx 返回 UpstreamError → 统一错误体）。
//
// 校验失败与内部错误统一输出 OpenAI 风格错误体（R3/KTD8）。
func (h *handler) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, backend.ErrorBody{
			Message: "方法不允许，期望 POST",
			Type:    errTypeInvalidRequest,
			Code:    errCodeMethodNotAllowed,
		})
		return
	}

	params, err := decodeChatBody(r)
	if err != nil {
		msg := "请求体不是合法 JSON 对象: " + err.Error()
		if errors.Is(err, io.EOF) {
			msg = "请求体为空"
		}
		if errors.Is(err, ErrBodyTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, backend.ErrorBody{
				Message: "请求体超过大小上限（1 MiB）",
				Type:    errTypeInvalidRequest,
				Code:    "body_too_large",
			})
			return
		}
		writeError(w, http.StatusBadRequest, backend.ErrorBody{
			Message: msg,
			Type:    errTypeInvalidRequest,
			Code:    errCodeInvalidJSON,
		})
		return
	}

	// model 校验：必填（400）且必须在注册表中（404，"模型不存在"语义）。
	modelVal, hasModel := params["model"]
	modelID, modelOK := modelVal.(string)
	if !hasModel || !modelOK {
		msg := "缺少 model 字段"
		if hasModel {
			msg = "model 字段必须为字符串"
		}
		writeError(w, http.StatusBadRequest, backend.ErrorBody{
			Message: msg,
			Type:    errTypeInvalidRequest,
			Code:    errCodeMissingModel,
		})
		return
	}
	m, err := h.cfg.Registry.Get(modelID)
	if err != nil {
		writeError(w, http.StatusNotFound, backend.ErrorBody{
			Message: fmt.Sprintf("模型 %q 不存在", modelID),
			Type:    errTypeInvalidRequest,
			Code:    errCodeModelNotFound,
		})
		return
	}

	// response_format 准入：模型声明 json_mode=false 时拒绝 json_object 请求
	// （评审修正：能力字段消费路径——不静默剥离）。
	if rf, ok := params["response_format"].(map[string]any); ok && rf["type"] == "json_object" && !m.Capabilities.JSONMode {
		writeError(w, http.StatusBadRequest, backend.ErrorBody{
			Message: fmt.Sprintf("模型 %q 不支持 JSON mode（response_format.json_object）", modelID),
			Type:    errTypeInvalidRequest,
			Code:    "json_mode_not_supported",
		})
		return
	}

	// messages 校验：必须为非空数组。
	msgs, ok := params["messages"].([]any)
	if !ok || len(msgs) == 0 {
		writeError(w, http.StatusBadRequest, backend.ErrorBody{
			Message: "messages 必须为非空数组",
			Type:    errTypeInvalidRequest,
			Code:    errCodeMissingMessages,
		})
		return
	}

	// tools 结构校验（可选字段；解析失败 → 400）。
	tools, toolsErr := tooling.ParseTools(params["tools"])
	if toolsErr != nil {
		writeError(w, http.StatusBadRequest, backend.ErrorBody{
			Message: "tools 结构非法: " + toolsErr.Error(),
			Type:    errTypeInvalidRequest,
			Code:    errCodeInvalidTools,
		})
		return
	}

	stream, streamErr := parseStreamFlag(params["stream"])
	if streamErr != nil {
		writeError(w, http.StatusBadRequest, backend.ErrorBody{
			Message: streamErr.Error(),
			Type:    errTypeInvalidRequest,
			Code:    "invalid_stream",
		})
		return
	}

	switch {
	case !stream:
		// 非流式：受控推理循环（R2/R14/R15）→ 标准响应骨架 JSON。
		resp, err := h.cfg.Runner.Run(r.Context(), m, params)
		if err != nil {
			h.writeRunError(w, err)
			return
		}
		resp.Model = m.ID // R3：响应 model 回显请求使用的对外模型 id。
		writeJSON(w, http.StatusOK, resp)
	case len(tools) > 0:
		// stream=true 且带 tools（G1）：上游走非流式调用（full/partial/none 策略由
		// loop 内部处理），再按 OpenAI chunk 规范模拟 SSE 流（R12/KTD8）。
		// 先建立 SSE 连接并启动心跳，循环/工具执行阶段持续保活（评审修正：
		// 代理 idle 超时不得切断长循环）。
		sse, err := NewSSEWriter(w)
		if err != nil {
			h.writeRunError(w, err)
			return
		}
		sse.StartHeartbeat(r.Context())
		defer sse.Stop()
		includeUsage := streamOptionsIncludeUsage(params)
		p := dropStreamParams(params) // 剥离 stream/stream_options，避免上游误判为流式。
		resp, err := h.cfg.Runner.Run(r.Context(), m, p)
		if err != nil {
			// 循环失败：以流内错误事件 + [DONE] 表达（评审修正：不裸断连接）。
			_ = sse.WriteErrorEvent(stableErrorMessage(err), errorTypeOf(err), "", errorCodeOf(err))
			_ = sse.WriteDONE()
			return
		}
		resp.Model = m.ID
		_ = SimulateStream(r.Context(), w, resp, SimulateOptions{IncludeUsage: includeUsage})
		// 模拟流写出失败（客户端断开/写超时）时头部已发出，无更多可做。
	default:
		// stream=true 且无 tools：按注册表 Streaming 能力分流（评审修正：
		// streaming=false 的模型走非流式调用 + 模拟流，不静默透传）。
		if !m.Capabilities.Streaming {
			sse, err := NewSSEWriter(w)
			if err != nil {
				h.writeRunError(w, err)
				return
			}
			sse.StartHeartbeat(r.Context())
			defer sse.Stop()
			includeUsage := streamOptionsIncludeUsage(params)
			p := dropStreamParams(params)
			resp, err := h.cfg.Runner.Run(r.Context(), m, p)
			if err != nil {
				_ = sse.WriteErrorEvent(stableErrorMessage(err), errorTypeOf(err), "", errorCodeOf(err))
				_ = sse.WriteDONE()
				return
			}
			resp.Model = m.ID
			_ = SimulateStream(r.Context(), w, resp, SimulateOptions{IncludeUsage: includeUsage})
			return
		}
		if h.cfg.Client == nil {
			writeError(w, http.StatusInternalServerError, backend.ErrorBody{
				Message: "流式后端未配置（HandlerConfig.Client 为空）",
				Type:    errTypeServer,
				Code:    errCodeInternal,
			})
			return
		}
		upstream, err := h.cfg.Client.Stream(r.Context(), m, params)
		if err != nil {
			// 流尚未开始即失败（上游非 2xx/网络错误）：以失败记录触发审计（R17），
			// 再返回统一错误体（UpstreamError → 502，其余 → 500）。
			h.auditStreamFailure(m, msgs, err)
			h.writeRunError(w, err)
			return
		}
		var onCall audit.OnCall
		if h.cfg.Notifier != nil {
			onCall = h.cfg.Notifier.Notify
		}
		err = ProxyStream(r.Context(), w, upstream, ProxyOptions{
			Model:    m.ID,
			Strategy: "direct",
			OnCall:   onCall,
			Input:    audit.InputSummary{MessageCount: len(msgs)},
		})
		if err != nil {
			// 流已开始：截断/断连等已通过流内错误事件与 [DONE] 表达（KTD7），
			// 审计已由 ProxyStream 触发（F4）；这里不再写响应。
			return
		}
	}
}

// parseStreamFlag 解析 stream 字段（评审修正）：布尔值原样；字符串 "true"/"false"
// 宽松解析；其他类型/值返回明确错误（不静默降级为非流式）。
func parseStreamFlag(v any) (bool, error) {
	switch t := v.(type) {
	case nil:
		return false, nil
	case bool:
		return t, nil
	case string:
		switch t {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, fmt.Errorf("stream 字段字符串值 %q 非法（应为 true|false）", t)
		}
	default:
		return false, fmt.Errorf("stream 字段类型非法（%T，应为布尔）", v)
	}
}

// stableErrorMessage 从内部错误提取稳定错误文本（不泄漏原始堆栈，KTD8）。
func stableErrorMessage(err error) string {
	var ue *backend.UpstreamError
	var ie *backend.IncompleteResponseError
	var ve *loop.ValidationExhaustedError
	var ul *loop.UpstreamLimitError
	var te *tooling.Error
	switch {
	case errors.As(err, &ue):
		return "上游服务错误"
	case errors.As(err, &ie):
		return "上游模型未生成可见内容，请提高 max_tokens 或调整模型推理设置后重试"
	case errors.As(err, &ve):
		return "模型输出校验失败，请调整请求后重试"
	case errors.As(err, &ul):
		return "单请求上游调用次数超过上限"
	case errors.As(err, &te):
		return "工具调用处理失败"
	default:
		return "内部服务错误"
	}
}

// errorCodeOf 返回内部错误的稳定错误码（供流内错误事件，KTD8）。
func errorCodeOf(err error) string {
	var ue *backend.UpstreamError
	var ie *backend.IncompleteResponseError
	var ve *loop.ValidationExhaustedError
	var ul *loop.UpstreamLimitError
	var te *tooling.Error
	switch {
	case errors.As(err, &ue):
		return "upstream_error"
	case errors.As(err, &ie):
		return errCodeIncomplete
	case errors.As(err, &ve):
		return errCodeValidationFailed
	case errors.As(err, &ul):
		return errCodeUpstreamLimit
	case errors.As(err, &te):
		return errCodeToolingFailed
	default:
		return errCodeInternal
	}
}

func errorTypeOf(err error) string {
	var ue *backend.UpstreamError
	var ie *backend.IncompleteResponseError
	if errors.As(err, &ue) || errors.As(err, &ie) {
		return errTypeUpstream
	}
	return errTypeServer
}

// decodeChatBody 读取并解析请求体为 map[string]any（与 backend/tooling/loop
// 一致的形态）。空体返回 io.EOF；非 JSON 对象返回解析错误；
// 超过 maxRequestBody 返回 ErrBodyTooLarge（评审修正：413 而非截断解析）。
var ErrBodyTooLarge = errors.New("请求体超过大小上限")

func decodeChatBody(r *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, io.EOF
	}
	if len(body) > maxRequestBody {
		return nil, ErrBodyTooLarge
	}
	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil {
		return nil, err
	}
	return params, nil
}

// streamOptionsIncludeUsage 提取 stream_options.include_usage（KTD8：模拟流在流末
// 发 usage chunk，choices 为空数组）。
func streamOptionsIncludeUsage(params map[string]any) bool {
	so, ok := params["stream_options"].(map[string]any)
	if !ok {
		return false
	}
	v, _ := so["include_usage"].(bool)
	return v
}

// dropStreamParams 复制参数表并剥离仅流式语义的字段（stream/stream_options），
// 供"上游非流式调用 + 模拟流"路径使用；返回新 map，不修改输入。
func dropStreamParams(params map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for k, v := range params {
		if k == "stream" || k == "stream_options" {
			continue
		}
		out[k] = v
	}
	return out
}

// writeRunError 把 loop/backend/tooling 错误映射为统一错误体（KTD8）：
// 上游非 2xx（*backend.UpstreamError）→ 502（错误体保留上游 message/type/code，
// 已由 backend 层清洗）；其余内部错误 → 500，message 为稳定文本，不泄漏原始堆栈。
func (h *handler) writeRunError(w http.ResponseWriter, err error) {
	var ue *backend.UpstreamError
	if errors.As(err, &ue) {
		eb := backend.ErrorBody{}
		if ue.Body != nil {
			eb = *ue.Body
		}
		if eb.Message == "" {
			eb.Message = "上游服务错误"
		}
		if eb.Type == "" {
			eb.Type = errTypeUpstream
		}
		if eb.Code == "" {
			eb.Code = "upstream_error"
		}
		writeError(w, http.StatusBadGateway, eb)
		return
	}
	var ie *backend.IncompleteResponseError
	if errors.As(err, &ie) {
		writeError(w, http.StatusBadGateway, backend.ErrorBody{
			Message: stableErrorMessage(err),
			Type:    errTypeUpstream,
			Code:    errorCodeOf(err),
		})
		return
	}
	var ve *loop.ValidationExhaustedError
	if errors.As(err, &ve) {
		writeError(w, http.StatusInternalServerError, backend.ErrorBody{
			Message: "模型输出校验失败，请调整请求后重试",
			Type:    errTypeServer,
			Code:    errCodeValidationFailed,
		})
		return
	}
	var ul *loop.UpstreamLimitError
	if errors.As(err, &ul) {
		writeError(w, http.StatusInternalServerError, backend.ErrorBody{
			Message: "单请求上游调用次数超过上限",
			Type:    errTypeServer,
			Code:    errCodeUpstreamLimit,
		})
		return
	}
	var te *tooling.Error
	if errors.As(err, &te) {
		writeError(w, http.StatusInternalServerError, backend.ErrorBody{
			Message: "工具调用处理失败（错误类型码 " + string(te.Kind) + "）",
			Type:    errTypeServer,
			Code:    errCodeToolingFailed,
		})
		return
	}
	writeError(w, http.StatusInternalServerError, backend.ErrorBody{
		Message: "内部服务错误",
		Type:    errTypeServer,
		Code:    errCodeInternal,
	})
}

// auditStreamFailure 为"流尚未开始即失败"的直接流式调用产出审计记录（R17）：
// 上游非 2xx 或网络错误。流已开始的失败由 ProxyStream 以部分记录触发（F4）。
func (h *handler) auditStreamFailure(m *registry.Model, msgs []any, err error) {
	if h.cfg.Notifier == nil {
		return
	}
	h.cfg.Notifier.Notify(audit.CallRecord{
		Timestamp: time.Now(),
		Model:     m.ID,
		Strategy:  "direct",
		Input:     audit.InputSummary{MessageCount: len(msgs)},
		Error:     err.Error(),
		Stream:    true,
	})
}
