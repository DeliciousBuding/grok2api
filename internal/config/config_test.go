package config

import (
	"os"
	"path/filepath"
	"strings"
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
		"server.max_header_bytes",
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

func TestLoadRejectsInvalidStatsigPairConfig(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing hex",
			body: "[proxy.clearance]\nstatsig_seed = \"abc\"\n",
			want: "statsig_seed and statsig_hex must be configured together",
		},
		{
			name: "bad seed length",
			body: "[proxy.clearance]\nstatsig_seed = \"abc\"\nstatsig_hex = \"0123456789abcdef\"\n",
			want: "statsig_seed must decode to 48 bytes",
		},
		{
			name: "non hex fingerprint",
			body: "[proxy.clearance]\nstatsig_seed = \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\nstatsig_hex = \"not-hex\"\n",
			want: "statsig_hex must contain only hexadecimal characters",
		},
		{
			name: "oversized hex",
			body: "[proxy.clearance]\nstatsig_seed = \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\nstatsig_hex = \"" + strings.Repeat("a", 513) + "\"\n",
			want: "statsig_hex must be <= 512 characters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			defaults := filepath.Join(dir, "defaults.toml")
			user := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(defaults, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			s := &Snapshot{defaultsPath: defaults, userPath: user, defaultsMtime: -1, userMtime: -1}

			err := s.Load()

			if err == nil {
				t.Fatal("expected invalid statsig config to fail loading")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "not-hex") || strings.Contains(err.Error(), strings.Repeat("a", 32)) {
				t.Fatalf("statsig config error should not echo raw values: %v", err)
			}
		})
	}
}

func TestUpdateRejectsInvalidStatsigPairBeforePersisting(t *testing.T) {
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.toml")
	user := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(defaults, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte("[proxy.clearance]\nstatsig_seed = \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\nstatsig_hex = \"0123456789abcdef\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Snapshot{defaultsPath: defaults, userPath: user, defaultsMtime: -1, userMtime: -1}

	err := s.Update(map[string]any{
		"proxy": map[string]any{
			"clearance": map[string]any{
				"statsig_hex": "not-hex",
			},
		},
	})

	if err == nil {
		t.Fatal("expected invalid runtime statsig update to fail")
	}
	if !strings.Contains(err.Error(), "statsig_hex must contain only hexadecimal characters") {
		t.Fatalf("expected statsig_hex validation error, got %v", err)
	}
	raw, readErr := os.ReadFile(user)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(raw), "not-hex") {
		t.Fatalf("invalid statsig update should not be persisted: %s", raw)
	}
}

func setMtime(t *testing.T, path string, ts time.Time) {
	t.Helper()
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}
