package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qfy-agent/qfy-agent/audit"
	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/loop"
	"github.com/qfy-agent/qfy-agent/registry"
	"github.com/qfy-agent/qfy-agent/tooling"
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

	stream := false
	if v, ok := params["stream"].(bool); ok {
		stream = v
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
		includeUsage := streamOptionsIncludeUsage(params)
		p := dropStreamParams(params) // 剥离 stream/stream_options，避免上游误判为流式。
		resp, err := h.cfg.Runner.Run(r.Context(), m, p)
		if err != nil {
			h.writeRunError(w, err)
			return
		}
		resp.Model = m.ID
		_ = SimulateStream(r.Context(), w, resp, SimulateOptions{IncludeUsage: includeUsage})
		// 模拟流写出失败（客户端断开/写超时）时头部已发出，无更多可做。
	default:
		// stream=true 且无 tools：透传真实上游流（R11）。
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

// decodeChatBody 读取并解析请求体为 map[string]any（与 backend/tooling/loop
// 一致的形态）。空体返回 io.EOF；非 JSON 对象返回解析错误。
func decodeChatBody(r *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, io.EOF
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
		Error:     errorText(err),
		Stream:    true,
	})
}

// errorText 提取稳定错误文本（成功为空）。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
