package audit

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCallRecordFields：CallRecord 结构字段齐全（R17：时间戳、模型、策略、
// 输入/输出摘要、耗时、错误、轮次、是否流式），可承载完整审计信息。
func TestCallRecordFields(t *testing.T) {
	rec := CallRecord{
		Timestamp: time.Now(),
		Model:     "gemma-4-e4b",
		Strategy:  "none",
		Input: InputSummary{
			MessageCount: 2,
			RoleContents: map[string]string{"user": "请把列\"客户名称\"映射到标准字段"},
			ToolNames:    []string{"map_column"},
		},
		Output: OutputSummary{
			Content: "已完成映射",
			ToolCalls: []ToolCallSummary{
				{Name: "map_column", Arguments: `{"column":"客户名"}`},
			},
		},
		Duration: 123 * time.Millisecond,
		Error:    "",
		Round:    0,
		Stream:   false,
	}
	if rec.Model != "gemma-4-e4b" || rec.Strategy != "none" {
		t.Errorf("模型/策略字段不符: %+v", rec)
	}
	if rec.Input.MessageCount != 2 || len(rec.Input.RoleContents) != 1 {
		t.Errorf("输入摘要字段不符: %+v", rec.Input)
	}
	if rec.Output.Content != "已完成映射" {
		t.Errorf("输出摘要字段不符: %+v", rec)
	}
	if len(rec.Output.ToolCalls) != 1 || rec.Output.ToolCalls[0].Name != "map_column" {
		t.Errorf("tool_calls 概要字段不符: %+v", rec.Output.ToolCalls)
	}
	if rec.Duration <= 0 {
		t.Errorf("耗时字段应为正: %v", rec.Duration)
	}
	if rec.Error != "" || rec.Stream {
		t.Errorf("错误/流式字段默认值不符: %+v", rec)
	}
}

// TestNotifyReceivesRecord：配置回调后 Notify 收到完整记录（R17）。
func TestNotifyReceivesRecord(t *testing.T) {
	n := NewNotifier()
	var got CallRecord
	var gotFlag bool
	n.SetOnCall(func(rec CallRecord) {
		got = rec
		gotFlag = true
	})
	want := CallRecord{Model: "m1", Strategy: "full", Round: 2}
	n.Notify(want)
	if !gotFlag {
		t.Fatal("回调未被触发")
	}
	if got.Model != "m1" || got.Strategy != "full" || got.Round != 2 {
		t.Errorf("回调收到的记录不符: %+v", got)
	}
}

// TestNotifyNoopWithoutCallback：未配置回调时 Notify 为无操作，不 panic。
func TestNotifyNoopWithoutCallback(t *testing.T) {
	n := NewNotifier()
	n.Notify(CallRecord{Model: "m1"}) // 不应 panic
}

// TestNotifyCallbackPanicRecovered：回调 panic 被 recover，不影响 Notify 调用方（KTD9）。
func TestNotifyCallbackPanicRecovered(t *testing.T) {
	n := NewNotifier()
	calls := 0
	n.SetOnCall(func(rec CallRecord) {
		calls++
		panic("审计回调爆炸")
	})
	n.Notify(CallRecord{Model: "m1"}) // 不应 panic
	if calls != 1 {
		t.Errorf("回调应被调用 1 次（panic 前已进入），得到 %d", calls)
	}
	// 后续通知仍可用（回调本身未失效）。
	n.Notify(CallRecord{Model: "m2"})
	if calls != 2 {
		t.Errorf("回调应持续生效，得到 %d 次调用", calls)
	}
}

// TestSetOnCallReplaces：SetOnCall 运行期可重复配置（F5 支持运行期注册）。
func TestSetOnCallReplaces(t *testing.T) {
	n := NewNotifier()
	var first, second []string
	n.SetOnCall(func(rec CallRecord) { first = append(first, rec.Model) })
	n.Notify(CallRecord{Model: "a"})
	n.SetOnCall(func(rec CallRecord) { second = append(second, rec.Model) })
	n.Notify(CallRecord{Model: "b"})
	if len(first) != 1 || first[0] != "a" {
		t.Errorf("旧回调应只收到替换前的记录: %v", first)
	}
	if len(second) != 1 || second[0] != "b" {
		t.Errorf("新回调应只收到替换后的记录: %v", second)
	}
}

// TestSetOnCallClear：传 nil 清除回调后 Notify 为无操作。
func TestSetOnCallClear(t *testing.T) {
	n := NewNotifier()
	n.SetOnCall(func(rec CallRecord) { t.Error("清除后不应再触发") })
	n.SetOnCall(nil)
	n.Notify(CallRecord{Model: "a"})
}

// TestNotifyConcurrent：并发 SetOnCall/Notify 无数据竞争（F5，go test -race 覆盖）。
func TestNotifyConcurrent(t *testing.T) {
	n := NewNotifier()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				n.Notify(CallRecord{Model: strings.Repeat("m", i+1)})
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				n.SetOnCall(func(rec CallRecord) {})
			}
		}()
	}
	wg.Wait()
}
