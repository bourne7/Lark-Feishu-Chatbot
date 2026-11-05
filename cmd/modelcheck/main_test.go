package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lark-feishu-chatbot/internal/model"
)

// diagnosticStreamFunc injects completion and interruption at the model boundary.
type diagnosticStreamFunc func(context.Context, []model.Message, func(string) error) error

func (f diagnosticStreamFunc) Stream(ctx context.Context, messages []model.Message, emit func(string) error) error {
	return f(ctx, messages, emit)
}

// TestDiagnosticLogsOneAnswerPerTurn catches per-chunk logging and lost partial answers.
func TestDiagnosticLogsOneAnswerPerTurn(t *testing.T) {
	for _, mode := range []string{"success", "interrupted", "invalid-context"} {
		t.Run(mode, func(t *testing.T) {
			var stdout bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&stdout, nil))
			inputs := []string{}
			answers := []string{}
			provider := diagnosticStreamFunc(func(
				_ context.Context,
				history []model.Message,
				emit func(string) error,
			) error {
				inputs = append(inputs, history[len(history)-1].Content)
				first := history[0].Content
				marker := strings.Fields(first[strings.Index(first, "lark-feishu-chatbot-"):])[0]
				answer := "Answer 🌍 " + marker
				if mode == "invalid-context" {
					answer = "Answer missing marker 🌍"
				}
				answers = append(answers, answer)
				if err := emit("Answer "); err != nil {
					return err
				}
				if mode == "interrupted" {
					answers[len(answers)-1] = "Answer 🌍 partial"
					if err := emit("🌍 partial"); err != nil {
						return err
					}
					return context.Canceled
				}
				// Use several deltas so the log must aggregate rather than mirror writes.
				return emit(strings.TrimPrefix(answer, "Answer "))
			})
			err := check(context.Background(), provider, logger, true)
			if mode == "success" && err != nil {
				t.Fatal(err)
			}
			if mode == "interrupted" && !errors.Is(err, context.Canceled) {
				t.Fatalf("interruption lost: %v", err)
			}
			if mode == "invalid-context" && err == nil {
				t.Fatal("missing context accepted")
			}
			inputCount, outputCount := 0, 0
			for _, line := range bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n")) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if input, ok := record["input"].(string); ok {
					if inputCount >= len(inputs) || input != inputs[inputCount] {
						t.Fatalf("duplicate or incorrect input: %s", line)
					}
					inputCount++
				}
				if output, ok := record["output"].(string); ok {
					if outputCount >= len(answers) || output != answers[outputCount] {
						t.Fatalf("fragmented or incorrect output: %s", line)
					}
					if record["complete"] != (mode != "interrupted") {
						t.Fatalf("incorrect completion status: %s", line)
					}
					outputCount++
				}
			}
			if inputCount != len(inputs) || outputCount != len(answers) {
				t.Fatalf("lost conversation records: inputs=%d outputs=%d", inputCount, outputCount)
			}
		})
	}
}

// diagnosticModel confirms the diagnostic replays both earlier turns.
type diagnosticModel struct {
	calls int
	t     *testing.T
}

// TestRunMirrorsDiagnosticAndErrors catches missing file output and errors logged after close.
func TestRunMirrorsDiagnosticAndErrors(t *testing.T) {
	for _, mode := range []string{
		"success",
		"provider-error",
		"network-error",
		"interrupted",
		"invalid-config",
		"content-disabled",
	} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "provider-error" {
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"error":"secret-test-key"}`)
					return
				}
				var request struct {
					Messages []model.Message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
					t.Error("invalid diagnostic request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				first := request.Messages[0].Content
				start := strings.Index(first, "lark-feishu-chatbot-")
				if start < 0 {
					t.Error("missing marker in request")
					return
				}
				marker := strings.Fields(first[start:])[0]
				data, _ := json.Marshal(map[string]any{
					"choices": []any{
						map[string]any{
							"delta":         map[string]string{"content": marker + " 🌍"},
							"finish_reason": "stop",
						},
					},
				})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", data)
				if mode != "interrupted" {
					fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "logs", "diagnostic.log")
			for key, value := range map[string]string{
				"LLM_API_TYPE":          "chat-completions",
				"LLM_BASE_URL":          server.URL,
				"LLM_API_KEY":           "secret-test-key",
				"LLM_MODEL":             "fixture-model",
				"LLM_MAX_OUTPUT_TOKENS": "128",
				"LLM_TEMPERATURE":       "",
				"LLM_PROXY_URL":         "",
				"LLM_SYSTEM_PROMPT":     "",
				"REQUEST_TIMEOUT":       "5s",
				"ENABLE_LOG":            "true",
				"LOG_FILE":              path,
				"LOG_LEVEL":             "info",
				"LOG_CONTENT":           "true",
			} {
				t.Setenv(key, value)
			}
			if mode == "invalid-config" {
				t.Setenv("LLM_MODEL", "")
			}
			if mode == "content-disabled" {
				t.Setenv("LOG_CONTENT", "false")
			}
			if mode == "network-error" {
				server.Close()
			}
			var stdout bytes.Buffer
			err := run(&stdout)
			wantError := mode == "provider-error" ||
				mode == "invalid-config" ||
				mode == "network-error" ||
				mode == "interrupted"
			if (err != nil) != wantError {
				t.Fatalf("unexpected result: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, stdout.Bytes()) || len(data) == 0 {
				t.Fatalf("terminal and file differ: terminal=%q file=%q", stdout.Bytes(), data)
			}
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
				if !json.Valid(line) {
					t.Fatalf("invalid JSON log: %s", line)
				}
			}
			if bytes.Contains(data, []byte("secret-test-key")) {
				t.Fatal("credential leaked into logs")
			}
			if wantError && !bytes.Contains(data, []byte(`"level":"ERROR"`)) {
				t.Fatal("failure missing from file log")
			}
			if mode == "network-error" && !bytes.Contains(data, []byte("model connection failed")) {
				t.Fatalf("network reason missing: %s", data)
			}
			if mode == "interrupted" &&
				(!bytes.Contains(data, []byte("🌍")) ||
					bytes.Count(data, []byte(`"msg":"diagnostic output"`)) != 1 ||
					!bytes.Contains(data, []byte(`"complete":false`))) {
				t.Fatalf("interrupted answer missing or fragmented: %s", data)
			}
			if mode == "success" &&
				(!bytes.Contains(data, []byte("Remember this marker:")) ||
					!bytes.Contains(data, []byte("🌍")) ||
					!bytes.Contains(data, []byte("Turn 2: passed"))) {
				t.Fatalf("diagnostic content missing: %s", data)
			}
			if mode == "content-disabled" &&
				(bytes.Contains(data, []byte("Remember this marker:")) ||
					bytes.Contains(data, []byte("🌍"))) {
				t.Fatal("disabled content logging leaked conversation text")
			}
		})
	}
}

func (m *diagnosticModel) Stream(_ context.Context, history []model.Message, emit func(string) error) error {
	m.calls++
	if m.calls == 2 && len(history) != 3 {
		m.t.Fatalf("missing replay: %+v", history)
	}
	first := history[0].Content
	start := strings.Index(first, "lark-feishu-chatbot-")
	if start < 0 {
		return fmt.Errorf("missing diagnostic marker")
	}
	marker := strings.Fields(first[start:])[0]
	return emit("```text\n" + marker + "\n```")
}

// TestDiagnostic verifies bounded two-turn behavior without a real model endpoint.
func TestDiagnostic(t *testing.T) {
	p := &diagnosticModel{t: t}
	if err := check(context.Background(), p, slog.New(slog.NewJSONHandler(io.Discard, nil)), true); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatal(p.calls)
	}
}
