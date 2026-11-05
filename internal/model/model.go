// Package model adapts text streaming protocols without owning conversation state.
package model

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"lark-feishu-chatbot/internal/config"
)

// Message is one portable text turn. Tool execution is deliberately outside this contract.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Streamer emits only visible text and returns exactly one terminal result.
type Streamer interface {
	Stream(context.Context, []Message, func(string) error) error
}

// Client holds immutable protocol settings and a reusable HTTP connection pool.
type Client struct {
	cfg  config.Model
	http *http.Client
}

// New constructs a protocol adapter. A supplied HTTP client supports isolated wire tests.
// [Main Request] 03 Bootstrap: select the configured protocol explicitly and reuse one HTTP transport for model calls.
func New(cfg config.Model, client *http.Client) (Streamer, error) {
	switch cfg.APIType {
	case "chat-completions", "responses", "anthropic-messages":
	default:
		return nil, errors.New("unsupported model protocol")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil ||
		u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" {
		return nil, errors.New("invalid model base URL")
	}
	if cfg.APIKey == "" || cfg.Name == "" || cfg.MaxOutputTokens <= 0 {
		return nil, errors.New("missing model settings")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		if cfg.ProxyURL != "" {
			proxy, err := url.Parse(cfg.ProxyURL)
			if err != nil {
				return nil, errors.New("invalid model proxy")
			}
			transport.Proxy = http.ProxyURL(proxy)
		}
		client = &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Client{cfg: cfg, http: client}, nil
}

// request converts portable text into the selected protocol's request envelope.
// [Main Request] 10 Model: encode explicit history and append only this protocol's resource path to the configured
// base URL.
func (c *Client) request(messages []Message) (string, map[string]any, error) {
	for _, m := range messages {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
			return "", nil, errors.New("invalid text history")
		}
	}
	if len(messages) == 0 {
		return "", nil, errors.New("empty text history")
	}
	body := map[string]any{"model": c.cfg.Name, "stream": true}
	if c.cfg.Temperature != nil {
		body["temperature"] = *c.cfg.Temperature
	}
	var path string
	switch c.cfg.APIType {
	case "chat-completions":
		path = "/chat/completions"
		input := append([]Message(nil), messages...)
		if c.cfg.SystemPrompt != "" {
			input = append([]Message{{Role: "system", Content: c.cfg.SystemPrompt}}, input...)
		}
		body["messages"], body["max_tokens"] = input, c.cfg.MaxOutputTokens
	case "responses":
		path = "/responses"
		body["input"], body["max_output_tokens"], body["store"] = messages, c.cfg.MaxOutputTokens, false
		if c.cfg.SystemPrompt != "" {
			body["instructions"] = c.cfg.SystemPrompt
		}
	case "anthropic-messages":
		path = "/messages"
		body["messages"], body["max_tokens"] = messages, c.cfg.MaxOutputTokens
		if c.cfg.SystemPrompt != "" {
			body["system"] = c.cfg.SystemPrompt
		}
	}
	return path, body, nil
}
