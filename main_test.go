package main

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/config"
)

func TestHTTPServerConfigAppliesConfiguredTimeouts(t *testing.T) {
	loadMainTestConfig(t, `[server]
read_header_timeout_sec = 11
read_timeout_sec = 12
write_timeout_sec = 13
idle_timeout_sec = 14
shutdown_timeout_sec = 15
max_header_bytes = 2048
`)

	srv := newHTTPServerFromConfig("127.0.0.1:0", http.NewServeMux(), config.Global())

	if srv.ReadHeaderTimeout != 11*time.Second {
		t.Fatalf("expected read header timeout 11s, got %v", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 12*time.Second {
		t.Fatalf("expected read timeout 12s, got %v", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 13*time.Second {
		t.Fatalf("expected write timeout 13s, got %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 14*time.Second {
		t.Fatalf("expected idle timeout 14s, got %v", srv.IdleTimeout)
	}
	if got := serverShutdownTimeout(config.Global()); got != 15*time.Second {
		t.Fatalf("expected shutdown timeout 15s, got %v", got)
	}
	if srv.MaxHeaderBytes != 2048 {
		t.Fatalf("expected max header bytes 2048, got %d", srv.MaxHeaderBytes)
	}
}

func TestHTTPServerConfigFallsBackForInvalidTimeouts(t *testing.T) {
	loadMainTestConfig(t, `[server]
read_header_timeout_sec = -1
read_timeout_sec = -1
write_timeout_sec = -1
idle_timeout_sec = -1
shutdown_timeout_sec = -1
max_header_bytes = -1
`)

	srv := newHTTPServerFromConfig("127.0.0.1:0", http.NewServeMux(), config.Global())

	if srv.ReadHeaderTimeout != defaultReadHeaderTimeout {
		t.Fatalf("expected default read header timeout, got %v", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != defaultReadTimeout {
		t.Fatalf("expected default read timeout, got %v", srv.ReadTimeout)
	}
	if srv.WriteTimeout != defaultWriteTimeout {
		t.Fatalf("expected default write timeout, got %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("expected default idle timeout, got %v", srv.IdleTimeout)
	}
	if got := serverShutdownTimeout(config.Global()); got != defaultShutdownTimeout {
		t.Fatalf("expected default shutdown timeout, got %v", got)
	}
	if srv.MaxHeaderBytes != defaultMaxHeaderBytes {
		t.Fatalf("expected default max header bytes, got %d", srv.MaxHeaderBytes)
	}
}

func TestDirectorySyncIntervalsFallBackForNonPositiveValues(t *testing.T) {
	idle, active := directorySyncIntervals(0, -5)

	if idle != defaultDirectorySyncIdleInterval {
		t.Fatalf("expected default idle sync interval, got %d", idle)
	}
	if active != defaultDirectorySyncActiveInterval {
		t.Fatalf("expected default active sync interval, got %d", active)
	}
}

func TestDirectorySyncIntervalsKeepPositiveValues(t *testing.T) {
	idle, active := directorySyncIntervals(45, 7)

	if idle != 45 {
		t.Fatalf("expected idle sync interval 45, got %d", idle)
	}
	if active != 7 {
		t.Fatalf("expected active sync interval 7, got %d", active)
	}
}

func TestConsoleLoopsTolerateNonPositiveIntervalsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mustNotPanic(t, "console reset loop", func() {
		runConsoleResetLoop(ctx, nil, 0)
	})
	mustNotPanic(t, "console recovery loop", func() {
		runConsoleRecoveryLoop(ctx, nil, -5)
	})
}

func mustNotPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", name, r)
		}
	}()
	fn()
}

func loadMainTestConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	defaults := dir + "/defaults.toml"
	user := dir + "/config.toml"
	if err := os.WriteFile(defaults, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config.SetPaths(defaults, user)
	if err := config.Load(); err != nil {
		t.Fatal(err)
	}
}
