package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DeliciousBuding/grok2api/internal/config"
)

func TestSQLiteMediaCacheDSNEscapesURIPathCharacters(t *testing.T) {
	dsn := sqliteMediaCacheDSN(filepath.Join("data", "media?cache#1.db"))

	if strings.Contains(dsn, "media?cache") || strings.Contains(dsn, "#1.db") {
		t.Fatalf("sqlite media cache DSN should escape URI path metacharacters, got %q", dsn)
	}
	if !strings.Contains(dsn, "media%3Fcache%231.db") {
		t.Fatalf("sqlite media cache DSN should preserve the literal filename through escaping, got %q", dsn)
	}
	if !strings.Contains(dsn, "?_pragma=journal_mode(WAL)") {
		t.Fatalf("sqlite media cache DSN should keep pragmas in the query string, got %q", dsn)
	}
}

func TestLocalMediaCacheLimitBytesClampsMisconfiguredLargeValue(t *testing.T) {
	loadStorageTestConfig(t, "[cache.local]\nimage_max_mb = 9999999999999\nvideo_max_mb = 9999999999999\n")
	store := NewLocalMediaCacheStore()
	const maxCacheBytes = int64(1 << 40)

	if got := store.limitBytes(MediaImage); got != maxCacheBytes {
		t.Fatalf("expected image cache limit to clamp to %d, got %d", maxCacheBytes, got)
	}
	if got := store.limitBytes(MediaVideo); got != maxCacheBytes {
		t.Fatalf("expected video cache limit to clamp to %d, got %d", maxCacheBytes, got)
	}
}

func TestLocalMediaCacheLimitBytesHandlesInvalidAndNormalValues(t *testing.T) {
	loadStorageTestConfig(t, "[cache.local]\nimage_max_mb = -1\nvideo_max_mb = 2\n")
	store := NewLocalMediaCacheStore()

	if got := store.limitBytes(MediaImage); got != 0 {
		t.Fatalf("expected negative image cache limit to disable limit, got %d", got)
	}
	if got := store.limitBytes(MediaVideo); got != 2*1024*1024 {
		t.Fatalf("expected video cache limit to be 2MiB, got %d", got)
	}
}

func loadStorageTestConfig(t *testing.T, body string) {
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
