package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lark-feishu-chatbot/internal/model"
)

// run owns all card mutations while upstream text accumulates in a bounded buffer.
// [Main Request] 09 Generation: coordinate model text, idle/request cancellation and serialized updates to the
// already-created card.
func (s *Service) run(parent context.Context, out Output, r Request, generation uint64, history []model.Message) {
	started := time.Now()
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(context.Canceled)
	var mu sync.Mutex
	var text strings.Builder
	runes := 0
	lastVisible := time.Now()
	finished := false
	errIdle := errors.New("stream idle timeout")
	// [Main Request] 09 Generation: cancel stalled streams; only visible text renews this independent idle deadline.
	var idle *time.Timer
	idle = time.AfterFunc(s.cfg.Runtime.IdleTimeout, func() {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		if remaining := s.cfg.Runtime.IdleTimeout - time.Since(lastVisible); remaining > 0 {
			idle.Reset(remaining)
		} else {
			cancel(errIdle)
		}
	})
	stopIdle := func() { mu.Lock(); finished = true; idle.Stop(); mu.Unlock() }
	defer stopIdle()
	snapshot := func() string { mu.Lock(); defer mu.Unlock(); return text.String() }
	result := make(chan error, 1)
	// [Main Request] 10 Model: Stream emits deltas into a bounded buffer; this producer never mutates Feishu cards.
	go func() {
		result <- s.provider.Stream(ctx, history, func(delta string) error {
			mu.Lock()
			defer mu.Unlock()
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			n := utf8.RuneCountInString(delta)
			// The explicit history rune budget also bounds retained generation output.
			if runes+n > s.cfg.State.HistoryMaxRunes {
				return errors.New("answer exceeds configured text budget")
			}
			text.WriteString(delta)
			runes += n
			if strings.TrimSpace(delta) != "" {
				lastVisible = time.Now()
				idle.Reset(s.cfg.Runtime.IdleTimeout)
			}
			return nil
		})
	}()
	tick := time.NewTicker(s.cfg.Card.UpdateInterval)
	defer tick.Stop()
	var failure error
	last := ""
	// [Main Request] 11 Cards: this worker alone sends progressive snapshots on a timer and handles terminal
	// completion/cancellation.
	for {
		select {
		case failure = <-result:
			result = nil
			goto complete
		case <-ctx.Done():
			failure = context.Cause(ctx)
			goto complete
		case <-tick.C:
			current := snapshot()
			if current == last {
				continue
			}
			writeCtx, stop := context.WithTimeout(ctx, s.cfg.WriteTimeout)
			err := out.Update(writeCtx, current, "Generating")
			stop()
			if err != nil {
				failure = errors.New("card update failed")
				goto complete
			}
			last = current
		}
	}
complete:
	// [Main Request] 12 Finalize: stop timers, cancel upstream and wait for its exit before reading the
	// final/partial answer.
	stopIdle()
	if ctx.Err() != nil {
		failure = context.Cause(ctx)
	}
	cancel(context.Canceled)
	// Supported adapters honor cancellation; retain the slot until the producer exits.
	if result != nil {
		<-result
	}
	answer := snapshot()
	if failure == nil && strings.TrimSpace(answer) == "" {
		failure = errors.New("model completed without visible text")
	}
	status := "Complete"
	if failure != nil {
		status = "Failed: " + failure.Error()
	}
	// [Main Request] 12 Finalize: publish final text/status; only successful generation AND delivery may commit
	// history.
	writeErr := s.final(out, answer, status)
	if writeErr == nil && failure == nil {
		s.sessions.Commit(r.Topic, generation, r.Text, answer, time.Now())
	}
	s.log.Info(
		"generation finished",
		"message_id", r.ID,
		"duration", time.Since(started),
		"success", failure == nil && writeErr == nil,
	)
	// [Main Request] 12 Finalize: log the aggregate answer once, preserving partial text; failures remain separate
	// system logs.
	if s.cfg.LogContent {
		s.log.Info(
			"conversation output",
			"message_id", r.ID,
			"topic", r.Topic,
			"output", answer,
			"status", status,
			"complete", failure == nil && writeErr == nil,
		)
	}
	if failure != nil {
		s.log.Error("generation failed", "message_id", r.ID, "topic", r.Topic, "error", failure)
	}
}

// final uses a fresh bounded context so timeout notices can still reach the user.
// [Main Request] 12 Finalize: a fresh service-derived write deadline allows a final update after the generation
// deadline expires.
func (s *Service) final(out Output, text, status string) error {
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.WriteTimeout)
	defer cancel()
	err := out.Update(ctx, text, status)
	if err != nil {
		s.log.Error("final card update failed", "error", err)
	}
	return err
}
