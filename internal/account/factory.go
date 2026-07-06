package account

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DeliciousBuding/grok2api/internal/config"
	"github.com/DeliciousBuding/grok2api/internal/platform"
)

// RepositoryInfo describes the selected persistent account backend.
type RepositoryInfo struct {
	Backend string
	Target  string
}

// NewRepositoryFromConfig constructs the account repository selected by
// startup-time configuration.
func NewRepositoryFromConfig(cfg *config.Snapshot) (Repository, RepositoryInfo, error) {
	backend := strings.ToLower(strings.TrimSpace(cfg.GetStr("account.storage.backend", "text")))
	if v := strings.TrimSpace(os.Getenv("ACCOUNT_STORAGE_BACKEND")); v != "" {
		backend = strings.ToLower(v)
	}
	if backend == "" {
		backend = "text"
	}
	switch backend {
	case "text", "jsonl", "local":
		path := resolveLocalAccountPath(cfg)
		return NewTxtRepository(path), RepositoryInfo{Backend: "text", Target: path}, nil
	case "sqlite", "sqlite3":
		path := resolveSQLiteAccountPath(cfg)
		return NewSQLiteRepository(path), RepositoryInfo{Backend: "sqlite", Target: path}, nil
	case "pg+redis", "postgres+redis", "postgresql+redis", "postgres", "postgresql", "pg":
		return nil, RepositoryInfo{Backend: backend}, fmt.Errorf("account storage backend %q is not implemented; use text or sqlite", backend)
	default:
		return nil, RepositoryInfo{Backend: backend}, fmt.Errorf("unsupported account storage backend %q", backend)
	}
}

func resolveLocalAccountPath(cfg *config.Snapshot) string {
	if p := strings.TrimSpace(os.Getenv("ACCOUNT_LOCAL_PATH")); p != "" {
		return p
	}
	if p := strings.TrimSpace(cfg.GetStr("account.local.path", "")); p != "" {
		return p
	}
	return platform.DataPath("accounts.jsonl")
}

func resolveSQLiteAccountPath(cfg *config.Snapshot) string {
	if p := strings.TrimSpace(os.Getenv("ACCOUNT_SQLITE_PATH")); p != "" {
		return p
	}
	if p := strings.TrimSpace(cfg.GetStr("account.sqlite.path", "")); p != "" {
		return p
	}
	return filepath.Join(platform.DataDir(), "accounts.sqlite3")
}
