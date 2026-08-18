package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunReportsCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"model-a"}]}`)
		case r.URL.Path == "/v1/chat/completions" && strings.Contains(r.Header.Get("Accept"), "text/event-stream"):
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
		case r.URL.Path == "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			raw := string(body)
			switch {
			case strings.Contains(raw, `"response_format"`):
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"message":"response_format json_object is unsupported"}}`)
			case strings.Contains(raw, `"tools"`):
				fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"probe_weather","arguments":"{\"city\":\"上海\"}"}}]},"finish_reason":"tool_calls"}]}`)
			default:
				fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	r := run(context.Background(), config{
		BaseURL: srv.URL + "/v1", Model: "model-a", Timeout: time.Second, HTTPClient: srv.Client(),
	})
	if !r.OK {
		t.Fatalf("完整能力探测应通过: %+v", r)
	}
	if r.JSONObject.Status != "unsupported" {
		t.Errorf("json_object 400 应归类为 unsupported，得到 %+v", r.JSONObject)
	}
	if !r.Tools.NativeToolCall || !r.Tools.ArgumentsValid || !r.SSE.SawDONE {
		t.Errorf("tools/SSE 摘要不完整: tools=%+v sse=%+v", r.Tools, r.SSE)
	}
}

func TestProbeToolsRejectsMissingIDAndType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"probe_weather","arguments":"{\"city\":\"上海\"}"}}]}}]}`)
	}))
	defer srv.Close()

	c := probeTools(context.Background(), config{
		BaseURL: srv.URL, Model: "model-a", Timeout: time.Second, HTTPClient: srv.Client(),
	})
	if c.Status != "inconclusive" || c.NativeToolCall {
		t.Fatalf("缺少 id/type 的 tool call 不应通过探测: %+v", c)
	}
}

func TestProbeSSERejectsEmptyCompletedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := probeSSE(context.Background(), config{
		BaseURL: srv.URL, Model: "model-a", Timeout: time.Second, HTTPClient: srv.Client(),
	})
	if c.Status != "inconclusive" || !c.SawDONE || c.EventCount != 0 {
		t.Fatalf("空完成流不应通过探测: %+v", c)
	}
}

func TestProbeSSERejectsChunksWithoutVisibleContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := probeSSE(context.Background(), config{BaseURL: srv.URL, Model: "model-a", HTTPClient: srv.Client()})
	if c.Status != "inconclusive" || c.HasContent {
		t.Fatalf("无可见正文的 SSE 不应通过: %+v", c)
	}
}

func TestProbeSSERejectsMalformedChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {bad json}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := probeSSE(context.Background(), config{BaseURL: srv.URL, Model: "model-a", HTTPClient: srv.Client()})
	if c.Status != "error" {
		t.Fatalf("畸形 SSE chunk 应报告错误: %+v", c)
	}
}

func TestProbeChatRejectsEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := probeChat(context.Background(), config{BaseURL: srv.URL, Model: "model-a", HTTPClient: srv.Client()})
	if c.Status != "inconclusive" {
		t.Fatalf("空正文不应通过 Chat 探测: %+v", c)
	}
}

func TestProbeJSONObjectValidatesContentAndHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		status        int
		want          string
	}{
		{name: "object", content: `{"ok":true}`, status: 200, want: "pass"},
		{name: "plain text", content: `not json`, status: 200, want: "inconclusive"},
		{name: "array", content: `[1]`, status: 200, want: "inconclusive"},
		{name: "unauthorized", status: 401, want: "error"},
		{name: "rate limited", status: 429, want: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, tc.content)
				}
			}))
			defer srv.Close()
			c := probeJSONObject(context.Background(), config{BaseURL: srv.URL, Model: "model-a", HTTPClient: srv.Client()})
			if c.Status != tc.want {
				t.Fatalf("状态应为 %s: %+v", tc.want, c)
			}
		})
	}
}

func TestProbeJSONObjectDoesNotTreatUnrelatedBadRequestAsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"invalid model"}}`)
	}))
	defer srv.Close()
	c := probeJSONObject(context.Background(), config{BaseURL: srv.URL, Model: "model-a", HTTPClient: srv.Client()})
	if c.Status != "error" {
		t.Fatalf("无关的 400 不应归类为不支持 json_object: %+v", c)
	}
}

func TestRunSkipsCapabilityRequestsWhenModelDiscoveryFails(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	r := run(context.Background(), config{BaseURL: srv.URL, Model: "model-a", HTTPClient: srv.Client()})
	if requests != 1 || r.Chat.Status != "inconclusive" || r.SSE.Status != "inconclusive" {
		t.Fatalf("模型发现失败后应跳过其余探测: requests=%d report=%+v", requests, r)
	}
}

func TestRunMarksReasoningOnlyChatInconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"","reasoning_content":"thinking"},"finish_reason":"length"}]}`)
		}
	}))
	defer srv.Close()

	r := run(context.Background(), config{BaseURL: srv.URL + "/v1", Model: "model-a", Timeout: time.Second, HTTPClient: srv.Client()})
	if r.Chat.Status != "inconclusive" || r.OK {
		t.Fatalf("reasoning-only chat 不应被报告为可用: %+v", r.Chat)
	}
}
