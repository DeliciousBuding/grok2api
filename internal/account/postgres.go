package account

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/DeliciousBuding/grok2api/internal/platform"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresRepository stores account records in PostgreSQL. The canonical
// account payload remains the same JSON record used by the local backends.
type PostgresRepository struct {
	mu  sync.Mutex
	dsn string
	db  *sql.DB
}

func NewPostgresRepository(dsn string) *PostgresRepository {
	return &PostgresRepository{dsn: strings.TrimSpace(dsn)}
}

func (r *PostgresRepository) Initialize(ctx context.Context) error {
	if r.dsn == "" {
		return errors.New("postgres account repository requires a DSN")
	}
	db, err := sql.Open("pgx", r.dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if _, err := db.ExecContext(ctx, postgresAccountSchema); err != nil {
		_ = db.Close()
		return err
	}
	r.mu.Lock()
	r.db = db
	r.mu.Unlock()
	return nil
}

func (r *PostgresRepository) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

func (r *PostgresRepository) GetRevision(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentRevisionLocked(ctx)
}

func (r *PostgresRepository) RuntimeSnapshot(ctx context.Context) (*Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.queryRecordsLocked(ctx, "SELECT data_json::text, revision FROM accounts WHERE deleted_at IS NULL ORDER BY revision ASC")
	if err != nil {
		return nil, err
	}
	rev, err := r.currentRevisionLocked(ctx)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Revision: rev, Items: rows}, nil
}

func (r *PostgresRepository) ScanChanges(ctx context.Context, since int, limit int) (*ChangeSet, error) {
	if limit <= 0 {
		limit = 5000
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, hasMore, err := r.queryChangeWindowLocked(ctx, since, limit)
	if err != nil {
		return nil, err
	}
	current, err := r.currentRevisionLocked(ctx)
	if err != nil {
		return nil, err
	}
	return buildChangeSetFromWindow(rows, since, current, hasMore), nil
}

func (r *PostgresRepository) UpsertAccounts(ctx context.Context, items []Upsert) (*MutationResult, error) {
	normalized, err := normalizeUpserts(items, "")
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, rev, err := r.beginMutationLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := platform.NowMs()
	upserted := 0
	for _, it := range normalized {
		rec := &Record{
			Token:     it.Token,
			Pool:      it.Pool,
			Status:    StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
			Tags:      it.Tags,
			Quota:     DefaultQuotaSet(it.Pool).ToMap(),
			Ext:       it.Ext,
			Revision:  rev,
		}
		old, err := postgresRecordByToken(ctx, tx, it.Token)
		if err != nil {
			return nil, err
		}
		if old != nil {
			rec.CreatedAt = old.CreatedAt
		}
		if err := postgresWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
		upserted++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MutationResult{Upserted: upserted, Revision: rev}, nil
}

func (r *PostgresRepository) PatchAccounts(ctx context.Context, patches []Patch) (*MutationResult, error) {
	normalized, err := normalizePatches(patches)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, rev, err := r.beginMutationLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := platform.NowMs()
	patched := 0
	for _, p := range normalized {
		if p.Token == "" {
			continue
		}
		rec, err := postgresRecordByToken(ctx, tx, p.Token)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.IsDeleted() {
			continue
		}
		if err := applyPatchToRecord(rec, p, now, rev); err != nil {
			return nil, err
		}
		if err := postgresWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
		patched++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MutationResult{Patched: patched, Revision: rev}, nil
}

func (r *PostgresRepository) DeleteAccounts(ctx context.Context, tokens []string) (*MutationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, rev, err := r.beginMutationLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := platform.NowMs()
	deleted := 0
	for _, raw := range tokens {
		tok := platform.SanitizeToken(raw)
		if tok == "" {
			continue
		}
		rec, err := postgresRecordByToken(ctx, tx, tok)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.IsDeleted() {
			continue
		}
		rec.DeletedAt = &now
		rec.UpdatedAt = now
		rec.Revision = rev
		if err := postgresWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
		deleted++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MutationResult{Deleted: deleted, Revision: rev}, nil
}

func (r *PostgresRepository) GetAccounts(ctx context.Context, tokens []string) ([]*Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	db, err := r.dbLocked()
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(tokens))
	for _, raw := range tokens {
		tok := platform.SanitizeToken(raw)
		if tok == "" {
			continue
		}
		rec, err := postgresRecordByToken(ctx, db, tok)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (r *PostgresRepository) ListAccounts(ctx context.Context, q ListQuery) (*Page, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.queryRecordsLocked(ctx, "SELECT data_json::text, revision FROM accounts")
	if err != nil {
		return nil, err
	}
	rev, err := r.currentRevisionLocked(ctx)
	if err != nil {
		return nil, err
	}
	return pageRecords(records, q, rev), nil
}

func (r *PostgresRepository) ReplacePool(ctx context.Context, pool string, upserts []Upsert) (*MutationResult, error) {
	normalized, err := normalizeUpserts(upserts, pool)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, rev, err := r.beginMutationLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := platform.NowMs()
	rows, err := tx.QueryContext(ctx, "SELECT data_json::text, revision FROM accounts WHERE pool = $1 AND deleted_at IS NULL", pool)
	if err != nil {
		return nil, err
	}
	existing, err := scanPostgresRecords(rows)
	if err != nil {
		return nil, err
	}
	for _, rec := range existing {
		rec.DeletedAt = &now
		rec.UpdatedAt = now
		rec.Revision = rev
		if err := postgresWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
	}
	for _, it := range normalized {
		rec := &Record{
			Token:     it.Token,
			Pool:      pool,
			Status:    StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
			Tags:      it.Tags,
			Quota:     DefaultQuotaSet(pool).ToMap(),
			Ext:       it.Ext,
			Revision:  rev,
		}
		old, err := postgresRecordByToken(ctx, tx, it.Token)
		if err != nil {
			return nil, err
		}
		if old != nil {
			rec.CreatedAt = old.CreatedAt
		}
		if err := postgresWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MutationResult{Upserted: len(normalized), Revision: rev}, nil
}

func (r *PostgresRepository) ResetExpiredConsoleWindows(ctx context.Context) (int, error) {
	return r.mutateMatchingRecords(ctx, func(rec *Record, now int64) bool {
		if rec.IsDeleted() || rec.Status != StatusActive {
			return false
		}
		qs := rec.QuotaSet()
		w := qs.Console
		if w == nil {
			return false
		}
		return w.IsExhausted() || w.IsWindowExpired(now)
	}, func(rec *Record, now int64) {
		rec.Quota["console"] = DefaultQuotaWindow("basic", 5).ToMap()
		rec.LastSyncAt = &now
	})
}

func (r *PostgresRepository) RecoverConsoleExpiredAccounts(ctx context.Context) (int, error) {
	return r.mutateMatchingRecords(ctx, func(rec *Record, now int64) bool {
		if rec.IsDeleted() || rec.Status != StatusExpired {
			return false
		}
		if rec.StateReason == nil || *rec.StateReason != "console_429_threshold_exceeded" {
			return false
		}
		if rec.UsageUseCount <= 5 {
			return false
		}
		expiredAt := int64(0)
		if v, ok := rec.Ext["expired_at"].(float64); ok {
			expiredAt = int64(v)
		}
		return expiredAt <= now-3600000
	}, func(rec *Record, now int64) {
		rec.Status = StatusActive
		rec.StateReason = nil
		deleteExtKeys(rec.Ext, "expired_at", "expired_reason", "console_429_count", "console_429_last_at")
	})
}

func (r *PostgresRepository) mutateMatchingRecords(ctx context.Context, match func(*Record, int64) bool, mutate func(*Record, int64)) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.queryRecordsLocked(ctx, "SELECT data_json::text, revision FROM accounts")
	if err != nil {
		return 0, err
	}
	now := platform.NowMs()
	var selected []*Record
	for _, rec := range records {
		if match(rec, now) {
			selected = append(selected, rec)
		}
	}
	if len(selected) == 0 {
		return 0, nil
	}
	tx, rev, err := r.beginMutationLocked(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, rec := range selected {
		mutate(rec, now)
		rec.UpdatedAt = now
		rec.Revision = rev
		if err := postgresWriteRecord(ctx, tx, rec); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(selected), nil
}

func (r *PostgresRepository) beginMutationLocked(ctx context.Context) (*sql.Tx, int, error) {
	db, err := r.dbLocked()
	if err != nil {
		return nil, 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	var rev int
	if err := tx.QueryRowContext(ctx, "SELECT value FROM account_meta WHERE key = 'revision' FOR UPDATE").Scan(&rev); err != nil {
		_ = tx.Rollback()
		return nil, 0, err
	}
	rev++
	if _, err := tx.ExecContext(ctx, "UPDATE account_meta SET value = $1 WHERE key = 'revision'", rev); err != nil {
		_ = tx.Rollback()
		return nil, 0, err
	}
	return tx, rev, nil
}

func (r *PostgresRepository) currentRevisionLocked(ctx context.Context) (int, error) {
	db, err := r.dbLocked()
	if err != nil {
		return 0, err
	}
	var rev int
	if err := db.QueryRowContext(ctx, "SELECT value FROM account_meta WHERE key = 'revision'").Scan(&rev); err != nil {
		return 0, err
	}
	return rev, nil
}

func (r *PostgresRepository) queryRecordsLocked(ctx context.Context, query string, args ...any) ([]*Record, error) {
	db, err := r.dbLocked()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanPostgresRecords(rows)
}

func (r *PostgresRepository) queryChangeWindowLocked(ctx context.Context, since int, limit int) ([]*Record, bool, error) {
	db, err := r.dbLocked()
	if err != nil {
		return nil, false, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT data_json::text, revision
FROM accounts
WHERE revision > $1
ORDER BY revision ASC, token ASC
LIMIT $2`, since, limit)
	if err != nil {
		return nil, false, err
	}
	records, err := scanPostgresRecords(rows)
	if err != nil {
		return nil, false, err
	}
	if len(records) == 0 {
		return records, false, nil
	}
	boundary := records[len(records)-1]
	rows, err = db.QueryContext(ctx, `
SELECT data_json::text, revision
FROM accounts
WHERE revision = $1 AND token > $2
ORDER BY token ASC`, boundary.Revision, boundary.Token)
	if err != nil {
		return nil, false, err
	}
	sameRevisionTail, err := scanPostgresRecords(rows)
	if err != nil {
		return nil, false, err
	}
	records = append(records, sameRevisionTail...)
	hasMore, err := postgresHasRevisionAfter(ctx, db, boundary.Revision)
	if err != nil {
		return nil, false, err
	}
	return records, hasMore, nil
}

func postgresHasRevisionAfter(ctx context.Context, db *sql.DB, revision int) (bool, error) {
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT 1 FROM accounts WHERE revision > $1 LIMIT 1", revision).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *PostgresRepository) dbLocked() (*sql.DB, error) {
	if r.db == nil {
		return nil, errors.New("postgres account repository is not initialized")
	}
	return r.db, nil
}

type postgresRecordReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type postgresRecordWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func postgresRecordByToken(ctx context.Context, q postgresRecordReader, token string) (*Record, error) {
	var payload string
	var rev int
	err := q.QueryRowContext(ctx, "SELECT data_json::text, revision FROM accounts WHERE token = $1", token).Scan(&payload, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodePostgresRecord(payload, rev)
}

func postgresWriteRecord(ctx context.Context, w postgresRecordWriter, rec *Record) error {
	normalizeRecord(rec)
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	var deleted any
	if rec.DeletedAt != nil {
		deleted = *rec.DeletedAt
	}
	_, err = w.ExecContext(ctx, `
INSERT INTO accounts (token, pool, status, updated_at, deleted_at, revision, data_json)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
ON CONFLICT(token) DO UPDATE SET
  pool=excluded.pool,
  status=excluded.status,
  updated_at=excluded.updated_at,
  deleted_at=excluded.deleted_at,
  revision=excluded.revision,
  data_json=excluded.data_json
`, rec.Token, rec.Pool, string(rec.Status), rec.UpdatedAt, deleted, rec.Revision, string(payload))
	return err
}

func scanPostgresRecords(rows *sql.Rows) ([]*Record, error) {
	defer rows.Close()
	var records []*Record
	for rows.Next() {
		var payload string
		var rev int
		if err := rows.Scan(&payload, &rev); err != nil {
			return nil, err
		}
		rec, err := decodePostgresRecord(payload, rev)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func decodePostgresRecord(payload string, rev int) (*Record, error) {
	var rec Record
	if err := json.Unmarshal([]byte(payload), &rec); err != nil {
		return nil, err
	}
	if rec.Revision == 0 {
		rec.Revision = rev
	}
	normalizeRecord(&rec)
	return &rec, nil
}

const postgresAccountSchema = `
CREATE TABLE IF NOT EXISTS accounts (
  token TEXT PRIMARY KEY,
  pool TEXT NOT NULL,
  status TEXT NOT NULL,
  updated_at BIGINT NOT NULL,
  deleted_at BIGINT,
  revision BIGINT NOT NULL,
  data_json JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_revision ON accounts(revision, token);
CREATE INDEX IF NOT EXISTS idx_accounts_pool_status ON accounts(pool, status);
CREATE INDEX IF NOT EXISTS idx_accounts_deleted_at ON accounts(deleted_at);
CREATE TABLE IF NOT EXISTS account_meta (
  key TEXT PRIMARY KEY,
  value BIGINT NOT NULL
);
INSERT INTO account_meta (key, value)
VALUES ('revision', (SELECT COALESCE(MAX(revision), 0) FROM accounts))
ON CONFLICT(key) DO NOTHING;
UPDATE account_meta
SET value = (SELECT COALESCE(MAX(revision), 0) FROM accounts)
WHERE key = 'revision'
  AND value < (SELECT COALESCE(MAX(revision), 0) FROM accounts);
`

var _ Repository = (*PostgresRepository)(nil)

func (r *PostgresRepository) String() string {
	return fmt.Sprintf("postgres:%s", redactPostgresDSN(r.dsn))
}

func redactPostgresDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	if i := strings.Index(dsn, "@"); i >= 0 {
		prefix := dsn[:i]
		if j := strings.LastIndex(prefix, "://"); j >= 0 {
			return dsn[:j+3] + "redacted@" + dsn[i+1:]
		}
	}
	return "redacted"
}
