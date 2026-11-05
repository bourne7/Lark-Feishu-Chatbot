// Package state owns bounded, process-local conversation and admission data.
package state

import (
	"context"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"lark-feishu-chatbot/internal/config"
	"lark-feishu-chatbot/internal/model"
)

// Admission errors are stable signals for the application layer.
var (
	ErrBusy     = errors.New("this topic already has a request in progress")
	ErrCapacity = errors.New("service state capacity reached; retry later")
	ErrInput    = errors.New("message exceeds the configured history budget")
)

// session retains completed pairs and an active generation cancellation handle.
type session struct {
	history    []model.Message
	expires    time.Time
	generation uint64
	busy       bool
	cancel     context.CancelFunc
}

// Sessions serializes per-topic admission and rejects stale post-clear commits.
type Sessions struct {
	mu       sync.Mutex
	items    map[string]*session
	cfg      config.State
	sequence uint64
}

// NewSessions creates a bounded store using explicit settings.
func NewSessions(cfg config.State) *Sessions {
	return &Sessions{items: make(map[string]*session), cfg: cfg}
}

// Begin reserves a topic and copies history after budgeting the new input.
// [Main Request] 07 Admission: allow one active generation per topic and return copied history trimmed in complete
// pairs.
func (s *Sessions) Begin(key, input string, now time.Time, cancel context.CancelFunc) (uint64, []model.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if utf8.RuneCountInString(input) > s.cfg.HistoryMaxRunes {
		return 0, nil, ErrInput
	}
	for key, item := range s.items {
		if !item.busy && !item.expires.After(now) {
			delete(s.items, key)
		}
	}
	item := s.items[key]
	if item != nil && item.busy {
		return 0, nil, ErrBusy
	}
	if item == nil {
		if len(s.items) >= s.cfg.SessionMaxCount {
			var oldestKey string
			var oldest time.Time
			for k, v := range s.items {
				if !v.busy && (oldest.IsZero() || v.expires.Before(oldest)) {
					oldestKey, oldest = k, v.expires
				}
			}
			if oldest.IsZero() {
				return 0, nil, ErrCapacity
			}
			delete(s.items, oldestKey)
		}
		item = &session{expires: now.Add(s.cfg.SessionTTL)}
		s.items[key] = item
	}
	s.sequence++
	item.generation = s.sequence
	item.busy = true
	item.cancel = cancel
	history := append([]model.Message(nil), item.history...)
	for len(history) > 0 &&
		(len(history)+2 > s.cfg.HistoryMaxMessages ||
			runeCount(history)+utf8.RuneCountInString(input) > s.cfg.HistoryMaxRunes) {
		history = history[2:]
	}
	return item.generation, history, nil
}

// Commit stores complete successful pairs only if the generation is still current.
// [Main Request] 12 Finalize: cleared or replaced generations cannot restore stale context; retain complete pairs only.
func (s *Sessions) Commit(key string, generation uint64, input, answer string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[key]
	if item == nil || item.generation != generation || !item.busy {
		return false
	}
	history := append(append([]model.Message(nil), item.history...), model.Message{
		Role:    "user",
		Content: input,
	}, model.Message{
		Role:    "assistant",
		Content: answer,
	})
	for len(history) > 0 && (len(history) > s.cfg.HistoryMaxMessages || runeCount(history) > s.cfg.HistoryMaxRunes) {
		history = history[2:]
	}
	item.history = history
	item.expires = now.Add(s.cfg.SessionTTL)
	return true
}

// End releases only the generation that originally acquired the topic.
// [Main Request] 08 Worker: deferred cleanup releases this worker's topic reservation without releasing a newer
// generation.
func (s *Sessions) End(key string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item := s.items[key]; item != nil && item.generation == generation {
		item.busy = false
		item.cancel = nil
	}
}

// Clear removes history before canceling work, preventing late results from reviving it.
// [Main Request] 07 Admission: /clear deletes the topic before canceling its active worker so late commits cannot
// revive history.
func (s *Sessions) Clear(key string) {
	s.mu.Lock()
	var cancel context.CancelFunc
	if item := s.items[key]; item != nil {
		cancel = item.cancel
	}
	delete(s.items, key)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// runeCount measures a portable text budget without claiming model-specific token accuracy.
func runeCount(messages []model.Message) int {
	total := 0
	for _, m := range messages {
		total += utf8.RuneCountInString(m.Content)
	}
	return total
}
