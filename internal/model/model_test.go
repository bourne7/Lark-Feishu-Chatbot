package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lark-feishu-chatbot/internal/config"
)

// TestSSEEventBoundIncludesEmptyLines prevents unbounded data-field allocation.
func TestSSEEventBoundIncludesEmptyLines(t *testing.T) {
	err := readSSE(io.LimitReader(
		strings.NewReader(strings.Repeat("data:\n", maxEventBytes/6+1)),
		maxEventBytes+6,
	), func(string, string) (bool, error) {
		t.Fatal("oversized event dispatched")
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("missing event bound: %v", err)
	}
}

// TestMultilineSSE preserves JSON split across data lines and ignores heartbeat comments.
func TestMultilineSSE(t *testing.T) {
	body := ": heartbeat\r\nevent: sample\r\ndata: {\r\ndata: \"value\":\"café 🌍\"}\r\n\r\n"
	err := readSSE(strings.NewReader(body), func(event, data string) (bool, error) {
		if event != "sample" || !json.Valid([]byte(data)) || !strings.Contains(data, "café 🌍") {
			t.Fatalf("invalid event %q %q", event, data)
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestProtocols verifies actual wire paths, credentials, history and text events.
func TestProtocols(t *testing.T) {
	for _, tc := range []struct{ protocol, path, stream string }{
		{
			"chat-completions",
			"chat/completions",
			"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"Hello 🌍\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			"responses",
			"responses",
			"event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello 🌍\"}\n\n" +
				"event: response.completed\n" +
				"data: {\"type\":\"response.completed\"}\n\n",
		},
		{
			"anthropic-messages",
			"messages",
			"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\"," +
				"\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello 🌍\"}}\n\n" +
				"event: message_stop\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
		},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/custom/v1/"+tc.path {
					t.Errorf("incorrect path: %s", r.URL.Path)
				}
				if tc.protocol == "anthropic-messages" {
					if r.Header.Get("x-api-key") != "secret" || r.Header.Get("anthropic-version") != "2023-06-01" {
						t.Error("missing Messages credentials")
					}
				} else if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("missing bearer auth")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				encoded, _ := json.Marshal(body)
				if body["model"] != "fixture-model" ||
					body["stream"] != true ||
					!strings.Contains(string(encoded), "previous") {
					t.Errorf("bad request: %s", encoded)
				}
				if tc.protocol == "responses" && body["store"] != false {
					t.Error("response storage must be disabled")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, part := range []string{": keepalive\n\n", tc.stream[:7], tc.stream[7:]} {
					fmt.Fprint(w, part)
					w.(http.Flusher).Flush()
				}
			}))
			defer server.Close()
			client, err := New(config.Model{
				APIType:          tc.protocol,
				BaseURL:          server.URL + "/custom/v1/",
				APIKey:           "secret",
				Name:             "fixture-model",
				MaxOutputTokens:  128,
				AnthropicVersion: "2023-06-01",
				SystemPrompt:     "instructions",
			}, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			err = client.Stream(context.Background(), []Message{
				{Role: "user", Content: "previous"},
				{Role: "assistant", Content: "answer"},
				{Role: "user", Content: "next"},
			}, func(delta string) error {
				text.WriteString(delta)
				return nil
			})
			if err != nil || text.String() != "Hello 🌍" {
				t.Fatalf("text=%q err=%v", text.String(), err)
			}
		})
	}
}

// TestFailures prevents failed, empty, malformed and truncated streams becoming successful history.
func TestFailures(t *testing.T) {
	for _, tc := range []struct {
		kind, body string
		status     int
	}{
		{"chat-completions", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", 200},
		{"chat-completions", "data: [DONE]\n\n", 200},
		{
			"chat-completions",
			"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":" +
				"\"length\"}]}\n\ndata: [DONE]\n\n",
			200,
		},
		{
			"chat-completions",
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
			200,
		},
		{"responses", "data: {\"type\":\"response.completed\"}\n\n", 200},
		{"anthropic-messages", "data: {\"type\":\"message_stop\"}\n\n", 200},
		{
			"anthropic-messages",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}" +
				"\n\ndata: {\"type\":\"message_stop\"}\n\n",
			200,
		},
		{
			"responses",
			"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"secret\"}}}\n\n",
			200,
		},
		{"responses", "data: {\"type\":\"response.incomplete\"}\n\n", 200},
		{"anthropic-messages", "data: {\"type\":\"error\",\"error\":{\"message\":\"secret\"}}\n\n", 200},
		{"chat-completions", "data: garbage\n\n", 200},
		{"chat-completions", "{\"error\":\"secret\"}", 401},
	} {
		t.Run(tc.kind+tc.body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c, err := New(config.Model{
				APIType:         tc.kind,
				BaseURL:         s.URL,
				APIKey:          "secret",
				Name:            "fixture",
				MaxOutputTokens: 20,
			}, s.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = c.Stream(context.Background(), []Message{
				{Role: "user", Content: "hello"},
			}, func(string) error {
				return nil
			})
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe success/error: %v", err)
			}
		})
	}
}

// TestCancellation ensures stalled upstream sockets follow the request context.
func TestCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	c, _ := New(config.Model{
		APIType:         "chat-completions",
		BaseURL:         s.URL,
		APIKey:          "secret",
		Name:            "fixture",
		MaxOutputTokens: 20,
	}, s.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.Stream(ctx, []Message{{Role: "user", Content: "hello"}}, func(string) error {
		return nil
	}); err == nil {
		t.Fatal("cancellation not reported")
	}
}
