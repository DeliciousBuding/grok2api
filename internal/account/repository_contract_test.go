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

func TestRepositoryRejectsInvalidTagsBeforePersisting(t *testing.T) {
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

			longTag := "tag-" + strings.Repeat("x", 65)
			if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-long-tag", Tags: []string{longTag}}}); err == nil || !strings.Contains(err.Error(), "tag_too_long") {
				t.Fatalf("expected tag_too_long from upsert, got %v", err)
			} else if strings.Contains(err.Error(), strings.Repeat("x", 32)) {
				t.Fatalf("tag length error should not echo raw tag material: %v", err)
			}

			tooManyTags := make([]string, 11)
			for i := range tooManyTags {
				tooManyTags[i] = "tag-" + string(rune('a'+i))
			}
			if _, err := repo.ReplacePool(ctx, "basic", []Upsert{{Token: "tok-many-tags", Tags: tooManyTags}}); err == nil || !strings.Contains(err.Error(), "too_many_tags") {
				t.Fatalf("expected too_many_tags from replace pool, got %v", err)
			}

			if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-ok", Tags: []string{"stable"}}}); err != nil {
				t.Fatalf("seed valid account: %v", err)
			}
			if _, err := repo.PatchAccounts(ctx, []Patch{{Token: "tok-ok", Tags: []string{longTag}}}); err == nil || !strings.Contains(err.Error(), "tag_too_long") {
				t.Fatalf("expected tag_too_long from patch, got %v", err)
			}
			recs, err := repo.GetAccounts(ctx, []string{"tok-ok"})
			if err != nil {
				t.Fatalf("get patched account: %v", err)
			}
			if len(recs) != 1 || len(recs[0].Tags) != 1 || recs[0].Tags[0] != "stable" {
				t.Fatalf("failed patch should preserve existing tags, got %#v", recs)
			}

			baseTags := make([]string, 10)
			for i := range baseTags {
				baseTags[i] = "tag-" + string(rune('a'+i))
			}
			if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-many-patch-tags", Tags: baseTags}}); err != nil {
				t.Fatalf("seed max tag account: %v", err)
			}
			if _, err := repo.PatchAccounts(ctx, []Patch{{Token: "tok-many-patch-tags", AddTags: []string{"tag-extra"}}}); err == nil || !strings.Contains(err.Error(), "too_many_tags") {
				t.Fatalf("expected too_many_tags from patch add_tags, got %v", err)
			}
			recs, err = repo.GetAccounts(ctx, []string{"tok-many-patch-tags"})
			if err != nil {
				t.Fatalf("get account after failed add_tags: %v", err)
			}
			if len(recs) != 1 || len(recs[0].Tags) != len(baseTags) {
				t.Fatalf("failed add_tags should preserve existing tags, got %#v", recs)
			}
			for i, tag := range recs[0].Tags {
				if tag != baseTags[i] {
					t.Fatalf("failed add_tags should preserve existing tags, got %#v", recs[0].Tags)
				}
			}

			if _, err := repo.PatchAccounts(ctx, []Patch{{Token: "tok-many-patch-tags", RemoveTags: []string{longTag}}}); err == nil || !strings.Contains(err.Error(), "tag_too_long") {
				t.Fatalf("expected tag_too_long from patch remove_tags, got %v", err)
			}
			recs, err = repo.GetAccounts(ctx, []string{"tok-many-patch-tags"})
			if err != nil {
				t.Fatalf("get account after failed remove_tags: %v", err)
			}
			if len(recs) != 1 || len(recs[0].Tags) != len(baseTags) {
				t.Fatalf("failed remove_tags should preserve existing tags, got %#v", recs)
			}
		})
	}
}

func TestRepositoryBoundsPersistentReasonFields(t *testing.T) {
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

			if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-reason"}}); err != nil {
				t.Fatalf("seed account: %v", err)
			}

			longReason := "failure:" + strings.Repeat("x", MaxReasonLength+128) + ":tail"
			statusReason := "state:" + strings.Repeat("y", MaxReasonLength+128) + ":tail"
			status := StatusCooling
			if _, err := repo.PatchAccounts(ctx, []Patch{
				{
					Token:          "tok-reason",
					Status:         &status,
					LastFailReason: &longReason,
					StateReason:    &statusReason,
				},
			}); err != nil {
				t.Fatalf("patch oversized reasons: %v", err)
			}

			recs, err := repo.GetAccounts(ctx, []string{"tok-reason"})
			if err != nil {
				t.Fatalf("get account: %v", err)
			}
			if len(recs) != 1 {
				t.Fatalf("expected one account, got %d", len(recs))
			}
			if recs[0].LastFailReason == nil || len(*recs[0].LastFailReason) != MaxReasonLength {
				t.Fatalf("last_fail_reason should be capped to %d chars, got %#v", MaxReasonLength, recs[0].LastFailReason)
			}
			if recs[0].StateReason == nil || len(*recs[0].StateReason) != MaxReasonLength {
				t.Fatalf("state_reason should be capped to %d chars, got %#v", MaxReasonLength, recs[0].StateReason)
			}
			if strings.Contains(*recs[0].LastFailReason, ":tail") || strings.Contains(*recs[0].StateReason, ":tail") {
				t.Fatalf("bounded reasons should not persist oversized tail data: %#v %#v", recs[0].LastFailReason, recs[0].StateReason)
			}
		})
	}
}
