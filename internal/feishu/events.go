// Package feishu adapts authenticated callbacks and native Markdown message cards.
package feishu

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"lark-feishu-chatbot/internal/chat"
	"lark-feishu-chatbot/internal/config"
)

// Submitter is the synchronous, network-free callback admission boundary.
type Submitter interface{ Submit(chat.Request) error }

// NewHandler delegates verification, signatures and decryption to the platform SDK.
// [Main Request] 05 Callback: register the SDK message callback now; the HTTP handler below invokes it for
// authenticated events.
func NewHandler(cfg config.Feishu, service Submitter, logger *slog.Logger) http.Handler {
	d := dispatcher.NewEventDispatcher(cfg.VerificationToken, cfg.EncryptKey).
		OnP2MessageReceiveV1(func(_ context.Context, e *larkim.P2MessageReceiveV1) error {
			// [Main Request] 06 Normalize: turn supported user text into a topic-aware chat.Request; ignore
			// unrelated events.
			r, ok := normalize(e, cfg.BotName)
			if !ok {
				return nil
			}
			// [Main Request] 07 Admission: Submit returns after admission; the callback never waits for model/card I/O.
			if err := service.Submit(r); err != nil {
				logger.Error("message admission failed", "message_id", r.ID, "topic", r.Topic, "error", err)
				return err
			}
			return nil
		})
	d.InitConfig(larkevent.WithLogger(sdkLogger{}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// [Main Request] 05 Callback: cap the HTTP body before SDK token/signature checks, decryption and event
		// dispatch.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			logger.Error("callback body read failed", "error", "cannot read callback body within size limit")
			http.Error(w, "invalid callback body", http.StatusBadRequest)
			return
		}
		resp := d.Handle(r.Context(), &larkevent.EventReq{
			Header:     r.Header,
			Body:       body,
			RequestURI: r.URL.Path,
		})
		if resp.StatusCode >= 400 {
			logger.Error("callback rejected", "status", resp.StatusCode)
			http.Error(w, "callback rejected", resp.StatusCode)
			return
		}
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(resp.Body); err != nil {
			logger.Error("callback response write failed", "error", "cannot write callback response")
		}
	})
}

// normalize admits human text and removes only explicitly identified bot placeholders.
// [Main Request] 06 Normalize: accept private text or group text mentioning this bot, preserving other mentions
// and ordinary text.
func normalize(e *larkim.P2MessageReceiveV1, botName string) (chat.Request, bool) {
	if e == nil || e.Event == nil || e.Event.Sender == nil || e.Event.Message == nil {
		return chat.Request{}, false
	}
	sender, m := e.Event.Sender, e.Event.Message
	if value(sender.SenderType) != "user" || sender.SenderId == nil || value(m.MessageType) != "text" {
		return chat.Request{}, false
	}
	user := value(sender.SenderId.OpenId)
	if user == "" {
		user = value(sender.SenderId.UserId)
	}
	var content struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(value(m.Content)), &content) != nil {
		return chat.Request{}, false
	}
	mentioned := false
	for _, mention := range m.Mentions {
		if mention == nil {
			continue
		}
		if value(mention.Name) == botName &&
			(value(mention.MentionedType) == "bot" ||
				value(mention.MentionedType) == "") {
			mentioned = true
			if key := value(mention.Key); key != "" {
				content.Text = strings.ReplaceAll(content.Text, key, "")
			}
		}
	}
	if value(m.ChatType) != "p2p" && (value(m.ChatType) != "group" || !mentioned) {
		return chat.Request{}, false
	}
	// [Main Request] 06 Normalize: chat ID + root message ID shares history within a topic, including across group
	// participants.
	root := value(m.RootId)
	if root == "" {
		root = value(m.MessageId)
	}
	r := chat.Request{
		ID:    value(m.MessageId),
		Topic: value(m.ChatId) + "/" + root,
		User:  user,
		Text:  strings.TrimSpace(content.Text),
	}
	return r, r.ID != "" && value(m.ChatId) != "" && root != "" && r.User != "" && r.Text != ""
}

// value safely handles optional platform fields.
func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
