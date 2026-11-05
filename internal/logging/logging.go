// Package logging mirrors command diagnostics to stdout and an optional log file.
package logging

import (
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"

	"lark-feishu-chatbot/internal/config"
)

// Run initializes logs before other settings and records failures before closing the file.
// [Main Request] 02 Logging: serve runs inside this lifetime so configuration/runtime errors reach both
// destinations before close.
func Run(stdout io.Writer, execute func(*slog.Logger, config.Logging) error) error {
	settings, err := config.LoadLogging()
	if err != nil {
		slog.New(slog.NewJSONHandler(stdout, nil)).Error("logging initialization failed", "error", err)
		return err
	}
	logger, closeLog, err := New(settings.LogLevel, settings.EnableLog, settings.LogFile, stdout)
	if err != nil {
		slog.New(slog.NewJSONHandler(stdout, nil)).Error("logging initialization failed", "error", err)
		return err
	}
	defer closeLog()
	previous := slog.Default()
	previousWriter, previousFlags := log.Writer(), log.Flags()
	slog.SetDefault(logger)
	defer func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()
	if err := execute(logger, settings); err != nil {
		logger.Error("command failed", "error", err)
		return err
	}
	return nil
}

// New mirrors structured application logs to an optional append-only file.
func New(level slog.Level, enabled bool, path string, stdout io.Writer) (*slog.Logger, func() error, error) {
	writer := stdout
	closeLog := func() error { return nil }
	if enabled {
		if path == "" {
			return nil, nil, errors.New("LOG_FILE is required when ENABLE_LOG is true")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return nil, nil, errors.New("cannot create log directory")
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
		if err != nil {
			return nil, nil, errors.New("cannot open log file")
		}
		writer = io.MultiWriter(stdout, file)
		closeLog = file.Close
	}
	return slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level})), closeLog, nil
}
