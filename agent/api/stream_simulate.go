package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/qfy-agent/qfy-agent/backend"
)

// DefaultChunkSize 模拟流每块 content/arguments 的字符（rune）数（R12）。
const DefaultChunkSize = 8

// SimulateOptions 模拟流配置（R12）。
type SimulateOptions struct {
	// ChunkSize 每块 content/arguments 的 rune 数；0 取默认 8。
	ChunkSize int
	// IncludeUsage 是否在流末发送 usage chunk（choices 为空数组，KTD8）后 [DONE]。
	IncludeUsage bool
	// HeartbeatInterval 心跳间隔；0 取默认 15s，负值关闭心跳。
	HeartbeatInterval time.Duration
	// WriteTimeout 写超时续期间隔；0 取默认 30s，负值禁用续期。
	WriteTimeout time.Duration
}

// SimulateStream 把完整非流式响应按 OpenAI chunk 规范模拟为 SSE 流（R12/KTD8）：
//
//   - 首 chunk 带 delta.role="assistant"（仅此一次）；
//   - content 按块（默认 8 rune，可配置）增量发出；
//   - tool_calls 按 index 组织 delta：该调用首 chunk 带 id+type+function.name，
//     后续仅 function.arguments 字符串增量（按 rune 切分）；
//   - 末 chunk delta={} 且 finish_reason 非 null（白名单化）；
//   - include_usage 时最后发 usage chunk（choices 为空数组），随后 [DONE]。
//
// 模拟流同样走 sse.go 的心跳与写超时续期（KTD7）。仅模拟第一个 choice；
// 空 choices 的响应退化为"末 chunk + [DONE]"以保持流合法。
func SimulateStream(ctx context.Context, w http.ResponseWriter, resp *backend.ChatCompletion, opts SimulateOptions) error {
	chunkSize := opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	sse, err := NewSSEWriter(w, WithHeartbeatInterval(opts.HeartbeatInterval), WithWriteTimeout(opts.WriteTimeout))
	if err != nil {
		return err
	}
	defer sse.Stop()
	sse.StartHeartbeat(ctx)

	base := streamChunk{ID: resp.ID, Object: "chat.completion.chunk", Created: resp.Created, Model: resp.Model}

	if len(resp.Choices) > 0 {
		if err := simulateChoice(sse, base, resp.Choices[0], chunkSize); err != nil {
			return err
		}
	} else {
		// 空 choices：直接发末 chunk（finish_reason=stop）保持流合法。
		if err := writeSimChunk(sse, base, 0, streamDelta{}, "stop"); err != nil {
			return err
		}
	}

	// include_usage：最后发 usage chunk（choices 为空数组，KTD8），随后 [DONE]。
	if opts.IncludeUsage && resp.Usage != nil {
		raw, err := json.Marshal(resp.Usage)
		if err != nil {
			return err
		}
		uc := base
		uc.Choices = []streamChoice{}
		uc.Usage = raw
		if err := writeSimJSON(sse, uc); err != nil {
			return err
		}
	}
	return sse.WriteDONE()
}

// simulateChoice 模拟单个 choice 的 chunk 序列。
func simulateChoice(sse *SSEWriter, base streamChunk, ch backend.Choice, chunkSize int) error {
	idx := ch.Index
	// 首 chunk：delta.role="assistant"（仅此一次）。
	if ch.Message.Role != "" {
		if err := writeSimChunk(sse, base, idx, streamDelta{Role: ch.Message.Role}, ""); err != nil {
			return err
		}
	}
	// content 增量分块。
	if content := rawText(ch.Message.Content); content != "" {
		for _, seg := range splitRunes(content, chunkSize) {
			s := seg
			if err := writeSimChunk(sse, base, idx, streamDelta{Content: &s}, ""); err != nil {
				return err
			}
		}
	}
	// tool_calls 按 index 组织 delta：首块 id+type+function.name，后续仅 arguments 增量。
	for i, tc := range ch.Message.ToolCalls {
		first := streamToolCall{Index: i, ID: tc.ID, Type: "function", Function: streamFunction{Name: tc.Function.Name}}
		if err := writeSimChunk(sse, base, idx, streamDelta{ToolCalls: []streamToolCall{first}}, ""); err != nil {
			return err
		}
		for _, seg := range splitRunes(rawText(tc.Function.Arguments), chunkSize) {
			s := seg
			if err := writeSimChunk(sse, base, idx, streamDelta{ToolCalls: []streamToolCall{{Index: i, Function: streamFunction{Arguments: s}}}}, ""); err != nil {
				return err
			}
		}
	}
	// 末 chunk：delta={} 且 finish_reason 非 null（白名单化，KTD8）。
	fr := backend.NormalizeFinishReason(ch.FinishReason)
	return writeSimChunk(sse, base, idx, streamDelta{}, fr)
}

// writeSimChunk 组装并写出一个模拟 chunk；finishReason 为空时输出 "finish_reason":null。
func writeSimChunk(sse *SSEWriter, base streamChunk, idx int, delta streamDelta, finishReason string) error {
	c := base
	c.Choices = []streamChoice{{Index: idx, Delta: delta}}
	if finishReason != "" {
		fr := finishReason
		c.Choices[0].FinishReason = &fr
	}
	return writeSimJSON(sse, c)
}

// writeSimJSON 序列化并写出模拟 chunk。
func writeSimJSON(sse *SSEWriter, c streamChunk) error {
	b, err := json.Marshal(&c)
	if err != nil {
		return err
	}
	return sse.WriteEvent(b)
}

// rawText 把 json.RawMessage 文本化为字符串：JSON 字符串解引号；
// 其他形态（如多模态数组、null）分别原样文本化或视为空。
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// splitRunes 按 rune 数把 s 切分为 n 字符的块（n<=0 时整体一块）。
func splitRunes(s string, n int) []string {
	if n <= 0 {
		return []string{s}
	}
	rs := []rune(s)
	if len(rs) == 0 {
		return nil
	}
	out := make([]string, 0, (len(rs)+n-1)/n)
	for i := 0; i < len(rs); i += n {
		end := i + n
		if end > len(rs) {
			end = len(rs)
		}
		out = append(out, string(rs[i:end]))
	}
	return out
}
