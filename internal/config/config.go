// Package config loads explicit environment settings without deployment defaults.
package config

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Model describes one explicitly selected text generation protocol.
type Model struct {
	APIType, BaseURL, APIKey, Name, SystemPrompt, AnthropicVersion, ProxyURL string
	MaxOutputTokens                                                          int
	Temperature                                                              *float64
}

// Config contains validated settings grouped by ownership, not by environment aliases.
type Config struct {
	HTTPAddr                                   string
	ReadTimeout, WriteTimeout, ShutdownTimeout time.Duration
	Feishu                                     Feishu
	Model                                      Model
	Runtime                                    Runtime
	State                                      State
	Rate                                       Rate
	Card                                       Card
	Logging
	UseHTTPS          bool
	CertFile, KeyFile string
}

// Logging contains the shared, explicit settings used by both commands.
type Logging struct {
	LogLevel   slog.Level
	LogContent bool
	LogFile    string
	EnableLog  bool
}

// LoadLogging validates logging independently so startup failures can also be recorded.
func LoadLogging() (Logging, error) {
	r := &reader{}
	c := Logging{
		LogContent: r.boolean("LOG_CONTENT"),
		LogFile:    os.Getenv("LOG_FILE"),
		EnableLog:  r.boolean("ENABLE_LOG"),
	}
	if c.EnableLog && strings.TrimSpace(c.LogFile) == "" {
		r.errors = append(r.errors, "LOG_FILE is required when ENABLE_LOG is true")
	}
	if err := c.LogLevel.UnmarshalText([]byte(r.required("LOG_LEVEL"))); err != nil {
		r.errors = append(r.errors, "LOG_LEVEL is invalid")
	}
	return c, r.result()
}

// Feishu holds callback authentication and bot identity settings.
type Feishu struct{ AppID, AppSecret, VerificationToken, EncryptKey, BotName string }

// Runtime bounds generation time and concurrent work.
type Runtime struct {
	RequestTimeout, IdleTimeout time.Duration
	MaxConcurrent               int
}

// State bounds all in-memory history and message identifiers.
type State struct {
	SessionTTL, DedupeTTL                                                time.Duration
	SessionMaxCount, HistoryMaxMessages, HistoryMaxRunes, DedupeMaxCount int
}

// Rate configures rolling-minute and UTC-day attempt quotas.
type Rate struct {
	Enabled                     bool
	PerMinute, PerDay, MaxUsers int
}

// Card controls update cadence and maximum displayed text per card.
type Card struct {
	UpdateInterval time.Duration
	MaxRunes       int
}

// reader accumulates field-only validation errors without leaking values.
type reader struct{ errors []string }

// required returns an explicitly configured, nonblank value.
func (r *reader) required(key string) string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		r.errors = append(r.errors, key+" is required")
	}
	return v
}

// integer validates a configured numeric bound, including explicit zero when allowed.
func (r *reader) integer(key string, minimum int) int {
	v, err := strconv.Atoi(r.required(key))
	if err != nil || v < minimum {
		r.errors = append(r.errors, key+" has an invalid integer value")
	}
	return v
}

// duration rejects missing, zero and negative intervals.
func (r *reader) duration(key string) time.Duration {
	v, err := time.ParseDuration(r.required(key))
	if err != nil || v <= 0 {
		r.errors = append(r.errors, key+" must be a positive duration")
	}
	return v
}

// boolean requires explicit true or false configuration.
func (r *reader) boolean(key string) bool {
	v, err := strconv.ParseBool(r.required(key))
	if err != nil {
		r.errors = append(r.errors, key+" must be a boolean")
	}
	return v
}

// result combines safe validation failures into one actionable startup error.
func (r *reader) result() error {
	if len(r.errors) == 0 {
		return nil
	}
	return fmt.Errorf("invalid environment: %s", strings.Join(r.errors, "; "))
}

// endpoint rejects ambiguous URL components while preserving the user-supplied base path.
func endpoint(value string) bool {
	u, err := url.Parse(value)
	return err == nil &&
		(u.Scheme == "https" || u.Scheme == "http") &&
		u.Host != "" &&
		u.User == nil &&
		u.RawQuery == "" &&
		u.Fragment == ""
}

// LoadModel validates only model settings so diagnostics do not require Feishu secrets.
func LoadModel() (Model, error) {
	r := &reader{}
	m := Model{
		APIType:          r.required("LLM_API_TYPE"),
		BaseURL:          strings.TrimRight(r.required("LLM_BASE_URL"), "/"),
		APIKey:           r.required("LLM_API_KEY"),
		Name:             r.required("LLM_MODEL"),
		MaxOutputTokens:  r.integer("LLM_MAX_OUTPUT_TOKENS", 1),
		SystemPrompt:     os.Getenv("LLM_SYSTEM_PROMPT"),
		AnthropicVersion: os.Getenv("LLM_ANTHROPIC_VERSION"),
		ProxyURL:         os.Getenv("LLM_PROXY_URL"),
	}
	switch m.APIType {
	case "chat-completions", "responses":
	case "anthropic-messages":
		if m.AnthropicVersion == "" {
			r.errors = append(r.errors, "LLM_ANTHROPIC_VERSION is required for anthropic-messages")
		}
	default:
		r.errors = append(r.errors, "LLM_API_TYPE is unsupported")
	}
	if !endpoint(m.BaseURL) {
		r.errors = append(r.errors, "LLM_BASE_URL must be an HTTP URL without credentials, query or fragment")
	}
	if m.ProxyURL != "" {
		u, err := url.Parse(m.ProxyURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			r.errors = append(r.errors, "LLM_PROXY_URL must be an HTTP proxy URL")
		}
	}
	if raw := os.Getenv("LLM_TEMPERATURE"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 2 {
			r.errors = append(r.errors, "LLM_TEMPERATURE must be between zero and two")
		} else {
			m.Temperature = &v
		}
	}
	return m, r.result()
}

// Load validates every service setting before constructing network clients.
// [Main Request] 03 Bootstrap: parse explicit environment settings; return field-only validation errors to logging.Run.
func Load() (Config, error) {
	r := &reader{}
	c := Config{
		HTTPAddr:        r.required("HTTP_ADDR"),
		ReadTimeout:     r.duration("HTTP_READ_TIMEOUT"),
		WriteTimeout:    r.duration("HTTP_WRITE_TIMEOUT"),
		ShutdownTimeout: r.duration("SHUTDOWN_TIMEOUT"),
		Feishu: Feishu{
			AppID:             r.required("FEISHU_APP_ID"),
			AppSecret:         r.required("FEISHU_APP_SECRET"),
			VerificationToken: r.required("FEISHU_VERIFICATION_TOKEN"),
			EncryptKey:        os.Getenv("FEISHU_ENCRYPT_KEY"),
			BotName:           r.required("FEISHU_BOT_NAME"),
		},
		Runtime: Runtime{
			RequestTimeout: r.duration("REQUEST_TIMEOUT"),
			IdleTimeout:    r.duration("STREAM_IDLE_TIMEOUT"),
			MaxConcurrent:  r.integer("MAX_CONCURRENT_REQUESTS", 1),
		},
		State: State{
			SessionTTL:         r.duration("SESSION_TTL"),
			DedupeTTL:          r.duration("DEDUPE_TTL"),
			SessionMaxCount:    r.integer("SESSION_MAX_COUNT", 1),
			HistoryMaxMessages: r.integer("HISTORY_MAX_MESSAGES", 2),
			HistoryMaxRunes:    r.integer("HISTORY_MAX_RUNES", 1),
			DedupeMaxCount:     r.integer("DEDUPE_MAX_COUNT", 1),
		},
		Rate: Rate{
			Enabled:   r.boolean("ACCESS_CONTROL_ENABLE"),
			PerMinute: r.integer("RATE_LIMIT_PER_MINUTE", 0),
			PerDay:    r.integer("RATE_LIMIT_PER_DAY", 0),
			MaxUsers:  r.integer("RATE_LIMIT_MAX_USERS", 1),
		},
		Card: Card{
			UpdateInterval: r.duration("CARD_UPDATE_INTERVAL"),
			MaxRunes:       r.integer("CARD_MAX_RUNES", 128),
		},
	}
	var err error
	c.Logging, err = LoadLogging()
	if err != nil {
		r.errors = append(r.errors, err.Error())
	}
	c.UseHTTPS = r.boolean("USE_HTTPS")
	c.CertFile = os.Getenv("CERT_FILE")
	c.KeyFile = os.Getenv("KEY_FILE")
	if c.UseHTTPS {
		if strings.TrimSpace(c.CertFile) == "" {
			r.errors = append(r.errors, "CERT_FILE is required when USE_HTTPS is true")
		}
		if strings.TrimSpace(c.KeyFile) == "" {
			r.errors = append(r.errors, "KEY_FILE is required when USE_HTTPS is true")
		}
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		r.errors = append(r.errors, "HTTP_ADDR must contain a host and port")
	}
	c.Model, err = LoadModel()
	if err != nil {
		r.errors = append(r.errors, err.Error())
	}
	return c, r.result()
}
