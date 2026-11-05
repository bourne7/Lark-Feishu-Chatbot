package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestStartupFailureReachesFile ensures configuration failures use the initialized logger.
func TestStartupFailureReachesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup.log")
	for key, value := range map[string]string{
		"ENABLE_LOG":  "true",
		"LOG_FILE":    path,
		"LOG_LEVEL":   "info",
		"LOG_CONTENT": "true",
		"HTTP_ADDR":   "invalid",
	} {
		t.Setenv(key, value)
	}
	var stdout bytes.Buffer
	if err := run(&stdout); err == nil {
		t.Fatal("invalid service configuration accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, stdout.Bytes()) ||
		!bytes.Contains(data, []byte(`"level":"ERROR"`)) ||
		!bytes.Contains(data, []byte("HTTP_ADDR")) {
		t.Fatalf("startup error missing from terminal or file: %s", data)
	}
}
