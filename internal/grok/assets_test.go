package grok

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestConfiguredAssetDownloadMaxBytesClampsMisconfiguredLargeValue(t *testing.T) {
	loadGrokTestConfig(t, "[asset]\nmax_download_bytes = 1073741824\n")

	if got := configuredAssetDownloadMaxBytes(); got != maxAssetDownloadBytes {
		t.Fatalf("expected asset download byte limit to clamp to %d, got %d", maxAssetDownloadBytes, got)
	}
}

func TestConfiguredGrokOperationTimeoutClampsMisconfiguredLargeValue(t *testing.T) {
	loadGrokTestConfig(t, "[asset]\nupload_timeout = 86400\nlist_timeout = 86400\n[nsfw]\ntimeout = 86400\n")

	if got := configuredGrokOperationTimeout("asset.upload_timeout", 60); got != maxGrokOperationTimeout {
		t.Fatalf("expected upload timeout to clamp to %v, got %v", maxGrokOperationTimeout, got)
	}
	if got := configuredGrokOperationTimeout("asset.list_timeout", 60); got != maxGrokOperationTimeout {
		t.Fatalf("expected list timeout to clamp to %v, got %v", maxGrokOperationTimeout, got)
	}
	if got := configuredGrokOperationTimeout("nsfw.timeout", 30); got != maxGrokOperationTimeout {
		t.Fatalf("expected nsfw timeout to clamp to %v, got %v", maxGrokOperationTimeout, got)
	}
}

func TestConfiguredGrokOperationTimeoutUsesDefaultForInvalidValues(t *testing.T) {
	loadGrokTestConfig(t, "[asset]\nupload_timeout = -1\n")

	if got := configuredGrokOperationTimeout("asset.upload_timeout", 60); got != 60*time.Second {
		t.Fatalf("expected invalid upload timeout to use default, got %v", got)
	}
	if got := configuredGrokOperationTimeout("asset.delete_timeout", 60); got != 60*time.Second {
		t.Fatalf("expected missing delete timeout to use default, got %v", got)
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
