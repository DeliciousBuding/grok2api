package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteMediaCacheDSNEscapesURIPathCharacters(t *testing.T) {
	dsn := sqliteMediaCacheDSN(filepath.Join("data", "media?cache#1.db"))

	if strings.Contains(dsn, "media?cache") || strings.Contains(dsn, "#1.db") {
		t.Fatalf("sqlite media cache DSN should escape URI path metacharacters, got %q", dsn)
	}
	if !strings.Contains(dsn, "media%3Fcache%231.db") {
		t.Fatalf("sqlite media cache DSN should preserve the literal filename through escaping, got %q", dsn)
	}
	if !strings.Contains(dsn, "?_pragma=journal_mode(WAL)") {
		t.Fatalf("sqlite media cache DSN should keep pragmas in the query string, got %q", dsn)
	}
}
