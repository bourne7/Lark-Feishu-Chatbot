// Command modelcheck verifies streaming and explicit history without Feishu credentials.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"lark-feishu-chatbot/internal/config"
	"lark-feishu-chatbot/internal/logging"
	"lark-feishu-chatbot/internal/model"
)

// main reports only diagnostic output and sanitized configuration/transport errors.
func main() {
	if err := run(os.Stdout); err != nil {
		os.Exit(1)
	}
}

// run requires explicit model settings and a total diagnostic deadline.
func run(stdout io.Writer) error {
	return logging.Run(stdout, func(logger *slog.Logger, settings config.Logging) error {
		return diagnose(logger, settings.LogContent)
	})
}

// diagnose uses only model, deadline and shared logging settings, without Feishu credentials.
func diagnose(logger *slog.Logger, logContent bool) error {
	cfg, err := config.LoadModel()
	if err != nil {
		return err
	}
	limit, err := time.ParseDuration(os.Getenv("REQUEST_TIMEOUT"))
	if err != nil || limit <= 0 {
		return errors.New("REQUEST_TIMEOUT must be a positive duration")
	}
	provider, err := model.New(cfg, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	logger.Info("model diagnostic started", "protocol", cfg.APIType, "model", cfg.Name)
	return check(ctx, provider, logger, logContent)
}

// check sends exactly two turns and requires the second to recover a fresh marker from history.
func check(ctx context.Context, provider model.Streamer, logger *slog.Logger, logContent bool) error {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("cannot create diagnostic marker")
	}
	marker := "lark-feishu-chatbot-" + hex.EncodeToString(random[:])
	history := []model.Message{
		{
			Role: "user",
			Content: "Remember this marker: " + marker +
				" . Reply with the marker in a fenced text code block, " +
				"followed by Hello outside the block.",
		},
	}
	for turn := 1; turn <= 2; turn++ {
		if logContent {
			logger.Info("diagnostic input", "turn", turn, "input", history[len(history)-1].Content)
		}
		var text strings.Builder
		chunks := 0
		err := provider.Stream(ctx, history, func(delta string) error {
			chunks++
			text.WriteString(delta)
			return nil
		})
		if logContent {
			logger.Info(
				"diagnostic output",
				"turn", turn,
				"output", text.String(),
				"complete", err == nil,
				"text_chunks", chunks,
			)
		}
		if err != nil {
			return fmt.Errorf("diagnostic turn %d failed: %w", turn, err)
		}
		if !strings.Contains(text.String(), marker) {
			return fmt.Errorf("turn %d did not preserve diagnostic context", turn)
		}
		logger.Info(fmt.Sprintf("Turn %d: passed (%d text chunks).", turn, chunks), "turn", turn, "text_chunks", chunks)
		history = append(history, model.Message{
			Role:    "assistant",
			Content: text.String(),
		}, model.Message{
			Role:    "user",
			Content: "What was the exact marker I asked you to remember? Reply with that marker only.",
		})
	}
	return nil
}
