// Command lark-feishu-chatbot serves Feishu callbacks and streams model text into Markdown cards.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"lark-feishu-chatbot/internal/chat"
	"lark-feishu-chatbot/internal/config"
	"lark-feishu-chatbot/internal/feishu"
	"lark-feishu-chatbot/internal/logging"
	"lark-feishu-chatbot/internal/model"
)

// main delegates cleanup before returning a process failure code.
// [Main Request] 01 Entry: main -> run -> serve assembles the process before any callback arrives.
func main() {
	if err := run(os.Stdout); err != nil {
		os.Exit(1)
	}
}

// run composes explicit settings and shuts down both HTTP and background generations.
func run(stdout io.Writer) error {
	// [Main Request] 02 Logging: logging.Run opens terminal/file output before serve validates other settings.
	return logging.Run(stdout, func(logger *slog.Logger, _ config.Logging) error {
		return serve(logger)
	})
}

// serve composes the service after shared logging is available.
func serve(logger *slog.Logger) error {
	// [Main Request] 03 Bootstrap: validate configuration and optional HTTPS credentials, then create the model
	// adapter.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	tlsConfig, err := serverTLS(cfg)
	if err != nil {
		return err
	}
	provider, err := model.New(cfg.Model, nil)
	if err != nil {
		return err
	}
	// [Main Request] 03 Bootstrap: inject model.Streamer and the Feishu Sink into the chat coordinator.
	service := chat.New(
		context.Background(),
		cfg,
		provider,
		feishu.NewSink(feishu.NewAPI(cfg.Feishu, cfg.WriteTimeout), cfg.Card.MaxRunes),
		logger,
	)
	mux := http.NewServeMux()
	// [Main Request] 04 HTTP: /events enters feishu.NewHandler; /healthz checks only process availability.
	mux.Handle("POST /events", feishu.NewHandler(cfg.Feishu, service, logger))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintln(w, "ok")
	})
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: cfg.ReadTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.ReadTimeout,
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failure := make(chan error, 1)
	// [Main Request] 04 HTTP: start the configured listener, then wait for a signal or listener failure.
	go func() {
		if cfg.UseHTTPS {
			failure <- server.ListenAndServeTLS("", "")
		} else {
			failure <- server.ListenAndServe()
		}
	}()
	logger.Info("service listening", "address", cfg.HTTPAddr, "protocol", cfg.Model.APIType, "https", cfg.UseHTTPS)
	select {
	case <-signals.Done():
	case err = <-failure:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	// [Main Request] 13 Shutdown: stop worker admission and cancel generations while HTTP drains under the same
	// deadline.
	workerDone := make(chan error, 1)
	go func() { workerDone <- service.Shutdown(ctx) }()
	if shutdownErr := server.Shutdown(ctx); shutdownErr != nil {
		_ = server.Close()
		logger.Error("HTTP shutdown deadline reached", "error", shutdownErr)
	}
	if workerErr := <-workerDone; workerErr != nil {
		logger.Error("worker shutdown deadline reached", "error", workerErr)
	}
	return err
}
