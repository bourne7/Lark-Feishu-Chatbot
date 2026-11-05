package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"lark-feishu-chatbot/internal/chat"
	"lark-feishu-chatbot/internal/config"
)

// TestCallbackBodyReadFailureIsLogged covers system errors before SDK dispatch.
func TestCallbackBodyReadFailureIsLogged(t *testing.T) {
	var stdout bytes.Buffer
	h := NewHandler(config.Feishu{}, submitFunc(func(chat.Request) error {
		t.Fatal("invalid callback submitted")
		return nil
	}), slog.New(slog.NewJSONHandler(&stdout, nil)))
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"level":"ERROR"`)) ||
		!bytes.Contains(stdout.Bytes(), []byte("callback body")) {
		t.Fatalf("callback system error missing: %s", stdout.Bytes())
	}
}

// TestNormalize keeps ordinary at signs and supports open_id and multiple mentions.
func TestNormalize(t *testing.T) {
	var e larkim.P2MessageReceiveV1
	if err := json.Unmarshal(
		[]byte(`{"event":{"sender":{"sender_type":"user","sender_id":{"open_id":"ou_1"}},`+
			`"message":{"message_id":"m","chat_id":"c","root_id":"root",`+
			`"chat_type":"group","message_type":"text",`+
			`"content":"{\"text\":\"@_user_1 @_user_2 email a@b.com @ordinary\"}",`+
			`"mentions":[{"key":"@_user_1","name":"Lark-Feishu-Chatbot","mentioned_type":"bot"},`+
			`{"key":"@_user_2","name":"Alice"}]}}}`),
		&e,
	); err != nil {
		t.Fatal(err)
	}
	r, ok := normalize(&e, "Lark-Feishu-Chatbot")
	if !ok ||
		r.User != "ou_1" ||
		r.Topic != "c/root" ||
		!strings.Contains(r.Text, "a@b.com @ordinary") ||
		strings.Contains(r.Text, "@_user_1") {
		t.Fatalf("%+v %v", r, ok)
	}
	e.Event.Message.Mentions = nil
	if _, ok := normalize(&e, "Lark-Feishu-Chatbot"); ok {
		t.Fatal("unmentioned group message admitted")
	}
	bot := "bot"
	e.Event.Sender.SenderType = &bot
	if _, ok := normalize(&e, "Lark-Feishu-Chatbot"); ok {
		t.Fatal("bot event admitted")
	}
	if _, ok := normalize(&larkim.P2MessageReceiveV1{}, "Lark-Feishu-Chatbot"); ok {
		t.Fatal("empty event admitted")
	}
}

// TestCardSplitting bounds Unicode text and balances partial code fences for display.
func TestCardSplitting(t *testing.T) {
	text := "Intro\n```go\n" + strings.Repeat("🌍 fmt.Println(1)\n", 40)
	parts := splitMarkdown(text, 160)
	if len(parts) < 2 {
		t.Fatal("long answer was not split")
	}
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 160 || strings.Count(p, "```")%2 != 0 {
			t.Fatalf("invalid part %q", p)
		}
	}
	if !strings.Contains(strings.Join(parts, ""), "```go") {
		t.Fatal("language discarded")
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(renderCard(parts[0], "Generating")), &card); err != nil {
		t.Fatal(err)
	}
	if card["schema"] != "2.0" || !strings.Contains(renderCard(parts[0], "Complete"), "markdown") {
		t.Fatal(card)
	}
}

// TestFenceStateSurvivesMidlineSplit keeps literal backticks inside a long code line.
func TestFenceStateSurvivesMidlineSplit(t *testing.T) {
	text := "```go\n" + strings.Repeat("x", 42) + "```literal\n" + strings.Repeat("y", 80) + "\n```"
	parts := splitMarkdown(text, 128)
	if len(parts) < 2 || !strings.HasPrefix(parts[1], "```go\n") {
		t.Fatal(parts)
	}
	if !strings.HasSuffix(parts[0], "\n```") {
		t.Fatal(parts)
	}
	if !strings.HasPrefix(parts[len(parts)-1], "```go\n") {
		t.Fatal(parts)
	}
}

// submitFunc lets callback tests observe admission without background network calls.
type submitFunc func(chat.Request) error

func (f submitFunc) Submit(r chat.Request) error { return f(r) }

// TestCallbackChallengeAndAuthentication exercises the real SDK callback boundary.
func TestCallbackChallengeAndAuthentication(t *testing.T) {
	h := NewHandler(config.Feishu{
		VerificationToken: "verify",
		BotName:           "Lark-Feishu-Chatbot",
	}, submitFunc(func(chat.Request) error {
		return nil
	}), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	for _, tc := range []struct {
		token string
		ok    bool
	}{{"verify", true}, {"wrong", false}} {
		req := httptest.NewRequest(
			http.MethodPost,
			"/events",
			strings.NewReader(`{"type":"url_verification","token":"`+tc.token+`","challenge":"hello"}`),
		)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if tc.ok && (rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello")) {
			t.Fatal(rec.Code, rec.Body.String())
		}
		if !tc.ok && rec.Code == 200 {
			t.Fatal("bad token accepted")
		}
	}
}

// fakeAPI records message mutations without exposing Feishu credentials or traffic.
type fakeAPI struct {
	created, patched int
	contents         map[string]string
}

func (a *fakeAPI) Reply(_ context.Context, _ string, card string) (string, error) {
	a.created++
	id := string(rune('a' + a.created))
	if a.contents == nil {
		a.contents = make(map[string]string)
	}
	a.contents[id] = card
	return id, nil
}
func (a *fakeAPI) Patch(_ context.Context, id, card string) error {
	a.patched++
	a.contents[id] = card
	return nil
}

// TestObsoleteContinuationCleared prevents stale fragments when a completed fence changes splitting.
func TestObsoleteContinuationCleared(t *testing.T) {
	a := &fakeAPI{}
	sink := NewSink(a, 160)
	out, _ := sink.Open(context.Background(), "m")
	out.Update(context.Background(), strings.Repeat("old text\n", 100), "Generating")
	out.Update(context.Background(), "final text", "Complete")
	for id, card := range a.contents {
		if strings.Contains(card, "old text") {
			t.Fatalf("stale continuation %s", id)
		}
	}
}

// TestSinkAppendsCards ensures long streaming answers create bounded continuation cards.
func TestSinkAppendsCards(t *testing.T) {
	a := &fakeAPI{}
	sink := NewSink(a, 160)
	out, err := sink.Open(context.Background(), "incoming")
	if err != nil {
		t.Fatal(err)
	}
	if err = out.Update(context.Background(), strings.Repeat("text 🌍\n", 100), "Complete"); err != nil {
		t.Fatal(err)
	}
	if a.created < 2 || a.patched < 1 {
		t.Fatal(a)
	}
}
