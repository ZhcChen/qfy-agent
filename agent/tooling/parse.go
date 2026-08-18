package tooling

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// ParseToolCallJSON 解析模型输出为工具调用 JSON 对象（KTD4 解析降级链）：
//
//  1. 直接 json.Unmarshal：整体（去除首尾空白后）即合法 JSON；
//  2. 剥离 ```json 代码围栏与前后散文：围栏内内容为合法 JSON；
//  3. 括号配对扫描：从文本中提取首个完整 JSON 对象（字符串感知，容忍前后散文）。
//
// 返回提取到的 JSON 对象原始字节（保证合法 JSON）；全部失败返回错误。
// arguments 由调用方用 json.RawMessage 延迟解析。
func ParseToolCallJSON(content string) ([]byte, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, errors.New("模型输出为空，未提取到 JSON 对象")
	}
	if json.Valid([]byte(trimmed)) {
		return []byte(trimmed), nil
	}
	if inner, ok := stripFence(trimmed); ok {
		return []byte(inner), nil
	}
	if obj, ok := extractJSONObject(trimmed); ok {
		return []byte(obj), nil
	}
	return nil, errors.New("模型输出不是合法 JSON，且无法通过降级链提取 JSON 对象")
}

// fenceRe 匹配 ```json 或 ``` 代码围栏；语言标注可选，内容按非贪婪捕获（含跨行）。
var fenceRe = regexp.MustCompile("(?s)```[a-zA-Z]*\\s*\\n?(.*?)```")

// stripFence 从文本中提取首个"围栏内为合法 JSON"的代码块内容（剥离围栏与前后散文）。
func stripFence(s string) (string, bool) {
	for _, m := range fenceRe.FindAllStringSubmatch(s, -1) {
		inner := strings.TrimSpace(m[1])
		if json.Valid([]byte(inner)) {
			return inner, true
		}
	}
	return "", false
}

// extractJSONObject 括号配对扫描：按序考察文本中的每个 '{'，做字符串感知的深度配对扫描，
// 返回首个完整配对且是合法 JSON 的 {…} 片段（KTD4 第三级降级）。
func extractJSONObject(s string) (string, bool) {
	for start := 0; start < len(s); {
		i := strings.IndexByte(s[start:], '{')
		if i < 0 {
			return "", false
		}
		abs := start + i
		end, ok := matchBrace(s, abs)
		if !ok {
			// 无配对闭合：跳过该 '{'，继续找下一个。
			start = abs + 1
			continue
		}
		cand := s[abs : end+1]
		if json.Valid([]byte(cand)) {
			return cand, true
		}
		// 片段不是合法 JSON（如散文花括号）：从片段内继续找下一个 '{'。
		start = abs + 1
	}
	return "", false
}

// matchBrace 从 start（须为 '{'）开始做深度配对扫描：跳过 JSON 字符串内的花括号
// （含转义），返回与之匹配的 '}' 下标；无配对返回 (0, false)。
func matchBrace(s string, start int) (int, bool) {
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}
