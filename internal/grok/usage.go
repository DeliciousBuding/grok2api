package grok

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/account"
	"github.com/DeliciousBuding/grok2api/internal/platform"
)

// modeNames maps modeID → the request modelName for /rest/rate-limits.
var modeNames = map[int]string{
	0: "auto",
	1: "fast",
	2: "expert",
	3: "heavy",
	4: "grok-420-computer-use-sa",
}

// defaultWindowSecs is the fallback window duration when the API omits it.
var defaultWindowSecs = map[int]int{
	0: 7200,  // auto   — 2 h
	1: 86400, // fast   — 24 h
	2: 7200,  // expert — 2 h
	3: 7200,  // heavy  — 2 h
	4: 7200,  // grok_4_3 — 2 h
}

// RateLimitsResponse represents the upstream rate-limits payload shape.
type RateLimitsResponse struct {
	WindowSizeSeconds    int  `json:"windowSizeSeconds"`
	RemainingQueries     int  `json:"remainingQueries"`
	TotalQueries         int  `json:"totalQueries"`
	LowEffortRateLimits  *any `json:"lowEffortRateLimits"`
	HighEffortRateLimits *any `json:"highEffortRateLimits"`
}

// ParseRateLimits converts the flat rate-limits body to a ModeQuota.
// Returns nil when required fields are missing.
func ParseRateLimits(body map[string]any, defaultWindowSecs int) *account.ModeQuota {
	remaining, ok := body["remainingQueries"].(float64)
	if !ok {
		return nil
	}
	total := 0
	if t, ok := body["totalQueries"].(float64); ok {
		total = int(t)
	} else {
		total = int(remaining)
	}
	window := defaultWindowSecs
	if w, ok := body["windowSizeSeconds"].(float64); ok && int(w) > 0 {
		window = int(w)
	}
	syncedAt := platform.NowMs()
	resetAt := syncedAt + int64(window)*1000
	return &account.ModeQuota{
		Remaining: int(remaining),
		Total:     total,
		WindowSec: window,
		ResetAtMs: &resetAt,
	}
}

// UsageFetcher implements account.UsageFetcher by hitting /rest/rate-limits.
type UsageFetcher struct {
	transport *Transport
}

// NewUsageFetcher wraps a Transport with the UsageFetcher interface.
func NewUsageFetcher(t *Transport) *UsageFetcher { return &UsageFetcher{transport: t} }

// FetchAllQuotas fetches quota windows for all supported modes concurrently.
// Returns {modeID: ModeQuota} for every successful response, or nil if every
// mode failed. A single invalid-credentials error short-circuits and is
// returned so the caller can mark the account expired.
func (f *UsageFetcher) FetchAllQuotas(ctx context.Context, token, _ string, _ bool) (map[int]account.ModeQuota, error) {
	modes := []int{0, 1, 2, 3, 4}
	return fetchAllQuotaModes(ctx, modes, func(ctx context.Context, modeID int) (*account.ModeQuota, error) {
		return f.fetchOne(ctx, token, modeID)
	})
}

type quotaFetchFunc func(context.Context, int) (*account.ModeQuota, error)

func fetchAllQuotaModes(ctx context.Context, modes []int, fetch quotaFetchFunc) (map[int]account.ModeQuota, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		modeID int
		quota  *account.ModeQuota
		err    error
	}
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resultsCh := make(chan result, len(modes))
	var wg sync.WaitGroup
	for _, modeID := range modes {
		wg.Add(1)
		go func(modeID int) {
			defer wg.Done()
			q, err := fetch(reqCtx, modeID)
			resultsCh <- result{modeID: modeID, quota: q, err: err}
		}(modeID)
	}
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var results []result
	var invalidCredentialsErr error
	for r := range resultsCh {
		results = append(results, r)
		if invalidCredentialsErr == nil && isInvalidCredentialsQuotaError(r.err) {
			invalidCredentialsErr = r.err
			cancel()
		}
	}

	if invalidCredentialsErr != nil {
		return nil, invalidCredentialsErr
	}

	out := map[int]account.ModeQuota{}
	for _, r := range results {
		if r.quota == nil {
			continue
		}
		out[r.modeID] = *r.quota
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func isInvalidCredentialsQuotaError(err error) bool {
	if err == nil {
		return false
	}
	appErr, ok := err.(*platform.AppError)
	if !ok {
		return false
	}
	return (appErr.Status == 400 || appErr.Status == 401 || appErr.Status == 403) &&
		platform.IsInvalidCredentialsBody(appErr.Body)
}

// FetchModeQuota fetches the quota for a single mode.
func (f *UsageFetcher) FetchModeQuota(ctx context.Context, token, _ string, modeID int) (*account.ModeQuota, error) {
	return f.fetchOne(ctx, token, modeID)
}

func (f *UsageFetcher) fetchOne(ctx context.Context, token string, modeID int) (*account.ModeQuota, error) {
	modeName := modeNames[modeID]
	if modeName == "" {
		modeName = "auto"
	}
	payload := []byte(fmt.Sprintf(`{"modelName":%q}`, modeName))
	// 25s timeout per the Python implementation.
	reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	body, err := f.transport.PostJSON(reqCtx, RateLimits, token, payload,
		WithTimeout(20*time.Second),
	)
	if err != nil {
		return nil, err
	}
	def := defaultWindowSecs[modeID]
	if def == 0 {
		def = 72000
	}
	q := ParseRateLimits(body, def)
	if q == nil {
		// Body lacks required fields — return nil to indicate "no data".
		return nil, nil
	}
	return q, nil
}
