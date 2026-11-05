package state

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lark-feishu-chatbot/internal/config"
)

// stateConfig keeps tests independent of deployment samples.
func stateConfig() config.State {
	return config.State{
		SessionTTL:         time.Hour,
		SessionMaxCount:    2,
		HistoryMaxMessages: 4,
		HistoryMaxRunes:    40,
		DedupeTTL:          time.Minute,
		DedupeMaxCount:     2,
	}
}

// TestSessions checks isolated copies, successful pairs, expiry and clear generations.
func TestSessions(t *testing.T) {
	s := NewSessions(stateConfig())
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token, history, err := s.Begin("topic", "hello", now, cancel)
	if err != nil || len(history) != 0 {
		t.Fatal(err)
	}
	if _, _, err := s.Begin("topic", "other", now, func() {}); err != ErrBusy {
		t.Fatalf("busy=%v", err)
	}
	if !s.Commit("topic", token, "hello", "world", now) {
		t.Fatal("commit failed")
	}
	s.End("topic", token)
	token, history, err = s.Begin("topic", "next", now, cancel)
	if err != nil || len(history) != 2 {
		t.Fatalf("history=%v err=%v", history, err)
	}
	history[0].Content = "mutated"
	s.Clear("topic")
	if ctx.Err() == nil {
		t.Fatal("clear did not cancel")
	}
	if s.Commit("topic", token, "old", "answer", now) {
		t.Fatal("cleared generation committed")
	}
	s.End("topic", token)
	token, history, err = s.Begin("topic", "fresh", now, func() {})
	if err != nil || len(history) != 0 {
		t.Fatal("clear leaked history")
	}
	s.Commit("topic", token, "fresh", "answer", now)
	s.End("topic", token)
	_, history, err = s.Begin("topic", "expired", now.Add(2*time.Hour), func() {})
	if err != nil || len(history) != 0 {
		t.Fatal("expiry leaked history")
	}
}

// TestSessionBounds rejects oversized input and never evicts active topics.
func TestSessionBounds(t *testing.T) {
	s := NewSessions(stateConfig())
	now := time.Now()
	if _, _, err := s.Begin("bad", string(make([]rune, 41)), now, func() {}); err == nil {
		t.Fatal("oversized input accepted")
	}
	a, _, _ := s.Begin("a", "a", now, func() {})
	s.Begin("b", "b", now, func() {})
	if _, _, err := s.Begin("c", "c", now, func() {}); err != ErrCapacity {
		t.Fatal(err)
	}
	s.Commit("a", a, "a", "answer", now)
	s.End("a", a)
	if _, _, err := s.Begin("c", "c", now, func() {}); err != nil {
		t.Fatal(err)
	}
}

// TestDedupClaimIsAtomic proves duplicate concurrent deliveries admit one worker.
func TestDedupClaimIsAtomic(t *testing.T) {
	d := NewDedup(time.Minute, 2)
	now := time.Now()
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			ok, err := d.Claim("same", now)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatal(admitted.Load())
	}
	d.Claim("second", now)
	if _, err := d.Claim("third", now); err != ErrCapacity {
		t.Fatal(err)
	}
	if ok, err := d.Claim("same", now.Add(2*time.Minute)); !ok || err != nil {
		t.Fatalf("expiry %v %v", ok, err)
	}
}

// TestLimiter enforces a rolling minute, UTC daily reset and bounded user cardinality.
func TestLimiter(t *testing.T) {
	l := NewLimiter(config.Rate{Enabled: true, PerMinute: 2, PerDay: 3, MaxUsers: 1})
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if !l.Allow("u", now) || !l.Allow("u", now.Add(time.Second)) || l.Allow("u", now.Add(2*time.Second)) {
		t.Fatal("minute quota failed")
	}
	if !l.Allow("u", now.Add(time.Minute+2*time.Second)) || l.Allow("u", now.Add(3*time.Minute)) {
		t.Fatal("daily quota failed")
	}
	if l.Allow("other", now) {
		t.Fatal("capacity failed")
	}
	if !l.Allow("u", now.Add(24*time.Hour)) {
		t.Fatal("daily reset failed")
	}
	if !NewLimiter(config.Rate{MaxUsers: 1}).Allow("disabled", now) {
		t.Fatal("disabled quota rejected")
	}
}

// TestMinuteOnlyLimiterReclaimsIdleUsers prevents a full map rejecting new users all day.
func TestMinuteOnlyLimiterReclaimsIdleUsers(t *testing.T) {
	l := NewLimiter(config.Rate{Enabled: true, PerMinute: 2, MaxUsers: 1})
	now := time.Now()
	if !l.Allow("first", now) || !l.Allow("second", now.Add(2*time.Minute)) {
		t.Fatal("inactive minute-only user was not reclaimed")
	}
}

// TestAccessControlSwitch bypasses configured quotas and user tracking when disabled.
func TestAccessControlSwitch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		l := NewLimiter(config.Rate{Enabled: enabled, PerMinute: 1, PerDay: 1, MaxUsers: 1})
		now := time.Now()
		if !l.Allow("user", now) {
			t.Fatal("first request rejected")
		}
		if allowed := l.Allow("user", now); allowed == enabled {
			t.Fatalf("enabled=%v allowed=%v", enabled, allowed)
		}
		if !enabled && !l.Allow("another", now) {
			t.Fatal("disabled access control still tracks users")
		}
	}
}

// TestHistoryTrimsWholePairs preserves role ordering when either history budget is reached.
func TestHistoryTrimsWholePairs(t *testing.T) {
	cfg := stateConfig()
	cfg.HistoryMaxMessages = 4
	cfg.HistoryMaxRunes = 12
	s := NewSessions(cfg)
	now := time.Now()
	for _, input := range []string{"one", "two", "tri"} {
		token, _, err := s.Begin("t", input, now, func() {})
		if err != nil {
			t.Fatal(err)
		}
		s.Commit("t", token, input, "ok", now)
		s.End("t", token)
	}
	_, history, err := s.Begin("t", "end", now, func() {})
	if err != nil ||
		len(history) != 2 ||
		history[0].Role != "user" ||
		history[0].Content != "tri" ||
		history[1].Role != "assistant" {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}
