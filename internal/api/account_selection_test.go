package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/config"
	"github.com/DeliciousBuding/grok2api/internal/platform"
)

func TestSelectionMaxRetriesIsClamped(t *testing.T) {
	loadTestConfig(t, "[retry]\nmax_retries = 100\n")

	if got := selectionMaxRetries(); got != 5 {
		t.Fatalf("selectionMaxRetries should clamp retry storms to 5, got %d", got)
	}
}

func TestSelectionMaxRetriesTreatsNegativeAsZero(t *testing.T) {
	loadTestConfig(t, "[retry]\nmax_retries = -2\n")

	if got := selectionMaxRetries(); got != 0 {
		t.Fatalf("selectionMaxRetries should clamp negative values to 0, got %d", got)
	}
}

func TestRetryBudgetStopsAtConfiguredLimit(t *testing.T) {
	err := platform.UpstreamError("rate limited", 429, "")

	if !shouldRetryAttempt(err, 0, 1) {
		t.Fatal("first retryable error should spend retry budget")
	}
	if shouldRetryAttempt(err, 1, 1) {
		t.Fatal("retry budget should be exhausted when attempt reaches max")
	}
}

func TestTimeoutClassDurationReadsConfigAndRejectsInvalid(t *testing.T) {
	loadTestConfig(t, "[timeout]\nchat_sec = 7\nconsole_sec = -1\n")

	if got := timeoutClassDuration("chat", 300); got != 7*time.Second {
		t.Fatalf("expected configured chat timeout, got %v", got)
	}
	if got := timeoutClassDuration("console", 300); got != 300*time.Second {
		t.Fatalf("invalid timeout should fall back to default, got %v", got)
	}
	if got := timeoutClassDuration("image", 180); got != 180*time.Second {
		t.Fatalf("missing timeout should use default, got %v", got)
	}
}

func loadTestConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.toml")
	user := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(defaults, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config.SetPaths(defaults, user)
	if err := config.Load(); err != nil {
		t.Fatal(err)
	}
}
