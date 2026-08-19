package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ZhcChen/qfy-agent/agent/audit"
	"github.com/ZhcChen/qfy-agent/agent/backend"
	"github.com/ZhcChen/qfy-agent/agent/internal/anyutil"
	"github.com/ZhcChen/qfy-agent/agent/registry"
	"github.com/ZhcChen/qfy-agent/agent/schema"
	"github.com/ZhcChen/qfy-agent/agent/tooling"
)

// ValidationExhaustedError 校验重试耗尽后的稳定错误（R15/KTD6）：
// 只暴露错误类型码与概要信息，不泄漏模型原始输出与内部堆栈。
type ValidationExhaustedError struct {
	// Last 最后一次校验错误（类型码 + 概要）。
	Last *tooling.Error
}

func (e *ValidationExhaustedError) Error() string {
	if e == nil || e.Last == nil {
		return "模型输出校验连续失败"
	}
	return fmt.Sprintf("模型输出校验连续失败（类型码 %s）: %s", e.Last.Kind, e.Last.Message)
}

// Unwrap 暴露底层校验错误，供上层按类型码分类（KTD6）。
func (e *ValidationExhaustedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Last
}

// callRound 执行一轮模型调用（R15）：经 U3 策略调用后端；返回 *tooling.Error
// （KindParse/KindValidation，按 Kind 判定不做消息匹配）时把校验错误（含 U4
// 结构化错误路径与类型码）作为消息回喂模型，最多重试 MaxValidationRetries 次；
// 仍失败返回 *ValidationExhaustedError（稳定错误）。非校验错误（backend 错误等）
// 原样上抛。
//
// JSON mode（response_format.json_object）下对响应 content 做合法 JSON 校验
// （KTD6 计划承诺：输出必须做 schema 校验），失败同样回喂重试。
// 校验回喂消息只作用于本轮重试调用（调用成功后即从后续轮次中消失）；每次调用
// 前检查单请求上游调用预算（F3，transport 层另有每次发送前的硬边界）。
// toolResults 为上一轮工具执行概要（随本轮调用入审计，评审修正 F6）。
func (r *Runner) callRound(ctx context.Context, m *registry.Model, params map[string]any,
	round int, strategy string, toolResults []audit.ToolResult) (*backend.ChatCompletion, error) {
	msgs, _ := anyutil.AsSlice(params["messages"])
	cur := make([]any, len(msgs))
	copy(cur, msgs)
	params["messages"] = cur

	for attempt := 0; ; attempt++ {
		if err := r.checkUpstreamBudget(ctx); err != nil {
			return nil, err
		}
		start := time.Now()
		cc, err := r.strategies.Call(ctx, m, params, nil)
		r.notify(m, strategy, cur, params, cc, err, time.Since(start), round, toolResults)
		if err == nil {
			if te := checkJSONModeOutput(params, cc); te != nil {
				if attempt >= r.cfg.MaxValidationRetries {
					return nil, &ValidationExhaustedError{Last: te}
				}
				cur = append(cur, validationFeedbackMessage(te))
				params["messages"] = cur
				continue
			}
			return cc, nil
		}
		var te *tooling.Error
		if errors.As(err, &te) && (te.Kind == tooling.KindParse || te.Kind == tooling.KindValidation) {
			if attempt >= r.cfg.MaxValidationRetries {
				// R15/KTD6：重试耗尽，返回稳定错误（不泄漏模型原始输出与堆栈）。
				return nil, &ValidationExhaustedError{Last: te}
			}
			cur = append(cur, validationFeedbackMessage(te))
			params["messages"] = cur
			continue
		}
		return nil, err
	}
}

// checkJSONModeOutput JSON mode 输出校验（KTD6 计划承诺）：请求声明
// response_format.type=json_object 时，响应主 choice 的 content 必须是合法 JSON
// 对象（反序列化为 map）。不满足返回 *tooling.Error{KindValidation}（走 R15
// 回喂重试）；非 JSON mode 请求返回 nil。
func checkJSONModeOutput(params map[string]any, cc *backend.ChatCompletion) *tooling.Error {
	rf, ok := params["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		return nil
	}
	if cc == nil || len(cc.Choices) == 0 {
		return &tooling.Error{Kind: tooling.KindValidation, Message: "JSON mode 响应缺少 choices"}
	}
	var content string
	if err := json.Unmarshal(cc.Choices[0].Message.Content, &content); err != nil {
		return &tooling.Error{Kind: tooling.KindValidation, Message: "JSON mode 响应没有文本内容"}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(content), &obj); err != nil || obj == nil {
		return &tooling.Error{Kind: tooling.KindValidation, Message: "JSON mode 输出不是合法 JSON 对象"}
	}
	return nil
}

// validationFeedbackMessage 构造校验失败回喂消息（R15/KTD6）：携带错误类型码、
// 概要原因与 U4 结构化错误（字段路径 + 类型码），提示模型按输出约束重新输出。
func validationFeedbackMessage(te *tooling.Error) map[string]any {
	var b strings.Builder
	fmt.Fprintf(&b, "你上一条输出校验失败（错误类型码: %s）: %s", te.Kind, te.Message)
	if len(te.Details) > 0 {
		b.WriteString("（错误位置: ")
		for i, d := range te.Details {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(formatDetail(d))
		}
		b.WriteString("）")
	}
	b.WriteString("。请按输出约束重新输出正确的 JSON 对象。")
	return map[string]any{"role": "user", "content": b.String()}
}

// formatDetail 格式化单条 U4 结构化校验错误（字段路径 + 类型码）。
func formatDetail(d schema.Error) string {
	path := d.Path
	if path == "" {
		path = "root"
	}
	return fmt.Sprintf("%s [%s]", path, d.Kind)
}
