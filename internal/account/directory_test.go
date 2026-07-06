package account

import (
	"context"
	"path/filepath"
	"testing"
)

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

func TestSetMaxInflightIgnoresInvalidValues(t *testing.T) {
	dir := NewDirectory(nil)
	dir.SetMaxInflight(0)

	if dir.maxInflight != quotaMaxInflight {
		t.Fatalf("invalid max inflight should reset to default %d, got %d", quotaMaxInflight, dir.maxInflight)
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
