package config

import (
	"strings"
	"testing"
)

// modelEnvironment supplies explicit test values rather than application defaults.
func modelEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"LLM_API_TYPE": "chat-completions", "LLM_BASE_URL": "https://example.test/custom/v1",
		"LLM_API_KEY": "secret-test-key", "LLM_MODEL": "test-model", "LLM_MAX_OUTPUT_TOKENS": "128",
	} {
		t.Setenv(key, value)
	}
}

// TestLoadModel validates an isolated model connection without Feishu credentials.
func TestLoadModel(t *testing.T) {
	modelEnvironment(t)
	cfg, err := LoadModel()
	if err != nil || cfg.BaseURL != "https://example.test/custom/v1" || cfg.Temperature != nil {
		t.Fatalf("unexpected config: %#v, %v", cfg, err)
	}
	for _, value := range []string{"responses", "anthropic-messages"} {
		t.Setenv("LLM_API_TYPE", value)
		t.Setenv("LLM_ANTHROPIC_VERSION", "2023-06-01")
		if _, err := LoadModel(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLoadModelRejectsUnsafeSettings ensures errors cannot expose credentials.
func TestLoadModelRejectsUnsafeSettings(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"LLM_API_KEY", ""}, {"LLM_MAX_OUTPUT_TOKENS", "0"}, {"LLM_API_TYPE", "unknown"},
		{"LLM_BASE_URL", "ftp://example.test"}, {"LLM_BASE_URL", "https://user:secret-test-key@example.test"},
		{"LLM_BASE_URL", "https://example.test/v1?token=secret-test-key"}, {"LLM_TEMPERATURE", "NaN"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			modelEnvironment(t)
			t.Setenv(tc.key, tc.value)
			_, err := LoadModel()
			if err == nil || strings.Contains(err.Error(), "secret-test-key") {
				t.Fatalf("unsafe validation: %v", err)
			}
		})
	}
}

// TestRequiredEnvironmentAndExplicitZero distinguishes absent quotas from disabled ones.
func TestRequiredEnvironmentAndExplicitZero(t *testing.T) {
	modelEnvironment(t)
	if _, err := Load(); err == nil {
		t.Fatal("missing required service settings accepted")
	}
	for key, value := range map[string]string{
		"HTTP_ADDR":                 ":9000",
		"HTTP_READ_TIMEOUT":         "10s",
		"HTTP_WRITE_TIMEOUT":        "15s",
		"SHUTDOWN_TIMEOUT":          "5s",
		"FEISHU_APP_ID":             "app",
		"FEISHU_APP_SECRET":         "secret",
		"FEISHU_VERIFICATION_TOKEN": "token",
		"FEISHU_BOT_NAME":           "Lark-Feishu-Chatbot",
		"REQUEST_TIMEOUT":           "30s",
		"STREAM_IDLE_TIMEOUT":       "5s",
		"MAX_CONCURRENT_REQUESTS":   "2",
		"SESSION_TTL":               "1h",
		"SESSION_MAX_COUNT":         "10",
		"HISTORY_MAX_MESSAGES":      "10",
		"HISTORY_MAX_RUNES":         "1000",
		"DEDUPE_TTL":                "5m",
		"DEDUPE_MAX_COUNT":          "100",
		"RATE_LIMIT_PER_MINUTE":     "0",
		"RATE_LIMIT_PER_DAY":        "0",
		"RATE_LIMIT_MAX_USERS":      "100",
		"CARD_UPDATE_INTERVAL":      "1s",
		"CARD_MAX_RUNES":            "6000",
		"LOG_LEVEL":                 "info",
		"LOG_CONTENT":               "false",
		"ENABLE_LOG":                "false",
		"USE_HTTPS":                 "false",
		"ACCESS_CONTROL_ENABLE":     "false",
	} {
		t.Setenv(key, value)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENABLE_LOG", "true")
	t.Setenv("LOG_FILE", "")
	if _, err := Load(); err == nil {
		t.Fatal("enabled file logger accepted missing path")
	}
	t.Setenv("LOG_FILE", "logs/test.log")
	t.Setenv("USE_HTTPS", "true")
	if _, err := Load(); err == nil {
		t.Fatal("HTTPS accepted missing certificate paths")
	}
	t.Setenv("CERT_FILE", "certs/server.crt")
	t.Setenv("KEY_FILE", "certs/server.key")
	t.Setenv("ACCESS_CONTROL_ENABLE", "true")
	cfg, err := Load()
	if err != nil || !cfg.EnableLog || !cfg.UseHTTPS || !cfg.Rate.Enabled {
		t.Fatalf("switches not mapped: %v", err)
	}
	t.Setenv("STREAM_IDLE_TIMEOUT", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("zero idle timeout accepted")
	}
}
