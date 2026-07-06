package account

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/platform"
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

func TestRefreshTokensInvalidShortTokenDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	repo := NewTxtRepository(t.TempDir() + "/accounts.jsonl")
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "short", Pool: "basic"}}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	fetcher := &invalidUntilTokenQuotaFetcher{successToken: "other"}
	service := NewRefreshService(repo, fetcher)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("invalid short token refresh panicked: %v", r)
		}
	}()
	refreshed, failed, err := service.RefreshTokens(ctx, []string{"short"})
	if err != nil {
		t.Fatalf("refresh tokens: %v", err)
	}
	if refreshed != 0 || failed != 1 {
		t.Fatalf("expected short invalid token to be counted as one failure, got refreshed=%d failed=%d", refreshed, failed)
	}
}

func TestRefreshTokensStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repo := NewTxtRepository(t.TempDir() + "/accounts.jsonl")
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(ctx, []Upsert{
		{Token: "tok-a", Pool: "basic"},
		{Token: "tok-b", Pool: "basic"},
	}); err != nil {
		t.Fatalf("upsert accounts: %v", err)
	}
	fetcher := &cancelingQuotaFetcher{cancel: cancel}
	service := NewRefreshService(repo, fetcher)

	refreshed, failed, err := service.RefreshTokens(ctx, []string{"tok-a", "tok-b"})
	if err != nil {
		t.Fatalf("refresh tokens should report counts without returning a batch error: %v", err)
	}
	if refreshed != 0 || failed != 1 {
		t.Fatalf("expected only first canceled token to be counted, got refreshed=%d failed=%d", refreshed, failed)
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("expected context cancellation to stop remaining token refreshes, got %d upstream calls", got)
	}
}

func TestRefreshScheduledRefreshesAccountsBeyondFirstPage(t *testing.T) {
	ctx := context.Background()
	const accountCount = 3
	repo := newScheduledRefreshRepo(accountCount)
	fetcher := &countingQuotaFetcher{}
	service := NewRefreshService(repo, fetcher)

	refreshed, failed, err := service.RefreshScheduled(ctx, "")
	if err != nil {
		t.Fatalf("refresh scheduled: %v", err)
	}
	if refreshed != accountCount || failed != 0 {
		t.Fatalf("expected all accounts refreshed without failures, got refreshed=%d failed=%d", refreshed, failed)
	}
	if got := fetcher.uniqueCallCount(); got != accountCount {
		t.Fatalf("expected %d unique upstream fetches, got %d", accountCount, got)
	}
	if !fetcher.called("token-0002") {
		t.Fatal("expected account beyond first page to be refreshed")
	}
}

func TestRefreshScheduledDoesNotSkipRemainingAccountsWhenEarlierPageExpires(t *testing.T) {
	ctx := context.Background()
	const accountCount = 3
	repo := newScheduledRefreshRepo(accountCount)
	fetcher := &invalidUntilTokenQuotaFetcher{successToken: "token-0002"}
	service := NewRefreshService(repo, fetcher)

	refreshed, failed, err := service.RefreshScheduled(ctx, "")
	if err != nil {
		t.Fatalf("refresh scheduled: %v", err)
	}
	if refreshed != 1 || failed != accountCount-1 {
		t.Fatalf("expected trailing account refreshed after earlier expirations, got refreshed=%d failed=%d", refreshed, failed)
	}
	if !fetcher.called("token-0002") {
		t.Fatal("expected account after expired first page to be attempted")
	}
}

func TestRefreshScheduledStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	const accountCount = 3
	repo := newScheduledRefreshRepo(accountCount)
	fetcher := &cancelingQuotaFetcher{cancel: cancel}
	service := NewRefreshService(repo, fetcher)

	refreshed, failed, err := service.RefreshScheduled(ctx, "")
	if err != nil {
		t.Fatalf("refresh scheduled should report counts without returning a batch error: %v", err)
	}
	if refreshed != 0 || failed != 1 {
		t.Fatalf("expected only first canceled account to be counted, got refreshed=%d failed=%d", refreshed, failed)
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("expected context cancellation to stop remaining scheduled refreshes, got %d upstream calls", got)
	}
}

func TestRefreshOnDemandCanceledContextDoesNotConsumeThrottleWindow(t *testing.T) {
	repo := newScheduledRefreshRepo(1)
	fetcher := &countingQuotaFetcher{}
	service := NewRefreshService(repo, fetcher)
	service.minOnDemandDelta = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.RefreshOnDemand(ctx); err == nil {
		t.Fatal("expected canceled on-demand refresh to return context error")
	}

	if err := service.RefreshOnDemand(context.Background()); err != nil {
		t.Fatalf("expected next on-demand refresh to run: %v", err)
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("expected canceled refresh not to consume throttle window, got %d upstream calls", got)
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

type cancelingQuotaFetcher struct {
	mu     sync.Mutex
	calls  int
	cancel context.CancelFunc
}

func (f *cancelingQuotaFetcher) FetchAllQuotas(ctx context.Context, token, pool string, bootstrap bool) (map[int]ModeQuota, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
	}
	return nil, ctx.Err()
}

func (f *cancelingQuotaFetcher) FetchModeQuota(ctx context.Context, token, pool string, modeID int) (*ModeQuota, error) {
	return nil, ctx.Err()
}

func (f *cancelingQuotaFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type countingQuotaFetcher struct {
	mu     sync.Mutex
	calls  int
	tokens map[string]int
}

func (f *countingQuotaFetcher) FetchAllQuotas(ctx context.Context, token, pool string, bootstrap bool) (map[int]ModeQuota, error) {
	f.recordCall(token)
	return map[int]ModeQuota{
		1: {Remaining: 29, Total: 30, WindowSec: BasicFastWindowSec},
	}, nil
}

func (f *countingQuotaFetcher) recordCall(token string) {
	f.mu.Lock()
	f.calls++
	if f.tokens == nil {
		f.tokens = map[string]int{}
	}
	f.tokens[token]++
	f.mu.Unlock()
}

func (f *countingQuotaFetcher) FetchModeQuota(ctx context.Context, token, pool string, modeID int) (*ModeQuota, error) {
	return &ModeQuota{Remaining: 29, Total: 30, WindowSec: BasicFastWindowSec}, nil
}

func (f *countingQuotaFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *countingQuotaFetcher) uniqueCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokens)
}

func (f *countingQuotaFetcher) called(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[token] > 0
}

type invalidUntilTokenQuotaFetcher struct {
	countingQuotaFetcher
	successToken string
}

func (f *invalidUntilTokenQuotaFetcher) FetchAllQuotas(ctx context.Context, token, pool string, bootstrap bool) (map[int]ModeQuota, error) {
	f.recordCall(token)
	if token != f.successToken {
		return nil, platform.UpstreamError("invalid credentials", 401, "token expired")
	}
	return map[int]ModeQuota{
		1: {Remaining: 29, Total: 30, WindowSec: BasicFastWindowSec},
	}, nil
}

func (f *invalidUntilTokenQuotaFetcher) FetchModeQuota(ctx context.Context, token, pool string, modeID int) (*ModeQuota, error) {
	return &ModeQuota{Remaining: 29, Total: 30, WindowSec: BasicFastWindowSec}, nil
}

type scheduledRefreshRepo struct {
	records       []*Record
	clock         int64
	pageSizeLimit int
}

func newScheduledRefreshRepo(count int) *scheduledRefreshRepo {
	records := make([]*Record, count)
	for i := range records {
		rec := NewRecord(fmt.Sprintf("token-%04d", i))
		rec.UpdatedAt = int64(i)
		records[i] = rec
	}
	return &scheduledRefreshRepo{records: records, clock: int64(count), pageSizeLimit: 2}
}

func (r *scheduledRefreshRepo) Initialize(ctx context.Context) error { return nil }

func (r *scheduledRefreshRepo) GetRevision(ctx context.Context) (int, error) { return 0, nil }

func (r *scheduledRefreshRepo) RuntimeSnapshot(ctx context.Context) (*Snapshot, error) {
	return &Snapshot{}, nil
}

func (r *scheduledRefreshRepo) ScanChanges(ctx context.Context, since int, limit int) (*ChangeSet, error) {
	return &ChangeSet{}, nil
}

func (r *scheduledRefreshRepo) UpsertAccounts(ctx context.Context, items []Upsert) (*MutationResult, error) {
	return &MutationResult{}, nil
}

func (r *scheduledRefreshRepo) PatchAccounts(ctx context.Context, patches []Patch) (*MutationResult, error) {
	for _, patch := range patches {
		for _, rec := range r.records {
			if rec.Token != patch.Token {
				continue
			}
			r.clock++
			rec.UpdatedAt = r.clock
			if patch.UsageSyncDelta != nil {
				rec.UsageSyncCount += *patch.UsageSyncDelta
			}
			if patch.Status != nil {
				rec.Status = *patch.Status
			}
			if patch.LastSyncAt != nil {
				rec.LastSyncAt = patch.LastSyncAt
			}
			break
		}
	}
	return &MutationResult{Patched: len(patches)}, nil
}

func (r *scheduledRefreshRepo) DeleteAccounts(ctx context.Context, tokens []string) (*MutationResult, error) {
	return &MutationResult{}, nil
}

func (r *scheduledRefreshRepo) GetAccounts(ctx context.Context, tokens []string) ([]*Record, error) {
	byToken := make(map[string]*Record, len(r.records))
	for _, rec := range r.records {
		byToken[rec.Token] = rec
	}
	out := make([]*Record, 0, len(tokens))
	for _, tok := range tokens {
		if rec := byToken[tok]; rec != nil {
			cp := *rec
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *scheduledRefreshRepo) ListAccounts(ctx context.Context, q ListQuery) (*Page, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = 50
	}
	if r.pageSizeLimit > 0 && q.PageSize > r.pageSizeLimit {
		q.PageSize = r.pageSizeLimit
	} else if q.PageSize > 2000 {
		q.PageSize = 2000
	}
	items := make([]*Record, 0, len(r.records))
	for _, rec := range r.records {
		if q.Pool != "" && rec.Pool != q.Pool {
			continue
		}
		if q.Status != nil && rec.Status != *q.Status {
			continue
		}
		cp := *rec
		items = append(items, &cp)
	}
	sort.Slice(items, func(i, j int) bool {
		var less bool
		if q.SortBy == "token" {
			less = items[i].Token < items[j].Token
		} else {
			less = items[i].UpdatedAt < items[j].UpdatedAt
		}
		if q.SortDesc {
			return !less
		}
		return less
	})
	total := len(items)
	totalPages := 1
	if total > 0 {
		totalPages = (total + q.PageSize - 1) / q.PageSize
	}
	offset := (q.Page - 1) * q.PageSize
	if offset > total {
		offset = total
	}
	end := offset + q.PageSize
	if end > total {
		end = total
	}
	return &Page{
		Items:      items[offset:end],
		Total:      total,
		Page:       q.Page,
		PageSize:   q.PageSize,
		TotalPages: totalPages,
	}, nil
}

func (r *scheduledRefreshRepo) ReplacePool(ctx context.Context, pool string, upserts []Upsert) (*MutationResult, error) {
	return &MutationResult{}, nil
}

func (r *scheduledRefreshRepo) ResetExpiredConsoleWindows(ctx context.Context) (int, error) {
	return 0, nil
}

func (r *scheduledRefreshRepo) RecoverConsoleExpiredAccounts(ctx context.Context) (int, error) {
	return 0, nil
}

func (r *scheduledRefreshRepo) Close(ctx context.Context) error { return nil }
