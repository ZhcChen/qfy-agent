// Package audit 定义审计记录与回调钩子（U5，R17/KTD9）。
//
// CallRecord 是单次后端调用的审计快照（KTD9）：非流式路径统一由 loop 层产出，
// 流式透传路径由 api 层在流结束/中断时产出（评审修正 G2）。
// OnCall 为可配置回调：Notifier 以 recover 包裹调用，回调 panic 不影响请求响应。
//
// 执行模型（F1）：回调在请求 goroutine 内同步触发；并发请求下回调被并发
// 调用，消费方落库逻辑须自保证并发安全；库不碰数据库（R17）。
package audit

import (
	"sync"
	"time"
)

// OnCall 审计回调：每次后端调用完成后同步触发（F1）。
// 回调不得 panic（库以 recover 兜底，但 panic 会丢失本次审计记录）；
// 回调内不得阻塞请求过久（README 建议落库异步化）。
type OnCall func(record CallRecord)

// InputSummary 输入摘要：消息数、各角色 content 前 N 字符、工具列表、轮次（R17）。
type InputSummary struct {
	// MessageCount 消息数。
	MessageCount int
	// RoleContents 角色 → content 摘要（前 N 字符，N 由 loop 配置）。
	RoleContents map[string]string
	// ToolNames 请求声明的工具列表。
	ToolNames []string
	// Round 当前轮次（从 0 开始）。
	Round int
}

// OutputSummary 输出摘要：content 前 N 字符或 tool_calls 概要（R17）。
type OutputSummary struct {
	// Content 助手消息 content 前 N 字符（无文本时为 ""）。
	Content string
	// ToolCalls 工具调用概要（无调用时为空）。
	ToolCalls []ToolCallSummary
}

// ToolCallSummary 单条工具调用概要。
type ToolCallSummary struct {
	// Name 工具名。
	Name string
	// Arguments arguments 前 N 字符。
	Arguments string
}

// CallRecord 单次后端调用的审计记录（KTD9：非流式由 loop 层产出；
// 流式透传由 api 层在流结束或中断时产出，评审修正 G2）。
type CallRecord struct {
	// Timestamp 调用时间戳。
	Timestamp time.Time
	// Model 模型（注册表 ID）。
	Model string
	// Strategy 采用的策略（direct/full/partial/none，见 loop 层定义）。
	Strategy string
	// Input 输入摘要。
	Input InputSummary
	// Output 输出摘要（调用失败时可能为空）。
	Output OutputSummary
	// Duration 调用耗时。
	Duration time.Duration
	// Error 错误（稳定错误信息；成功为空字符串）。
	Error string
	// Round 轮次（从 0 开始）。
	Round int
	// Stream 是否流式调用（非流式循环恒为 false；流式透传恒为 true，由 api 层产出）。
	Stream bool
	// Truncated 上游流是否截断（缺 [DONE]；仅流式透传记录，由 api 层产出）。
	Truncated bool
}

// Notifier 并发安全的审计通知器（F5）：支持运行期配置回调，
// Notify 以 recover 包裹回调调用，回调 panic 不影响调用方。
type Notifier struct {
	mu     sync.RWMutex
	onCall OnCall
}

// NewNotifier 构造空通知器；未配置回调时 Notify 为无操作。
func NewNotifier() *Notifier { return &Notifier{} }

// SetOnCall 配置（或传 nil 清除）审计回调；运行期可重复调用（F5）。
func (n *Notifier) SetOnCall(fn OnCall) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onCall = fn
}

// Notify 同步触发审计回调（F1）。回调 panic 被 recover，不影响调用方（KTD9）；
// 未配置回调时为无操作。
func (n *Notifier) Notify(record CallRecord) {
	n.mu.RLock()
	fn := n.onCall
	n.mu.RUnlock()
	if fn == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(record)
}
