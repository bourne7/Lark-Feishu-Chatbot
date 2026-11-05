package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"lark-feishu-chatbot/internal/config"
)

// TestRunMirrorsDefaultLoggersAndFailure covers legacy terminal logs and final errors.
func TestRunMirrorsDefaultLoggersAndFailure(t *testing.T) {
	previousWriter, previousFlags := log.Writer(), log.Flags()
	path := filepath.Join(t.TempDir(), "command.log")
	for key, value := range map[string]string{
		"ENABLE_LOG":  "true",
		"LOG_FILE":    path,
		"LOG_LEVEL":   "info",
		"LOG_CONTENT": "true",
	} {
		t.Setenv(key, value)
	}
	var stdout bytes.Buffer
	failure := errors.New("fixture command failure")
	err := Run(&stdout, func(_ *slog.Logger, _ config.Logging) error {
		slog.Info("default structured log")
		log.Print("standard library log")
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("command error lost: %v", err)
	}
	if log.Writer() != previousWriter || log.Flags() != previousFlags {
		t.Error("default standard logger retained the closed command destination")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, stdout.Bytes()) {
		t.Fatal("default logger output did not reach both destinations")
	}
	for _, text := range []string{
		"default structured log",
		"standard library log",
		"fixture command failure",
	} {
		if !bytes.Contains(data, []byte(text)) {
			t.Fatalf("missing log %q: %s", text, data)
		}
	}
}

// TestLoggerMirrorsJSONAndAppends checks the file actually usable through a bind mount.
func TestLoggerMirrorsJSONAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "chatbot.log")
	for range 2 {
		var stdout bytes.Buffer
		logger, closeLog, err := New(slog.LevelInfo, true, path, &stdout)
		if err != nil {
			t.Fatal(err)
		}
		logger.Debug("hidden")
		logger.Info("ready", "component", "test")
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(bytes.TrimSpace(stdout.Bytes())) {
			t.Fatalf("invalid stdout JSON: %s", stdout.Bytes())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte("\n")) != 2 || bytes.Contains(data, []byte("hidden")) {
		t.Fatalf("unexpected file content: %s", data)
	}
}

// TestLoggerOptionalFileAndInvalidPath verifies stdout-only operation and explicit startup failure.
func TestLoggerOptionalFileAndInvalidPath(t *testing.T) {
	var stdout bytes.Buffer
	logger, closeLog, err := New(slog.LevelInfo, false, "", &stdout)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	logger.Info("ready")
	if stdout.Len() == 0 {
		t.Fatal("stdout missing")
	}
	if _, _, err := New(slog.LevelInfo, true, t.TempDir(), &stdout); err == nil {
		t.Fatal("directory accepted as log file")
	}
}

// TestDisabledFileLoggingDoesNotCreateFile keeps console diagnostics without touching the mount.
func TestDisabledFileLoggingDoesNotCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled.log")
	var stdout bytes.Buffer
	logger, closeLog, err := New(slog.LevelInfo, false, path, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	logger.Info("ready")
	if stdout.Len() == 0 {
		t.Fatal("console log missing")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled logger touched file: %v", err)
	}
}
