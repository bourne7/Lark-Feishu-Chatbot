package feishu

import (
	"context"
	"errors"
	"net/http"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"lark-feishu-chatbot/internal/chat"
	"lark-feishu-chatbot/internal/config"
)

// API isolates the two platform operations used by streaming cards.
type API interface {
	Reply(context.Context, string, string) (string, error)
	Patch(context.Context, string, string) error
}

// SDKAPI uses only the IM SDK service rather than initializing unrelated product services.
type SDKAPI struct{ im *larkim.V1 }

// NewAPI initializes the official token cache and serialization with bounded HTTP calls.
func NewAPI(cfg config.Feishu, timeout time.Duration) *SDKAPI {
	// The platform API origin is part of the Feishu protocol, not a model endpoint fallback.
	c := &larkcore.Config{BaseUrl: "https://open.feishu.cn", AppId: cfg.AppID, AppSecret: cfg.AppSecret,
		AppType: larkcore.AppTypeSelfBuilt, EnableTokenCache: true, ReqTimeout: timeout,
		Logger: sdkLogger{}, HttpClient: &http.Client{Timeout: timeout}}
	larkcore.NewLogger(c)
	larkcore.NewCache(c)
	larkcore.NewSerialization(c)
	larkcore.NewHttpClient(c)
	return &SDKAPI{im: larkim.New(c)}
}

// Reply creates one native interactive card and returns its patchable message identifier.
// [Main Request] 11 Cards: send the platform Reply request for a placeholder or continuation card and retain its
// message ID.
func (a *SDKAPI) Reply(ctx context.Context, parent, card string) (string, error) {
	resp, err := a.im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().MessageId(parent).
		Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("interactive").Content(card).Build()).Build())
	if err != nil || resp == nil || !resp.Success() || resp.Data == nil || resp.Data.MessageId == nil {
		return "", errors.New("Feishu card creation failed")
	}
	return *resp.Data.MessageId, nil
}

// Patch updates an existing card without forwarding platform response bodies into logs.
// [Main Request] 11 Cards: send a platform Patch request for an existing card; return failures to the owning chat
// worker.
func (a *SDKAPI) Patch(ctx context.Context, id, card string) error {
	resp, err := a.im.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().MessageId(id).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(card).Build()).Build())
	if err != nil || resp == nil || !resp.Success() {
		return errors.New("Feishu card update failed")
	}
	return nil
}

// Sink creates serial card output sessions with a configurable text bound.
type Sink struct {
	api     API
	maximum int
}

// NewSink injects an API so tests never need real Feishu credentials.
func NewSink(api API, maximum int) *Sink { return &Sink{api: api, maximum: maximum} }

// Open immediately creates a visible generation placeholder.
// [Main Request] 08 Worker: create the first reply card and return an output session owned by exactly one worker.
func (s *Sink) Open(ctx context.Context, parent string) (chat.Output, error) {
	card := renderCard("Thinking…", "Generating")
	id, err := s.api.Reply(ctx, parent, card)
	if err != nil {
		return nil, err
	}
	return &output{sink: s, parent: parent, ids: []string{id}, cards: []string{card}}, nil
}

// output is intentionally unsynchronized because chat assigns it one serial owner.
type output struct {
	sink       *Sink
	parent     string
	ids, cards []string
}

// Update appends continuation cards and avoids patching unchanged earlier fragments.
// [Main Request] 11 Cards: split the latest snapshot, render Markdown cards, create missing continuations and
// patch changed cards serially.
func (o *output) Update(ctx context.Context, text, status string) error {
	parts := splitMarkdown(text, o.sink.maximum)
	for i, part := range parts {
		label := status
		if i < len(parts)-1 {
			label = "Continued"
		}
		if part == "" {
			part = " "
		}
		card := renderCard(part, label)
		if i >= len(o.ids) {
			id, err := o.sink.api.Reply(ctx, o.parent, card)
			if err != nil {
				return err
			}
			o.ids = append(o.ids, id)
			o.cards = append(o.cards, card)
		} else if card != o.cards[i] {
			if err := o.sink.api.Patch(ctx, o.ids[i], card); err != nil {
				return err
			}
			o.cards[i] = card
		}
	}
	// Finishing an incomplete fence can merge fragments; clear any previously visible surplus.
	for i := len(parts); i < len(o.ids); i++ {
		card := renderCard("Content continues in the preceding card.", status)
		if card != o.cards[i] {
			if err := o.sink.api.Patch(ctx, o.ids[i], card); err != nil {
				return err
			}
			o.cards[i] = card
		}
	}
	return nil
}

// sdkLogger suppresses SDK payload logs; the application records safe operation-level outcomes.
type sdkLogger struct{}

func (sdkLogger) Debug(context.Context, ...interface{}) {}
func (sdkLogger) Info(context.Context, ...interface{})  {}
func (sdkLogger) Warn(context.Context, ...interface{})  {}
func (sdkLogger) Error(context.Context, ...interface{}) {}
