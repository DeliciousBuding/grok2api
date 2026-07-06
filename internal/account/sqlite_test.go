package account

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteRepositoryPersistsAccountMutations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "accounts.db")

	repo := NewSQLiteRepository(path)
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize sqlite repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-a", Pool: "super", Tags: []string{"vip"}}}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	status := StatusDisabled
	reason := "manual"
	if _, err := repo.PatchAccounts(ctx, []Patch{{Token: "tok-a", Status: &status, StateReason: &reason}}); err != nil {
		t.Fatalf("patch account: %v", err)
	}
	if err := repo.Close(ctx); err != nil {
		t.Fatalf("close sqlite repo: %v", err)
	}

	reopened := NewSQLiteRepository(path)
	if err := reopened.Initialize(ctx); err != nil {
		t.Fatalf("reopen sqlite repo: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close(ctx) })

	page, err := reopened.ListAccounts(ctx, ListQuery{Page: 1, PageSize: 10, IncludeDeleted: true})
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if page.Total != 1 || page.Items[0].Token != "tok-a" || page.Items[0].Pool != "super" {
		t.Fatalf("unexpected persisted page: %#v", page)
	}
	if page.Items[0].Status != StatusDisabled || page.Items[0].StateReason == nil || *page.Items[0].StateReason != reason {
		t.Fatalf("patch did not persist: %#v", page.Items[0])
	}
	if page.Revision < 2 {
		t.Fatalf("expected persisted revision >= 2, got %d", page.Revision)
	}
}

func TestSQLiteRepositoryTracksDeletedChanges(t *testing.T) {
	ctx := context.Background()
	repo := NewSQLiteRepository(filepath.Join(t.TempDir(), "accounts.db"))
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize sqlite repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close(ctx) })

	upsert, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-a"}, {Token: "tok-b"}})
	if err != nil {
		t.Fatalf("upsert accounts: %v", err)
	}
	if _, err := repo.DeleteAccounts(ctx, []string{"tok-a"}); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	changes, err := repo.ScanChanges(ctx, upsert.Revision, 10)
	if err != nil {
		t.Fatalf("scan changes: %v", err)
	}
	if changes.HasMore {
		t.Fatalf("unexpected has_more: %#v", changes)
	}
	if len(changes.DeletedTokens) != 1 || changes.DeletedTokens[0] != "tok-a" {
		t.Fatalf("expected deleted tok-a change, got %#v", changes.DeletedTokens)
	}
}

func TestSQLiteRepositoryAdvancesRevisionOnNoopMutation(t *testing.T) {
	ctx := context.Background()
	repo := NewSQLiteRepository(filepath.Join(t.TempDir(), "accounts.db"))
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize sqlite repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close(ctx) })

	status := StatusDisabled
	result, err := repo.PatchAccounts(ctx, []Patch{{Token: "missing-token", Status: &status}})
	if err != nil {
		t.Fatalf("patch missing account: %v", err)
	}
	revision, err := repo.GetRevision(ctx)
	if err != nil {
		t.Fatalf("get revision: %v", err)
	}
	if revision != result.Revision {
		t.Fatalf("expected repository revision %d to match mutation revision %d", revision, result.Revision)
	}

	changes, err := repo.ScanChanges(ctx, 0, 10)
	if err != nil {
		t.Fatalf("scan changes: %v", err)
	}
	if changes.Revision != revision || len(changes.Items) != 0 {
		t.Fatalf("unexpected no-op changeset: %#v", changes)
	}
}

func TestSQLiteAccountDSNEscapesURIPathCharacters(t *testing.T) {
	dsn := sqliteAccountDSN(filepath.Join("data", "accounts?blue#1.db"))

	if strings.Contains(dsn, "accounts?blue") || strings.Contains(dsn, "#1.db") {
		t.Fatalf("sqlite DSN should escape URI path metacharacters, got %q", dsn)
	}
	if !strings.Contains(dsn, "accounts%3Fblue%231.db") {
		t.Fatalf("sqlite DSN should preserve the literal filename through escaping, got %q", dsn)
	}
	if !strings.Contains(dsn, "?_pragma=journal_mode(WAL)") {
		t.Fatalf("sqlite DSN should keep pragmas in the query string, got %q", dsn)
	}
}

func TestSQLiteAccountDSNPreservesWindowsDriveLetter(t *testing.T) {
	path := "D:" + "\\" + "data" + "\\" + "accounts.db"
	dsn := sqliteAccountDSN(path)

	if !strings.HasPrefix(dsn, "file:D:/data/accounts.db?") {
		t.Fatalf("sqlite DSN should preserve Windows drive-letter paths, got %q", dsn)
	}
}
