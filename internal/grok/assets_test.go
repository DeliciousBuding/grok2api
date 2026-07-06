package grok

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DeliciousBuding/grok2api/internal/config"
)

func TestReadAssetUploadBytesRejectsOversizedRemoteContent(t *testing.T) {
	loadGrokTestConfig(t, "[asset]\nmax_download_bytes = 4\n")

	_, err := readAssetUploadBytes(strings.NewReader("12345"))
	if err == nil {
		t.Fatal("expected oversized asset download to fail")
	}
	if !strings.Contains(err.Error(), "asset download exceeds") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestReadAssetUploadBytesUsesDefaultLimitWhenUnconfigured(t *testing.T) {
	loadGrokTestConfig(t, "[asset]\nmax_download_bytes = 0\n")

	body := strings.NewReader(strings.Repeat("x", defaultAssetMaxDownloadBytes+1))
	_, err := readAssetUploadBytes(body)
	if err == nil {
		t.Fatal("expected default asset download limit to reject oversized content")
	}
}

func loadGrokTestConfig(t *testing.T, body string) {
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
