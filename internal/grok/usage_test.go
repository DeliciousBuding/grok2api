package grok

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/account"
	"github.com/DeliciousBuding/grok2api/internal/platform"
)

func TestFetchAllQuotaModesShortCircuitsInvalidCredentials(t *testing.T) {
	modes := []int{0, 1, 2, 3, 4}
	started := make(chan int, len(modes))
	cancelled := make(chan int, len(modes))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := fetchAllQuotaModes(ctx, modes, func(ctx context.Context, modeID int) (*account.ModeQuota, error) {
		started <- modeID
		if modeID == 0 {
			return nil, platform.UpstreamError("Upstream returned 401", 401, "invalid-credentials")
		}
		<-ctx.Done()
		cancelled <- modeID
		return nil, ctx.Err()
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected invalid credentials error")
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) || appErr.Status != 401 {
		t.Fatalf("expected 401 app error, got %T %[1]v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected invalid credentials to cancel remaining mode fetches quickly, took %s", elapsed)
	}

	for range modes {
		<-started
	}
	for i := 0; i < len(modes)-1; i++ {
		select {
		case <-cancelled:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("expected blocked mode fetch %d to observe cancellation", i+1)
		}
	}
}

func TestFetchAllQuotaModesSkipsFanoutWhenContextAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int32

	_, err := fetchAllQuotaModes(ctx, []int{0, 1, 2, 3, 4}, func(ctx context.Context, modeID int) (*account.ModeQuota, error) {
		atomic.AddInt32(&calls, 1)
		return nil, ctx.Err()
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("expected canceled context to skip quota fanout, got %d calls", got)
	}
}

func TestFetchAllQuotaModesReturnsContextErrorWhenAllModesCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancelOnce := sync.Once{}

	_, err := fetchAllQuotaModes(ctx, []int{0, 1, 2, 3, 4}, func(ctx context.Context, modeID int) (*account.ModeQuota, error) {
		cancelOnce.Do(cancel)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled when every quota mode failed from cancellation, got %v", err)
	}
}
