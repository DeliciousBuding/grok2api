package account

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRefreshTokensCoalescesConcurrentSameTokenRefreshes(t *testing.T) {
	ctx := context.Background()
	repo := NewTxtRepository(t.TempDir() + "/accounts.jsonl")
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-a", Pool: "basic"}}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	fetcher := &blockingQuotaFetcher{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service := NewRefreshService(repo, fetcher)

	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, _, err := service.RefreshTokens(ctx, []string{"tok-a"})
			results <- err
		}()
	}
	close(start)

	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("fetcher was not called")
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fetcher.callCount() > 1 {
			t.Fatalf("expected one in-flight upstream fetch for duplicate token, got %d", fetcher.callCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(fetcher.release)

	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("refresh returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("refresh did not finish")
		}
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("expected one upstream fetch after coalescing, got %d", got)
	}
}

func TestRefreshTokensDeduplicatesRepeatedTokensInBatch(t *testing.T) {
	ctx := context.Background()
	repo := NewTxtRepository(t.TempDir() + "/accounts.jsonl")
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-a", Pool: "basic"}}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	fetcher := &countingQuotaFetcher{}
	service := NewRefreshService(repo, fetcher)

	refreshed, failed, err := service.RefreshTokens(ctx, []string{"tok-a", "tok-a", "tok-a"})
	if err != nil {
		t.Fatalf("refresh tokens: %v", err)
	}
	if refreshed != 1 || failed != 0 {
		t.Fatalf("expected one unique refreshed token and no failures, got refreshed=%d failed=%d", refreshed, failed)
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("expected one upstream fetch for repeated token batch, got %d", got)
	}
}

type blockingQuotaFetcher struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *blockingQuotaFetcher) FetchAllQuotas(ctx context.Context, token, pool string, bootstrap bool) (map[int]ModeQuota, error) {
	f.mu.Lock()
	f.calls++
	f.once.Do(func() { close(f.started) })
	f.mu.Unlock()

	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[int]ModeQuota{
		1: {Remaining: 29, Total: 30, WindowSec: BasicFastWindowSec},
	}, nil
}

func (f *blockingQuotaFetcher) FetchModeQuota(ctx context.Context, token, pool string, modeID int) (*ModeQuota, error) {
	return &ModeQuota{Remaining: 29, Total: 30, WindowSec: BasicFastWindowSec}, nil
}

func (f *blockingQuotaFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type countingQuotaFetcher struct {
	mu    sync.Mutex
	calls int
}

func (f *countingQuotaFetcher) FetchAllQuotas(ctx context.Context, token, pool string, bootstrap bool) (map[int]ModeQuota, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return map[int]ModeQuota{
		1: {Remaining: 29, Total: 30, WindowSec: BasicFastWindowSec},
	}, nil
}

func (f *countingQuotaFetcher) FetchModeQuota(ctx context.Context, token, pool string, modeID int) (*ModeQuota, error) {
	return &ModeQuota{Remaining: 29, Total: 30, WindowSec: BasicFastWindowSec}, nil
}

func (f *countingQuotaFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
