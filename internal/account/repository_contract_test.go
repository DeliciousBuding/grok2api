package account

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryScanChangesDoesNotSplitSingleRevisionBatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo func(*testing.T) Repository
	}{
		{
			name: "txt",
			repo: func(t *testing.T) Repository {
				return NewTxtRepository(filepath.Join(t.TempDir(), "accounts.jsonl"))
			},
		},
		{
			name: "sqlite",
			repo: func(t *testing.T) Repository {
				return NewSQLiteRepository(filepath.Join(t.TempDir(), "accounts.sqlite3"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := tc.repo(t)
			if err := repo.Initialize(ctx); err != nil {
				t.Fatalf("initialize repo: %v", err)
			}
			t.Cleanup(func() { _ = repo.Close(ctx) })

			if _, err := repo.UpsertAccounts(ctx, []Upsert{
				{Token: "tok-a"},
				{Token: "tok-b"},
				{Token: "tok-c"},
			}); err != nil {
				t.Fatalf("upsert accounts: %v", err)
			}

			changes, err := repo.ScanChanges(ctx, 0, 2)
			if err != nil {
				t.Fatalf("scan changes: %v", err)
			}
			if changes.HasMore {
				t.Fatalf("single revision batch should not be split: %#v", changes)
			}
			if len(changes.Items) != 3 {
				t.Fatalf("expected all 3 records from the revision batch, got %d", len(changes.Items))
			}
		})
	}
}

func TestRepositoryRejectsOversizedTokensBeforePersisting(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo func(*testing.T) Repository
	}{
		{
			name: "txt",
			repo: func(t *testing.T) Repository {
				return NewTxtRepository(filepath.Join(t.TempDir(), "accounts.jsonl"))
			},
		},
		{
			name: "sqlite",
			repo: func(t *testing.T) Repository {
				return NewSQLiteRepository(filepath.Join(t.TempDir(), "accounts.sqlite3"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := tc.repo(t)
			if err := repo.Initialize(ctx); err != nil {
				t.Fatalf("initialize repo: %v", err)
			}
			t.Cleanup(func() { _ = repo.Close(ctx) })

			oversized := "tok_" + strings.Repeat("x", 4097)

			if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-ok"}, {Token: oversized}}); err == nil || !strings.Contains(err.Error(), "token_too_long") {
				t.Fatalf("expected token_too_long from upsert, got %v", err)
			} else if strings.Contains(err.Error(), strings.Repeat("x", 32)) {
				t.Fatalf("token length error should not echo raw token material: %v", err)
			}
			snapshot, err := repo.RuntimeSnapshot(ctx)
			if err != nil {
				t.Fatalf("snapshot after failed upsert: %v", err)
			}
			if len(snapshot.Items) != 0 {
				t.Fatalf("failed upsert should not persist partial records, got %d", len(snapshot.Items))
			}

			if _, err := repo.ReplacePool(ctx, "basic", []Upsert{{Token: oversized}}); err == nil || !strings.Contains(err.Error(), "token_too_long") {
				t.Fatalf("expected token_too_long from replace pool, got %v", err)
			}
			snapshot, err = repo.RuntimeSnapshot(ctx)
			if err != nil {
				t.Fatalf("snapshot after failed replace: %v", err)
			}
			if len(snapshot.Items) != 0 {
				t.Fatalf("failed replace should not persist records, got %d", len(snapshot.Items))
			}
		})
	}
}
