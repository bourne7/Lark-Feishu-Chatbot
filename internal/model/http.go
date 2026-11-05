package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Stream sends one request and validates protocol completion before accepting its output.
// [Main Request] 10 Model: build the protocol request, send authenticated HTTP and translate SSE events into
// visible text deltas.
func (c *Client) Stream(ctx context.Context, messages []Message, emit func(string) error) error {
	path, body, err := c.request(messages)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return errors.New("cannot encode model request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return errors.New("cannot create model request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.cfg.APIType == "anthropic-messages" {
		req.Header.Set("x-api-key", c.cfg.APIKey)
		req.Header.Set("anthropic-version", c.cfg.AnthropicVersion)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("model connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain only a bounded amount; never surface an untrusted response body or credential.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("model HTTP status %d", resp.StatusCode)
	}
	// [Main Request] 10 Model: reject protocol errors/incomplete answers; emit text back to worker.run without
	// updating cards here.
	hasText := false
	err = readSSE(resp.Body, func(event, data string) (bool, error) {
		if c.cfg.APIType == "chat-completions" && data == "[DONE]" {
			return true, nil
		}
		var item wireEvent
		if json.Unmarshal([]byte(data), &item) != nil {
			return false, errors.New("invalid model stream JSON")
		}
		if item.Type == "" {
			item.Type = event
		}
		if len(item.Error) > 0 && string(item.Error) != "null" {
			return false, errors.New("model reported an error")
		}
		var text string
		var done bool
		switch c.cfg.APIType {
		case "chat-completions":
			for _, choice := range item.Choices {
				if choice.Index != 0 {
					continue
				}
				if choice.FinishReason == "length" ||
					choice.FinishReason == "content_filter" ||
					choice.FinishReason == "tool_calls" {
					return false, errors.New("model did not complete a text answer")
				}
				text += choice.Delta.Content
			}
		case "responses":
			switch item.Type {
			case "response.output_text.delta":
				if json.Unmarshal(item.Delta, &text) != nil {
					return false, errors.New("invalid text delta")
				}
			case "response.completed":
				done = true
			case "response.failed", "response.incomplete", "error":
				return false, errors.New("model response did not complete")
			}
		case "anthropic-messages":
			switch item.Type {
			case "content_block_delta":
				var delta struct{ Type, Text string }
				if json.Unmarshal(item.Delta, &delta) != nil {
					return false, errors.New("invalid content delta")
				}
				if delta.Type == "text_delta" {
					text = delta.Text
				}
			case "message_delta":
				var delta struct {
					StopReason string `json:"stop_reason"`
				}
				if json.Unmarshal(item.Delta, &delta) != nil {
					return false, errors.New("invalid message delta")
				}
				if delta.StopReason == "max_tokens" || delta.StopReason == "tool_use" || delta.StopReason == "refusal" {
					return false, errors.New("model did not complete a text answer")
				}
			case "message_stop":
				done = true
			case "error":
				return false, errors.New("model reported an error")
			}
		}
		if text != "" {
			if strings.TrimSpace(text) != "" {
				hasText = true
			}
			if err := emit(text); err != nil {
				return false, err
			}
		}
		return done, nil
	})
	// [Main Request] 10 Model: cancellation, stream failure or empty text must propagate to the worker instead of
	// becoming success.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if !hasText {
		return errors.New("model completed without visible text")
	}
	return nil
}

// wireEvent captures only supported text and terminal fields; unknown event fields stay harmless.
type wireEvent struct {
	Type    string          `json:"type"`
	Delta   json.RawMessage `json:"delta"`
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}
