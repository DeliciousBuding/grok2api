package config

import "testing"

func TestAccountStorageKeysAreStartupOnly(t *testing.T) {
	for _, key := range []string{
		"account.storage.backend",
		"account.local.path",
		"account.sqlite.path",
		"account.postgresql.dsn",
		"account.redis.addr",
	} {
		if !IsStartupOnlyConfigKey(key) {
			t.Fatalf("expected %s to be startup-only", key)
		}
	}
}
