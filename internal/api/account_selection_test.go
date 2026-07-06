package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/account"
	"github.com/DeliciousBuding/grok2api/internal/config"
	"github.com/DeliciousBuding/grok2api/internal/model"
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

func TestShouldRetryUpstreamFindsJoinedAppError(t *testing.T) {
	loadTestConfig(t, "[retry]\non_codes = [\"503\"]\n")
	err := errors.Join(errors.New("stream closed"), platform.UpstreamError("unavailable", http.StatusServiceUnavailable, ""))

	if !shouldRetryUpstream(err) {
		t.Fatal("joined upstream AppError should remain retryable")
	}
}

func TestReserveAccountUsesPreferredTags(t *testing.T) {
	spec, ok := model.Resolve("grok-4.20-fast")
	if !ok {
		t.Fatal("resolve test model")
	}
	repo := &snapshotRepo{items: []*account.Record{
		accountSelectionTestRecord("tok-untagged", nil, 30),
		accountSelectionTestRecord("tok-tagged", []string{"tenant-a"}, 1),
	}}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}

	lease, modeID := reserveAccount(context.Background(), dir, spec, nil, []string{"tenant-a"})
	if lease == nil {
		t.Fatal("reserveAccount returned nil")
	}
	if lease.Token != "tok-tagged" {
		t.Fatalf("expected tagged account, got %q", lease.Token)
	}
	if modeID != int(model.ModeFast) {
		t.Fatalf("expected fast mode, got %d", modeID)
	}
}

func TestReserveAccountHonorsCanceledContext(t *testing.T) {
	spec, ok := model.Resolve("grok-4.20-fast")
	if !ok {
		t.Fatal("resolve test model")
	}
	repo := &snapshotRepo{items: []*account.Record{
		accountSelectionTestRecord("tok-ready", nil, 30),
	}}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lease, modeID := reserveAccount(ctx, dir, spec, nil, nil)

	if lease != nil {
		t.Fatalf("expected canceled context to skip account reservation, got token=%q", lease.Token)
	}
	if modeID != int(model.ModeFast) {
		t.Fatalf("expected fallback mode id %d, got %d", int(model.ModeFast), modeID)
	}
	for _, slot := range dir.Snapshot() {
		if slot.Inflight != 0 {
			t.Fatalf("canceled reservation should not increment inflight, snapshot=%#v", dir.Snapshot())
		}
	}
}

func TestChatCompletionRequestPreferTagsNormalizesInput(t *testing.T) {
	req := &chatCompletionRequest{Grok2APIPreferTags: []string{"tenant-b", "", "tenant-a", "tenant-a"}}

	got := req.preferTags()

	want := []string{"tenant-a", "tenant-b"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
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

func accountSelectionTestRecord(token string, tags []string, remaining int) *account.Record {
	rec := account.NewRecord(token)
	rec.Tags = tags
	quota := account.QuotaSet{}
	quota.Set(int(model.ModeFast), account.QuotaWindow{
		Total:         remaining,
		Remaining:     remaining,
		WindowSeconds: account.BasicFastWindowSec,
	})
	rec.Quota = quota.ToMap()
	return rec
}
