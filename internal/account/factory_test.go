package account

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DeliciousBuding/grok2api/internal/config"
)

func TestNewRepositoryFromConfigSelectsSQLiteBackend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.toml")
	user := filepath.Join(dir, "config.toml")
	sqlitePath := filepath.Join(dir, "accounts.sqlite3")
	if err := os.WriteFile(defaults, []byte(`
[account.storage]
backend = "sqlite"
[account.sqlite]
path = "`+strings.ReplaceAll(sqlitePath, `\`, `\\`)+`"
`), 0o644); err != nil {
		t.Fatalf("write defaults: %v", err)
	}
	config.SetPaths(defaults, user)
	if err := config.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}

	repo, info, err := NewRepositoryFromConfig(config.Global())
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	if info.Backend != "sqlite" || info.Target != sqlitePath {
		t.Fatalf("unexpected repository info: %#v", info)
	}
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize selected repo: %v", err)
	}
	_ = repo.Close(ctx)
}

func TestNewRepositoryFromConfigAllowsBackendEnvOverride(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.toml")
	user := filepath.Join(dir, "config.toml")
	sqlitePath := filepath.Join(dir, "accounts.sqlite3")
	if err := os.WriteFile(defaults, []byte(`
[account.storage]
backend = "text"
[account.sqlite]
path = "`+strings.ReplaceAll(sqlitePath, `\`, `\\`)+`"
`), 0o644); err != nil {
		t.Fatalf("write defaults: %v", err)
	}
	t.Setenv("ACCOUNT_STORAGE_BACKEND", "sqlite")
	config.SetPaths(defaults, user)
	if err := config.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}

	repo, info, err := NewRepositoryFromConfig(config.Global())
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	if info.Backend != "sqlite" || info.Target != sqlitePath {
		t.Fatalf("unexpected repository info: %#v", info)
	}
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize selected repo: %v", err)
	}
	_ = repo.Close(ctx)
}

func TestNewRepositoryFromConfigRejectsUnsupportedDistributedBackend(t *testing.T) {
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.toml")
	user := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(defaults, []byte(`
[account.storage]
backend = "pg+redis"
`), 0o644); err != nil {
		t.Fatalf("write defaults: %v", err)
	}
	config.SetPaths(defaults, user)
	if err := config.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}

	repo, _, err := NewRepositoryFromConfig(config.Global())
	if err == nil {
		_ = repo.Close(context.Background())
		t.Fatal("expected pg+redis to fail fast until implemented")
	}
	if !strings.Contains(err.Error(), "pg+redis") || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("unexpected error: %v", err)
	}
}
