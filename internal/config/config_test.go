package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountStorageKeysAreStartupOnly(t *testing.T) {
	for _, key := range []string{
		"account.storage.backend",
		"account.local.path",
		"account.sqlite.path",
		"account.postgresql.dsn",
		"account.redis.addr",
		"server.read_header_timeout_sec",
		"server.read_timeout_sec",
		"server.write_timeout_sec",
		"server.idle_timeout_sec",
		"server.shutdown_timeout_sec",
	} {
		if !IsStartupOnlyConfigKey(key) {
			t.Fatalf("expected %s to be startup-only", key)
		}
	}
}

func TestLoadIfStaleSkipsReloadWithinInterval(t *testing.T) {
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.toml")
	user := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(defaults, []byte("[server]\nmax_body_bytes = 8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setMtime(t, defaults, time.Unix(1, 0))
	s := &Snapshot{defaultsPath: defaults, userPath: user, defaultsMtime: -1, userMtime: -1}
	now := time.Unix(100, 0)
	if err := s.loadIfStaleAt(now, time.Second); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if got := s.GetInt("server.max_body_bytes", 0); got != 8 {
		t.Fatalf("expected initial config value 8, got %d", got)
	}

	if err := os.WriteFile(defaults, []byte("[server]\nmax_body_bytes = [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setMtime(t, defaults, time.Unix(2, 0))
	if err := s.loadIfStaleAt(now.Add(500*time.Millisecond), time.Second); err != nil {
		t.Fatalf("load inside throttle window should skip parsing changed file: %v", err)
	}
	if got := s.GetInt("server.max_body_bytes", 0); got != 8 {
		t.Fatalf("config value should stay cached inside throttle window, got %d", got)
	}
	if err := s.loadIfStaleAt(now.Add(2*time.Second), time.Second); err == nil {
		t.Fatal("expected reload after throttle window to parse changed invalid config")
	}
}

func setMtime(t *testing.T, path string, ts time.Time) {
	t.Helper()
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}
