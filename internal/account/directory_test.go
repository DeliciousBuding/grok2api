package account

import (
	"context"
	"math"
	"path/filepath"
	"testing"
)

func TestQuotaSelectionPrefersMatchingTagsWithinPoolMode(t *testing.T) {
	dir := NewDirectory(nil)
	reset := int64(9999999999999)
	untagged := directoryTestSlot("tok-untagged", PoolBasic, 1, 30, reset, nil)
	tagged := directoryTestSlot("tok-tagged", PoolBasic, 1, 1, reset, []string{"paid", "tenant-a"})
	dir.slots = map[string]*Slot{
		untagged.Token: untagged,
		tagged.Token:   tagged,
	}
	dir.byMode = map[modeKey]map[string]struct{}{
		{pool: PoolBasic, modeID: 1}: {
			untagged.Token: struct{}{},
			tagged.Token:   struct{}{},
		},
	}

	lease, _ := dir.Reserve([]int{int(PoolBasic)}, 1, nil, []string{"tenant-a", "paid"})
	if lease == nil {
		t.Fatal("reserve returned nil")
	}
	if lease.Token != tagged.Token {
		t.Fatalf("expected preferred tagged account %q, got %q", tagged.Token, lease.Token)
	}
}

func TestQuotaSelectionFallsBackWhenNoCandidateMatchesPreferredTags(t *testing.T) {
	dir := NewDirectory(nil)
	reset := int64(9999999999999)
	stronger := directoryTestSlot("tok-stronger", PoolBasic, 1, 30, reset, nil)
	weaker := directoryTestSlot("tok-weaker", PoolBasic, 1, 1, reset, []string{"tenant-a"})
	dir.slots = map[string]*Slot{
		stronger.Token: stronger,
		weaker.Token:   weaker,
	}
	dir.byMode = map[modeKey]map[string]struct{}{
		{pool: PoolBasic, modeID: 1}: {
			stronger.Token: struct{}{},
			weaker.Token:   struct{}{},
		},
	}

	lease, _ := dir.Reserve([]int{int(PoolBasic)}, 1, nil, []string{"tenant-b"})
	if lease == nil {
		t.Fatal("reserve returned nil")
	}
	if lease.Token != stronger.Token {
		t.Fatalf("expected best available fallback account %q, got %q", stronger.Token, lease.Token)
	}
}

func TestRandomSelectionPrefersMatchingTags(t *testing.T) {
	dir := NewDirectory(nil)
	reset := int64(9999999999999)
	untagged := directoryTestSlot("tok-untagged", PoolBasic, 1, 30, reset, nil)
	tagged := directoryTestSlot("tok-tagged", PoolBasic, 5, 20, reset, []string{"tenant-a"})
	dir.slots = map[string]*Slot{
		untagged.Token: untagged,
		tagged.Token:   tagged,
	}
	dir.byMode = map[modeKey]map[string]struct{}{
		{pool: PoolBasic, modeID: 1}: {untagged.Token: struct{}{}},
		{pool: PoolBasic, modeID: 5}: {tagged.Token: struct{}{}},
	}

	got := dir.randomSelectLocked(int(PoolBasic), 1, nil, newTagPreference([]string{"tenant-a"}), 0)
	if got == nil {
		t.Fatal("randomSelectLocked returned nil")
	}
	if got.Token != tagged.Token {
		t.Fatalf("expected preferred tagged account %q, got %q", tagged.Token, got.Token)
	}
}

func TestRandomSelectionDeduplicatesAccountsAcrossModeBuckets(t *testing.T) {
	dir := NewDirectory(nil)
	reset := int64(9999999999999)
	multiMode := directoryTestSlot("tok-multi-mode", PoolBasic, 1, 30, reset, nil)
	multiMode.Quota.Set(5, QuotaWindow{
		Total:         20,
		Remaining:     20,
		WindowSeconds: 3600,
		ResetAt:       &reset,
	})
	singleMode := directoryTestSlot("tok-single-mode", PoolBasic, 1, 30, reset, nil)
	dir.slots = map[string]*Slot{
		multiMode.Token:  multiMode,
		singleMode.Token: singleMode,
	}
	dir.byMode = map[modeKey]map[string]struct{}{
		{pool: PoolBasic, modeID: 1}: {
			multiMode.Token:  struct{}{},
			singleMode.Token: struct{}{},
		},
		{pool: PoolBasic, modeID: 5}: {
			multiMode.Token: struct{}{},
		},
	}

	counts := map[string]int{}
	const trials = 12000
	for range trials {
		got := dir.randomSelectLocked(int(PoolBasic), 1, nil, nil, 0)
		if got == nil {
			t.Fatal("randomSelectLocked returned nil")
		}
		counts[got.Token]++
	}

	diff := math.Abs(float64(counts[multiMode.Token] - counts[singleMode.Token]))
	if diff > trials*0.10 {
		t.Fatalf("expected roughly even deduplicated random selection, counts=%v diff=%.0f", counts, diff)
	}
}

func TestQuotaSelectionRespectsConfiguredMaxInflight(t *testing.T) {
	dir := NewDirectory(nil)
	dir.SetMaxInflight(2)

	reset := int64(9999999999999)
	slot := &Slot{
		Token:    "tok-1",
		PoolID:   PoolBasic,
		StatusID: StatusIDActive,
		Quota: QuotaSet{
			Fast: QuotaWindow{
				Total:         30,
				Remaining:     30,
				WindowSeconds: 86400,
				ResetAt:       &reset,
			},
		},
		Health: 1.0,
	}
	dir.slots = map[string]*Slot{slot.Token: slot}
	dir.byMode = map[modeKey]map[string]struct{}{
		{pool: PoolBasic, modeID: 1}: {slot.Token: struct{}{}},
	}

	if lease, _ := dir.Reserve([]int{int(PoolBasic)}, 1, nil, nil); lease == nil {
		t.Fatal("first reserve returned nil")
	}
	if lease, _ := dir.Reserve([]int{int(PoolBasic)}, 1, nil, nil); lease == nil {
		t.Fatal("second reserve returned nil")
	}
	if lease, _ := dir.Reserve([]int{int(PoolBasic)}, 1, nil, nil); lease != nil {
		t.Fatalf("third reserve should respect configured max inflight, got %#v", lease)
	}
}

func TestReserveAnyUsesQuotaStrategyModeBuckets(t *testing.T) {
	dir := NewDirectory(nil)
	dir.SetMaxInflight(1)

	reset := int64(9999999999999)
	slot := directoryTestSlot("tok-any", PoolBasic, 1, 30, reset, nil)
	dir.slots = map[string]*Slot{slot.Token: slot}
	dir.byMode = map[modeKey]map[string]struct{}{
		{pool: PoolBasic, modeID: 1}: {slot.Token: struct{}{}},
	}

	lease := dir.ReserveAny([]int{int(PoolBasic)}, nil)
	if lease == nil {
		t.Fatal("ReserveAny should select an account with any available quota mode")
	}
	if lease.Token != slot.Token {
		t.Fatalf("expected token %q, got %q", slot.Token, lease.Token)
	}
	if lease.ModeID != 1 {
		t.Fatalf("expected selected mode 1, got %d", lease.ModeID)
	}
	if slot.Inflight != 1 {
		t.Fatalf("expected inflight lease count 1, got %d", slot.Inflight)
	}
	if lease := dir.ReserveAny([]int{int(PoolBasic)}, nil); lease != nil {
		t.Fatalf("second ReserveAny should respect max inflight, got %#v", lease)
	}
}

func TestSetMaxInflightIgnoresInvalidValues(t *testing.T) {
	dir := NewDirectory(nil)
	dir.SetMaxInflight(0)

	if dir.maxInflight != quotaMaxInflight {
		t.Fatalf("invalid max inflight should reset to default %d, got %d", quotaMaxInflight, dir.maxInflight)
	}
}

func TestSetMaxInflightClampsMisconfiguredLargeValue(t *testing.T) {
	dir := NewDirectory(nil)
	dir.SetMaxInflight(10_000)

	if dir.maxInflight != maxAccountSelectionInflight {
		t.Fatalf("large max inflight should clamp to %d, got %d", maxAccountSelectionInflight, dir.maxInflight)
	}
}

func TestDirectorySyncAdvancesRevisionOnNoopRepositoryChange(t *testing.T) {
	ctx := context.Background()
	repo := NewTxtRepository(filepath.Join(t.TempDir(), "accounts.jsonl"))
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize repo: %v", err)
	}
	dir := NewDirectory(repo)
	if err := dir.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap directory: %v", err)
	}

	status := StatusDisabled
	result, err := repo.PatchAccounts(ctx, []Patch{{Token: "missing-token", Status: &status}})
	if err != nil {
		t.Fatalf("patch missing account: %v", err)
	}
	changed, err := dir.SyncIfChanged(ctx)
	if err != nil {
		t.Fatalf("sync directory: %v", err)
	}
	if changed {
		t.Fatal("no-op repository change should not report directory item changes")
	}
	if dir.Revision() != result.Revision {
		t.Fatalf("expected directory revision %d, got %d", result.Revision, dir.Revision())
	}
}

func TestDirectorySnapshotReturnsIsolatedSlots(t *testing.T) {
	dir := NewDirectory(nil)
	reset := int64(9999999999999)
	slot := directoryTestSlot("tok-isolated", PoolBasic, 1, 30, reset, []string{"tenant-a"})
	slot.Quota.Set(3, QuotaWindow{
		Total:         10,
		Remaining:     10,
		WindowSeconds: 7200,
		ResetAt:       &reset,
	})
	dir.slots = map[string]*Slot{slot.Token: slot}

	snap := dir.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected one slot, got %d", len(snap))
	}
	snap[0].FailCount = 99
	snap[0].Health = 0.01
	snap[0].Tags[0] = "mutated"
	if w := snap[0].Quota.Get(3); w != nil {
		w.Remaining = 0
	}

	got := dir.Snapshot()[0]
	if got.FailCount != 0 {
		t.Fatalf("snapshot mutation leaked fail count into directory: %d", got.FailCount)
	}
	if got.Health != 1.0 {
		t.Fatalf("snapshot mutation leaked health into directory: %v", got.Health)
	}
	if got.Tags[0] != "tenant-a" {
		t.Fatalf("snapshot mutation leaked tags into directory: %v", got.Tags)
	}
	if w := got.Quota.Get(3); w == nil || w.Remaining != 10 {
		t.Fatalf("snapshot mutation leaked quota into directory: %#v", w)
	}
}

func directoryTestSlot(token string, pool PoolID, modeID int, remaining int, reset int64, tags []string) *Slot {
	quota := QuotaSet{}
	quota.Set(modeID, QuotaWindow{
		Total:         remaining,
		Remaining:     remaining,
		WindowSeconds: 86400,
		ResetAt:       &reset,
	})
	return &Slot{
		Token:    token,
		PoolID:   pool,
		StatusID: StatusIDActive,
		Quota:    quota,
		Health:   1.0,
		Tags:     tags,
	}
}
