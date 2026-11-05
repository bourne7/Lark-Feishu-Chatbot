package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lark-feishu-chatbot/internal/config"
	"lark-feishu-chatbot/internal/logging"
	"lark-feishu-chatbot/internal/model"
)

// TestGenerationLogsContentOnceAndErrorsIndependently catches duplicated input and hidden failures.
func TestGenerationLogsContentOnceAndErrorsIndependently(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, mode := range []string{"success", "network-error", "canceled"} {
			t.Run(fmt.Sprintf("%v/%s", enabled, mode), func(t *testing.T) {
				var stdout bytes.Buffer
				cfg := testConfig()
				cfg.LogContent = enabled
				provider := streamFunc(func(_ context.Context, _ []model.Message, emit func(string) error) error {
					if err := emit("Answer "); err != nil {
						return err
					}
					if err := emit("🌍"); err != nil {
						return err
					}
					if mode == "network-error" {
						return errors.New("model connection failed")
					}
					if mode == "canceled" {
						return context.Canceled
					}
					return nil
				})
				sink := &captureSink{finals: make(chan string, 10)}
				service := New(context.Background(), cfg, provider, sink, slog.New(slog.NewJSONHandler(&stdout, nil)))
				defer service.Shutdown(context.Background())
				if err := service.Submit(Request{
					ID:    "request",
					Topic: "topic",
					User:  "user",
					Text:  "Question 🌍",
				}); err != nil {
					t.Fatal(err)
				}
				awaitFinal(t, sink)
				service.wg.Wait()
				inputs, outputs, failures := 0, 0, 0
				for _, line := range bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n")) {
					var record map[string]any
					if err := json.Unmarshal(line, &record); err != nil {
						t.Fatal(err)
					}
					if input, ok := record["input"]; ok {
						inputs++
						if input != "Question 🌍" {
							t.Fatalf("incorrect input: %s", line)
						}
					}
					if output, ok := record["output"]; ok {
						outputs++
						if output != "Answer 🌍" {
							t.Fatalf("fragmented output: %s", line)
						}
					}
					if record["level"] == "ERROR" {
						failures++
						if record["error"] == nil {
							t.Fatalf("failure has no reason: %s", line)
						}
					}
				}
				wantContent := 0
				if enabled {
					wantContent = 1
				}
				if inputs != wantContent || outputs != wantContent {
					t.Fatalf("conversation records: inputs=%d outputs=%d, want %d each", inputs, outputs, wantContent)
				}
				if (failures > 0) != (mode != "success") {
					t.Fatalf("system errors missing or spurious: %s", stdout.Bytes())
				}
			})
		}
	}
}

// blockedLogWriter exposes a slow destination without creating unbounded background work.
type blockedLogWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

// TestContentLoggingDoesNotBlockAdmission keeps terminal I/O outside the admission mutex.
func TestContentLoggingDoesNotBlockAdmission(t *testing.T) {
	writer := &blockedLogWriter{started: make(chan struct{}), release: make(chan struct{})}
	cfg := testConfig()
	cfg.LogContent = true
	provider := streamFunc(func(_ context.Context, _ []model.Message, emit func(string) error) error {
		return emit("answer")
	})
	service := New(context.Background(), cfg, provider, &captureSink{
		finals: make(chan string, 10),
	}, slog.New(slog.NewJSONHandler(writer, nil)))
	defer service.Shutdown(context.Background())
	defer close(writer.release)
	admitted := make(chan error, 1)
	go func() {
		admitted <- service.Submit(Request{ID: "1", Topic: "topic", User: "user", Text: "question"})
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("content log did not start")
	}
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow content logger blocked admission")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- service.Shutdown(ctx) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("slow content logger blocked shutdown deadline")
	}
}

// TestContentLogsMirrorInputsAndReplies covers model replies, local commands and opt-out.
func TestContentLogsMirrorInputsAndReplies(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			var stdout bytes.Buffer
			path := filepath.Join(t.TempDir(), "chatbot.log")
			logger, closeLog, err := logging.New(slog.LevelInfo, true, path, &stdout)
			if err != nil {
				t.Fatal(err)
			}
			defer closeLog()
			cfg := testConfig()
			cfg.LogContent = enabled
			provider := streamFunc(func(_ context.Context, _ []model.Message, emit func(string) error) error {
				return emit("Answer 🌍")
			})
			sink := &captureSink{finals: make(chan string, 10)}
			service := New(context.Background(), cfg, provider, sink, logger)
			defer service.Shutdown(context.Background())
			for i, input := range []string{"Question 🌍", "/help", "/clear"} {
				if err := service.Submit(Request{
					ID:    fmt.Sprint(i),
					Topic: "topic",
					User:  "user",
					Text:  input,
				}); err != nil {
					t.Fatal(err)
				}
				awaitFinal(t, sink)
				service.wg.Wait()
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, stdout.Bytes()) {
				t.Fatal("terminal and file logs differ")
			}
			for _, text := range []string{
				"Question 🌍",
				"Answer 🌍",
				"Show all commands and usage",
				"History cleared",
			} {
				if bytes.Contains(data, []byte(text)) != enabled {
					t.Fatalf("content logging=%v, unexpected presence of %q: %s", enabled, text, data)
				}
			}
		})
	}
}

// TestFailedPartialIsNotReplayed verifies that failures never become successful context.
func TestFailedPartialIsNotReplayed(t *testing.T) {
	calls := 0
	p := streamFunc(func(_ context.Context, m []model.Message, emit func(string) error) error {
		calls++
		if len(m) != 1 {
			t.Fatalf("failed history replayed: %+v", m)
		}
		emit("partial")
		if calls == 1 {
			return errors.New("fixture failure")
		}
		return nil
	})
	sink := &captureSink{finals: make(chan string, 10)}
	s := New(context.Background(), testConfig(), p, sink, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer s.Shutdown(context.Background())
	for _, id := range []string{"1", "2"} {
		if err := s.Submit(Request{ID: id, Topic: "t", User: "u", Text: "hello"}); err != nil {
			t.Fatal(err)
		}
		awaitFinal(t, sink)
		s.wg.Wait()
	}
}

// TestGlobalCapacityAndShutdown bounds work and rejects admission after shutdown.
func TestGlobalCapacityAndShutdown(t *testing.T) {
	started := make(chan struct{})
	p := streamFunc(func(ctx context.Context, _ []model.Message, _ func(string) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	cfg := testConfig()
	cfg.Runtime.MaxConcurrent = 1
	sink := &captureSink{finals: make(chan string, 10)}
	s := New(context.Background(), cfg, p, sink, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	r := Request{ID: "1", Topic: "one", User: "u", Text: "hello"}
	if err := s.Submit(r); err != nil {
		t.Fatal(err)
	}
	<-started
	r.ID = "2"
	r.Topic = "two"
	if err := s.Submit(r); err == nil {
		t.Fatal("global limit ignored")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(r); err == nil {
		t.Fatal("closed service admitted work")
	}
}

// streamFunc exposes controllable model behavior without a network dependency.
type streamFunc func(context.Context, []model.Message, func(string) error) error

func (f streamFunc) Stream(ctx context.Context, m []model.Message, emit func(string) error) error {
	return f(ctx, m, emit)
}

// captureSink records final status and model history across background requests.
type captureSink struct {
	mu      sync.Mutex
	finals  chan string
	updates []string
}

func (s *captureSink) Open(context.Context, string) (Output, error) { return s, nil }
func (s *captureSink) Update(_ context.Context, text, status string) error {
	s.mu.Lock()
	s.updates = append(s.updates, text)
	s.mu.Unlock()
	if status != "Generating" {
		s.finals <- status
	}
	return nil
}

// testConfig supplies all runtime budgets explicitly.
func testConfig() config.Config {
	return config.Config{
		WriteTimeout: time.Second,
		Runtime: config.Runtime{
			RequestTimeout: time.Second,
			IdleTimeout:    200 * time.Millisecond,
			MaxConcurrent:  2,
		},
		State: config.State{
			SessionTTL:         time.Hour,
			SessionMaxCount:    10,
			HistoryMaxMessages: 10,
			HistoryMaxRunes:    1000,
			DedupeTTL:          time.Minute,
			DedupeMaxCount:     100,
		},
		Rate: config.Rate{MaxUsers: 10},
		Card: config.Card{UpdateInterval: 10 * time.Millisecond, MaxRunes: 6000},
	}
}

// awaitFinal bounds tests while waiting for actual asynchronous terminal behavior.
func awaitFinal(t *testing.T, sink *captureSink) string {
	t.Helper()
	select {
	case status := <-sink.finals:
		return status
	case <-time.After(2 * time.Second):
		t.Fatal("worker never finalized")
		return ""
	}
}

// TestHistoryAndClear confirms completed history replay and an empty post-clear conversation.
func TestHistoryAndClear(t *testing.T) {
	var mu sync.Mutex
	var lengths []int
	provider := streamFunc(func(ctx context.Context, m []model.Message, emit func(string) error) error {
		mu.Lock()
		lengths = append(lengths, len(m))
		mu.Unlock()
		return emit("answer")
	})
	sink := &captureSink{finals: make(chan string, 10)}
	service := New(context.Background(), testConfig(), provider, sink, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer service.Shutdown(context.Background())
	for _, r := range []Request{
		{ID: "1", Topic: "t", User: "u", Text: "first"},
		{ID: "2", Topic: "t", User: "u", Text: "next"},
		{ID: "3", Topic: "t", User: "u", Text: "/clear"},
		{ID: "4", Topic: "t", User: "u", Text: "fresh"},
	} {
		if err := service.Submit(r); err != nil {
			t.Fatal(err)
		}
		awaitFinal(t, sink)
		service.wg.Wait()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lengths) != 3 || lengths[0] != 1 || lengths[1] != 3 || lengths[2] != 1 {
		t.Fatal(lengths)
	}
}

// TestHelpPreservesHistoryAndQuota catches help being sent to the model or consuming a question.
func TestHelpPreservesHistoryAndQuota(t *testing.T) {
	var histories [][]model.Message
	provider := streamFunc(func(_ context.Context, messages []model.Message, emit func(string) error) error {
		histories = append(histories, append([]model.Message(nil), messages...))
		return emit("answer")
	})
	cfg := testConfig()
	cfg.Rate.Enabled = true
	cfg.Rate.PerDay = 2
	sink := &captureSink{finals: make(chan string, 10)}
	service := New(context.Background(), cfg, provider, sink, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer service.Shutdown(context.Background())
	for _, request := range []Request{
		{ID: "first", Topic: "topic", User: "user", Text: "first question"},
		{ID: "help", Topic: "topic", User: "user", Text: " /help "},
		{ID: "followup", Topic: "topic", User: "user", Text: "follow-up question"},
	} {
		if err := service.Submit(request); err != nil {
			t.Fatal(err)
		}
		if status := awaitFinal(t, sink); status != "Complete" {
			t.Fatalf("%s: %s", request.ID, status)
		}
		service.wg.Wait()
		if request.ID == "help" {
			sink.mu.Lock()
			help := sink.updates[len(sink.updates)-1]
			sink.mu.Unlock()
			for _, command := range []string{"/help", "/clear"} {
				if !strings.Contains(help, command) {
					t.Errorf("help does not explain %s: %s", command, help)
				}
			}
		}
	}
	if len(histories) != 2 || len(histories[0]) != 1 || len(histories[1]) != 3 {
		t.Fatalf("help changed model history or consumed the question quota: %+v", histories)
	}
	if histories[1][0].Content != "first question" ||
		histories[1][1].Content != "answer" ||
		histories[1][2].Content != "follow-up question" {
		t.Fatalf("help changed the conversation: %+v", histories[1])
	}
}

// TestIdleAndClearCancelUpstream prevents canceled generations entering future history.
func TestIdleAndClearCancelUpstream(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(string(rune('0'+map[bool]int{false: 0, true: 1}[clear])), func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			provider := streamFunc(func(ctx context.Context, m []model.Message, emit func(string) error) error {
				close(started)
				emit("partial")
				<-ctx.Done()
				close(canceled)
				return ctx.Err()
			})
			cfg := testConfig()
			cfg.Runtime.IdleTimeout = 30 * time.Millisecond
			sink := &captureSink{finals: make(chan string, 10)}
			service := New(context.Background(), cfg, provider, sink, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			defer service.Shutdown(context.Background())
			if err := service.Submit(Request{ID: "a", Topic: "t", User: "u", Text: "ask"}); err != nil {
				t.Fatal(err)
			}
			<-started
			if clear {
				service.Submit(Request{ID: "b", Topic: "t", User: "u", Text: "/clear"})
			}
			status := awaitFinal(t, sink)
			if !clear && !strings.Contains(status, "idle") {
				t.Fatal(status)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("upstream not canceled")
			}
		})
	}
}

// TestPromptAcknowledgementAndDedup ensures Submit does not wait for the model.
func TestPromptAcknowledgementAndDedup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	p := streamFunc(func(ctx context.Context, m []model.Message, emit func(string) error) error {
		mu.Lock()
		calls++
		mu.Unlock()
		close(started)
		select {
		case <-release:
			return emit("done")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	sink := &captureSink{finals: make(chan string, 10)}
	s := New(context.Background(), testConfig(), p, sink, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer s.Shutdown(context.Background())
	r := Request{ID: "same", Topic: "topic", User: "u", Text: "hello"}
	if err := s.Submit(r); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := s.Submit(r); err != nil {
		t.Fatal(err)
	}
	close(release)
	awaitFinal(t, sink)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatal(calls)
	}
}

// blockedOutput models a card API call that only returns when its context is canceled.
type blockedOutput struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

// TestTotalDeadlineWithContinuousText prevents active streams from bypassing the total limit.
func TestTotalDeadlineWithContinuousText(t *testing.T) {
	cfg := testConfig()
	cfg.Runtime.RequestTimeout = 50 * time.Millisecond
	cfg.Runtime.IdleTimeout = 200 * time.Millisecond
	p := streamFunc(func(ctx context.Context, _ []model.Message, emit func(string) error) error {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if err := emit("x"); err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	sink := &captureSink{finals: make(chan string, 10)}
	s := New(context.Background(), cfg, p, sink, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer s.Shutdown(context.Background())
	s.Submit(Request{ID: "1", Topic: "t", User: "u", Text: "hello"})
	if status := awaitFinal(t, sink); !strings.Contains(status, "deadline") {
		t.Fatal(status)
	}
}

func (o *blockedOutput) Open(context.Context, string) (Output, error) { return o, nil }
func (o *blockedOutput) Update(ctx context.Context, _ string, status string) error {
	if status != "Generating" {
		return nil
	}
	o.once.Do(func() { close(o.started) })
	<-ctx.Done()
	close(o.canceled)
	return ctx.Err()
}

// TestIdleCancelsBlockedCardWrite enforces the idle deadline during outbound presentation calls.
func TestIdleCancelsBlockedCardWrite(t *testing.T) {
	cfg := testConfig()
	cfg.Runtime.IdleTimeout = 40 * time.Millisecond
	cfg.WriteTimeout = time.Second
	sink := &blockedOutput{started: make(chan struct{}), canceled: make(chan struct{})}
	p := streamFunc(func(ctx context.Context, _ []model.Message, emit func(string) error) error {
		if err := emit("partial"); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	s := New(context.Background(), cfg, p, sink, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer s.Shutdown(context.Background())
	s.Submit(Request{ID: "1", Topic: "t", User: "u", Text: "hello"})
	<-sink.started
	select {
	case <-sink.canceled:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("idle deadline did not cancel blocked card write")
	}
}
