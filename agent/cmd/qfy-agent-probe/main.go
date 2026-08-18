// Command qfy-agent-probe 探测 OpenAI-compatible 后端的实际能力。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type config struct {
	BaseURL    string
	Model      string
	APIKey     string
	Timeout    time.Duration
	MaxTokens  int
	HTTPClient *http.Client
}

type check struct {
	Status              string   `json:"status"`
	HTTPStatus          int      `json:"http_status,omitempty"`
	Message             string   `json:"message,omitempty"`
	IDs                 []string `json:"ids,omitempty"`
	TargetListed        bool     `json:"target_listed,omitempty"`
	FinishReason        string   `json:"finish_reason,omitempty"`
	HasContent          bool     `json:"has_content,omitempty"`
	HasReasoningContent bool     `json:"has_reasoning_content,omitempty"`
	NativeToolCall      bool     `json:"native_tool_call,omitempty"`
	ArgumentsValid      bool     `json:"arguments_valid,omitempty"`
	EventCount          int      `json:"event_count,omitempty"`
	SawDONE             bool     `json:"saw_done,omitempty"`
}

type report struct {
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	Models     check  `json:"models"`
	Chat       check  `json:"chat"`
	JSONObject check  `json:"json_object"`
	Tools      check  `json:"tools"`
	SSE        check  `json:"sse"`
	OK         bool   `json:"ok"`
}

type completion struct {
	Choices []struct {
		Message struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func main() {
	baseURL := flag.String("base-url", "http://192.168.1.91:1234/v1", "OpenAI-compatible API 基础地址")
	model := flag.String("model", "", "后端模型 ID（必填）")
	apiKey := flag.String("api-key", "", "可选 API key")
	timeout := flag.Duration("timeout", 2*time.Minute, "单次请求超时")
	maxTokens := flag.Int("max-tokens", 512, "补全 token 上限")
	flag.Parse()
	if strings.TrimSpace(*model) == "" {
		fmt.Fprintln(os.Stderr, "必须指定 -model")
		os.Exit(2)
	}

	cfg := config{BaseURL: *baseURL, Model: *model, APIKey: *apiKey, Timeout: *timeout, MaxTokens: *maxTokens}
	r := run(context.Background(), cfg)
	_ = json.NewEncoder(os.Stdout).Encode(r)
	if !r.OK {
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) report {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 512
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.Timeout}
	}
	r := report{BaseURL: cfg.BaseURL, Model: cfg.Model}
	r.Models = probeModels(ctx, cfg)
	r.Chat = probeChat(ctx, cfg)
	r.JSONObject = probeJSONObject(ctx, cfg)
	r.Tools = probeTools(ctx, cfg)
	r.SSE = probeSSE(ctx, cfg)
	jsonUsable := r.JSONObject.Status == "pass" || r.JSONObject.Status == "unsupported"
	r.OK = r.Models.Status == "pass" && r.Models.TargetListed && r.Chat.Status == "pass" && jsonUsable && r.Tools.Status == "pass" && r.SSE.Status == "pass"
	return r
}

func probeModels(ctx context.Context, cfg config) check {
	status, body, err := request(ctx, cfg, http.MethodGet, "/models", nil, "")
	c := check{HTTPStatus: status}
	if err != nil {
		c.Status, c.Message = "error", err.Error()
		return c
	}
	if status != http.StatusOK {
		c.Status, c.Message = "error", "models endpoint returned non-200"
		return c
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		c.Status, c.Message = "error", "models response is not valid JSON"
		return c
	}
	for _, item := range payload.Data {
		c.IDs = append(c.IDs, item.ID)
		if item.ID == cfg.Model {
			c.TargetListed = true
		}
	}
	c.Status = "pass"
	return c
}

func probeChat(ctx context.Context, cfg config) check {
	payload := baseCompletionPayload(cfg, "只回答 OK")
	status, body, err := request(ctx, cfg, http.MethodPost, "/chat/completions", payload, "application/json")
	c := check{HTTPStatus: status}
	if err != nil {
		c.Status, c.Message = "error", err.Error()
		return c
	}
	var cc completion
	if status != http.StatusOK || json.Unmarshal(body, &cc) != nil || len(cc.Choices) == 0 {
		c.Status, c.Message = "error", "chat completion failed or returned an invalid response"
		return c
	}
	c.FinishReason = cc.Choices[0].FinishReason
	c.HasContent = hasText(cc.Choices[0].Message.Content)
	c.HasReasoningContent = hasText(cc.Choices[0].Message.ReasoningContent)
	if !c.HasContent && c.HasReasoningContent {
		c.Status, c.Message = "inconclusive", "model returned reasoning_content without visible content"
		return c
	}
	if !c.HasContent {
		c.Status, c.Message = "inconclusive", "model returned no visible content"
		return c
	}
	c.Status = "pass"
	return c
}

func probeJSONObject(ctx context.Context, cfg config) check {
	payload := baseCompletionPayload(cfg, "只输出 {\"ok\":true}")
	payload["response_format"] = map[string]any{"type": "json_object"}
	status, body, err := request(ctx, cfg, http.MethodPost, "/chat/completions", payload, "application/json")
	c := check{HTTPStatus: status}
	if err != nil {
		c.Status, c.Message = "error", err.Error()
		return c
	}
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		c.Status, c.Message = "unsupported", "backend rejected response_format.json_object"
		return c
	}
	var cc completion
	if status != http.StatusOK || json.Unmarshal(body, &cc) != nil || len(cc.Choices) == 0 {
		c.Status, c.Message = "error", "json_object probe returned an invalid response"
		return c
	}
	c.Status = "pass"
	c.HasContent = hasText(cc.Choices[0].Message.Content)
	if !isJSONObjectContent(cc.Choices[0].Message.Content) {
		c.Status, c.Message = "inconclusive", "backend did not return a JSON object"
	}
	return c
}

func probeTools(ctx context.Context, cfg config) check {
	payload := baseCompletionPayload(cfg, "请调用 probe_weather 查询上海天气，不要直接回答")
	payload["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "probe_weather", "description": "查询城市天气", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"},
		},
	}}}
	payload["tool_choice"] = "required"
	status, body, err := request(ctx, cfg, http.MethodPost, "/chat/completions", payload, "application/json")
	c := check{HTTPStatus: status}
	if err != nil {
		c.Status, c.Message = "error", err.Error()
		return c
	}
	var cc completion
	if status != http.StatusOK || json.Unmarshal(body, &cc) != nil || len(cc.Choices) == 0 {
		c.Status, c.Message = "error", "tools probe failed or returned an invalid response"
		return c
	}
	for _, call := range cc.Choices[0].Message.ToolCalls {
		if call.ID == "" || call.Type != "function" || call.Function.Name != "probe_weather" {
			continue
		}
		c.NativeToolCall = true
		c.ArgumentsValid = validWeatherArguments(call.Function.Arguments)
	}
	if cc.Choices[0].FinishReason != "tool_calls" || !c.NativeToolCall || !c.ArgumentsValid {
		c.Status, c.Message = "inconclusive", "backend did not return a valid native tool call"
		return c
	}
	c.Status = "pass"
	return c
}

func probeSSE(ctx context.Context, cfg config) check {
	payload := baseCompletionPayload(cfg, "用一句话回答 OK")
	payload["stream"] = true
	status, body, err := requestStream(ctx, cfg, payload)
	c := check{HTTPStatus: status}
	if err != nil {
		c.Status, c.Message = "error", err.Error()
		return c
	}
	if status != http.StatusOK || body == nil {
		c.Status, c.Message = "error", "SSE endpoint returned non-200"
		return c
	}
	defer body.Close()
	scanner := bufio.NewScanner(io.LimitReader(body, 8<<20))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			c.SawDONE = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          *string         `json:"content"`
					ReasoningContent json.RawMessage `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
			c.EventCount++
			for _, choice := range chunk.Choices {
				if choice.Delta.Content != nil && *choice.Delta.Content != "" {
					c.HasContent = true
				}
				if hasText(choice.Delta.ReasoningContent) {
					c.HasReasoningContent = true
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		c.Status, c.Message = "error", "failed to read SSE response"
		return c
	}
	if !c.SawDONE {
		c.Status, c.Message = "error", "SSE stream ended without [DONE]"
		return c
	}
	if c.EventCount == 0 {
		c.Status, c.Message = "inconclusive", "SSE completed without a valid completion chunk"
		return c
	}
	if !c.HasContent && c.HasReasoningContent {
		c.Status, c.Message = "inconclusive", "SSE returned reasoning_content without visible content"
		return c
	}
	c.Status = "pass"
	return c
}

func baseCompletionPayload(cfg config, prompt string) map[string]any {
	return map[string]any{
		"model": cfg.Model, "messages": []any{map[string]any{"role": "user", "content": prompt}},
		"temperature": 0, "max_tokens": cfg.MaxTokens,
	}
}

func request(ctx context.Context, cfg config, method, path string, payload any, contentType string) (int, []byte, error) {
	req, err := newRequest(ctx, cfg, method, path, payload)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, data, err
}

func requestStream(ctx context.Context, cfg config, payload any) (int, io.ReadCloser, error) {
	req, err := newRequest(ctx, cfg, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, resp.Body, nil
}

func newRequest(ctx context.Context, cfg config, method, path string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	return req, nil
}

func hasText(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var text string
	return json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != ""
}

func rawJSONString(raw json.RawMessage) []byte {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []byte(text)
	}
	return raw
}

func isJSONObjectContent(raw json.RawMessage) bool {
	var value map[string]any
	return json.Unmarshal(rawJSONString(raw), &value) == nil && value != nil
}

func validWeatherArguments(raw json.RawMessage) bool {
	var args map[string]any
	if json.Unmarshal(rawJSONString(raw), &args) != nil || args == nil {
		return false
	}
	city, ok := args["city"].(string)
	return ok && strings.TrimSpace(city) != ""
}
