// Package chat coordinates bounded background conversations and serialized output.
package chat

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"lark-feishu-chatbot/internal/config"
	"lark-feishu-chatbot/internal/model"
	"lark-feishu-chatbot/internal/state"
)

// helpText lists every local command and the conversation behavior available to users.
const helpText = "## Commands\n" +
	"- /help: Show all commands and usage. Keeps the current history and does not count as a model question.\n" +
	"- /clear: Clear this topic's history and cancel its active answer. Reply within the topic you want to clear.\n" +
	"\n" +
	"## Chat\n" +
	"- Send a text message in a private chat, or @mention the bot with your question in a group.\n" +
	"- Reply within the same topic to continue with context. " +
	"In groups, @mention the bot on every follow-up and command.\n" +
	"- Send a new message outside an existing topic to start a separate conversation.\n" +
	"- Read answers as they stream into Markdown cards with lists, links and fenced code blocks. " +
	"Long answers continue in additional cards.\n" +
	"- Inactive history expires after the configured retention period. " +
	"A service restart also clears history, and older messages may be trimmed.\n" +
	"- Wait for an answer to finish before asking again in the same topic. " +
	"Rate limits may apply; stalled or overlong requests stop automatically."

// Output is owned by one worker for its entire lifetime.
type Output interface {
	Update(context.Context, string, string) error
}

// Sink creates a response attached to the original incoming message.
type Sink interface {
	Open(context.Context, string) (Output, error)
}

// Request carries validated platform-independent message identifiers.
type Request struct{ ID, Topic, User, Text string }

// Service owns admission, process-local state and cancellation of background work.
type Service struct {
	ctx      context.Context
	cancel   context.CancelFunc
	cfg      config.Config
	provider model.Streamer
	sink     Sink
	log      *slog.Logger
	sessions *state.Sessions
	dedup    *state.Dedup
	limiter  *state.Limiter
	slots    chan struct{}
	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
}

// New constructs a service whose workers outlive individual HTTP callbacks.
// [Main Request] 03 Bootstrap: chat owns bounded history, deduplication, quotas, worker slots and generation
// cancellation.
func New(ctx context.Context, cfg config.Config, provider model.Streamer, sink Sink, logger *slog.Logger) *Service {
	ctx, cancel := context.WithCancel(ctx)
	return &Service{
		ctx:      ctx,
		cancel:   cancel,
		cfg:      cfg,
		provider: provider,
		sink:     sink,
		log:      logger,
		sessions: state.NewSessions(cfg.State),
		dedup:    state.NewDedup(cfg.State.DedupeTTL, cfg.State.DedupeMaxCount),
		limiter:  state.NewLimiter(cfg.Rate),
		slots:    make(chan struct{}, cfg.Runtime.MaxConcurrent),
	}
}

// Submit atomically admits work without waiting for any network operation.
// [Main Request] 07 Admission: validate service/message state and claim the message ID before reserving any
// background work.
func (s *Service) Submit(r Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return errors.New("service is shutting down")
	}
	if r.ID == "" || r.Topic == "" || r.User == "" || strings.TrimSpace(r.Text) == "" {
		return errors.New("invalid message")
	}
	claimed, err := s.dedup.Claim(r.ID, time.Now())
	if err != nil || !claimed {
		return err
	}
	text := strings.TrimSpace(r.Text)
	clear := text == "/clear"
	help := text == "/help"
	// [Main Request] 07 Admission: /clear cancels and removes topic state even if no slot remains for its confirmation.
	if clear {
		s.sessions.Clear(r.Topic)
	}
	// [Main Request] 07 Admission: every reply needs a bounded worker; its deadline includes initial card creation.
	select {
	case s.slots <- struct{}{}:
	default:
		s.dedup.Release(r.ID)
		return state.ErrCapacity
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Runtime.RequestTimeout)
	var generation uint64
	var history []model.Message
	var notice string
	var reply string
	// [Main Request] 07 Admission: local commands skip the model; ordinary text reserves topic history, then
	// checks user quota.
	if help {
		reply, notice = helpText, "Complete"
	} else if clear {
		notice = "History cleared"
	} else {
		generation, history, err = s.sessions.Begin(r.Topic, r.Text, time.Now(), cancel)
		if err != nil {
			notice = err.Error()
		} else if !s.limiter.Allow(r.User, time.Now()) {
			notice = "Rate limit reached; try again later"
			s.sessions.End(r.Topic, generation)
			generation = 0
		}
	}
	s.wg.Add(1)
	// [Main Request] 08 Worker: use the service lifetime, not the callback context; always release its slot/topic
	// reservation.
	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()
		defer cancel()
		if generation != 0 {
			defer s.sessions.End(r.Topic, generation)
		}
		if s.cfg.LogContent {
			s.log.Info("message received", "message_id", r.ID, "topic", r.Topic, "user", r.User, "input", r.Text)
		}
		// [Main Request] 08 Worker: feishu.Sink.Open creates the placeholder before any model request is sent.
		out, err := s.sink.Open(ctx, r.ID)
		if err != nil {
			s.log.Error("response creation failed", "message_id", r.ID, "error", err)
			return
		}
		// [Main Request] 08 Worker: send help, clear confirmations and admission notices locally, then finish
		// without generation.
		if notice != "" {
			writeErr := s.final(out, reply, notice)
			if s.cfg.LogContent {
				s.log.Info(
					"local reply",
					"message_id", r.ID,
					"topic", r.Topic,
					"output", reply,
					"status", notice,
					"success", writeErr == nil,
				)
			}
			return
		}
		// [Main Request] 09 Generation: append this input to copied successful history and hand the output session
		// to worker.run.
		history = append(history, model.Message{Role: "user", Content: r.Text})
		s.run(ctx, out, r, generation, history)
	}()
	return nil
}

// Shutdown stops admission before waiting, preventing Add from racing with Wait.
// [Main Request] 13 Shutdown: reject new work, cancel existing workers and wait only until the shutdown context
// expires.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
