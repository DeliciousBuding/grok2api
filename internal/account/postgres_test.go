package account

import (
	"context"
	"os"
	"testing"
)

func TestPostgresRepositoryRoundTrip(t *testing.T) {
	dsn := os.Getenv("GROK2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GROK2API_TEST_POSTGRES_DSN to run PostgreSQL repository integration tests")
	}
	ctx := context.Background()
	repo := NewPostgresRepository(dsn)
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize postgres repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close(ctx) })

	if _, err := repo.UpsertAccounts(ctx, []Upsert{
		{Token: "pg-tok-a", Pool: "basic", Tags: []string{"blue"}},
		{Token: "pg-tok-b", Pool: "super"},
		{Token: "pg-tok-c", Pool: "basic"},
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

	status := StatusDisabled
	if _, err := repo.PatchAccounts(ctx, []Patch{{Token: "pg-tok-a", Status: &status}}); err != nil {
		t.Fatalf("patch account: %v", err)
	}
	recs, err := repo.GetAccounts(ctx, []string{"pg-tok-a"})
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if len(recs) != 1 || recs[0].Status != StatusDisabled {
		t.Fatalf("expected disabled pg-tok-a, got %#v", recs)
	}

	if _, err := repo.ReplacePool(ctx, "basic", []Upsert{{Token: "pg-tok-new", Pool: "basic"}}); err != nil {
		t.Fatalf("replace pool: %v", err)
	}
	page, err := repo.ListAccounts(ctx, ListQuery{Pool: "basic", PageSize: 100})
	if err != nil {
		t.Fatalf("list basic accounts: %v", err)
	}
	if page.Total != 1 || page.Items[0].Token != "pg-tok-new" {
		t.Fatalf("replace pool should leave one basic account, got total=%d items=%#v", page.Total, page.Items)
	}
}
