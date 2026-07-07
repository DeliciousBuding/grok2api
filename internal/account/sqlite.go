package account

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/DeliciousBuding/grok2api/internal/platform"
	_ "modernc.org/sqlite"
)

// SQLiteRepository stores account records in a local SQLite database.
type SQLiteRepository struct {
	mu   sync.Mutex
	path string
	db   *sql.DB
}

// NewSQLiteRepository opens (or creates) a SQLite account database at path.
func NewSQLiteRepository(path string) *SQLiteRepository {
	return &SQLiteRepository{path: path}
}

func (r *SQLiteRepository) Initialize(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", sqliteAccountDSN(r.path))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, sqliteAccountSchema); err != nil {
		_ = db.Close()
		return err
	}
	r.mu.Lock()
	r.db = db
	r.mu.Unlock()
	return nil
}

func (r *SQLiteRepository) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

func (r *SQLiteRepository) GetRevision(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentRevisionLocked(ctx)
}

func (r *SQLiteRepository) RuntimeSnapshot(ctx context.Context) (*Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.queryRecordsLocked(ctx, "SELECT data_json, revision FROM accounts WHERE deleted_at IS NULL ORDER BY revision ASC")
	if err != nil {
		return nil, err
	}
	rev, err := r.currentRevisionLocked(ctx)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Revision: rev, Items: rows}, nil
}

func (r *SQLiteRepository) ScanChanges(ctx context.Context, since int, limit int) (*ChangeSet, error) {
	if limit <= 0 {
		limit = 5000
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.queryRecordsLocked(ctx, "SELECT data_json, revision FROM accounts WHERE revision > ? ORDER BY revision ASC", since)
	if err != nil {
		return nil, err
	}
	current, err := r.currentRevisionLocked(ctx)
	if err != nil {
		return nil, err
	}
	return buildChangeSet(rows, since, current, limit), nil
}

func (r *SQLiteRepository) UpsertAccounts(ctx context.Context, items []Upsert) (*MutationResult, error) {
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
		old, err := sqliteRecordByToken(ctx, tx, it.Token)
		if err != nil {
			return nil, err
		}
		if old != nil {
			rec.CreatedAt = old.CreatedAt
		}
		if err := sqliteWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
		upserted++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MutationResult{Upserted: upserted, Revision: rev}, nil
}

func (r *SQLiteRepository) PatchAccounts(ctx context.Context, patches []Patch) (*MutationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, rev, err := r.beginMutationLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := platform.NowMs()
	patched := 0
	for _, p := range patches {
		tok := platform.SanitizeToken(p.Token)
		if tok == "" {
			continue
		}
		rec, err := sqliteRecordByToken(ctx, tx, tok)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.IsDeleted() {
			continue
		}
		applyPatchToRecord(rec, p, now, rev)
		if err := sqliteWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
		patched++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MutationResult{Patched: patched, Revision: rev}, nil
}

func (r *SQLiteRepository) DeleteAccounts(ctx context.Context, tokens []string) (*MutationResult, error) {
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
		rec, err := sqliteRecordByToken(ctx, tx, tok)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.IsDeleted() {
			continue
		}
		rec.DeletedAt = &now
		rec.UpdatedAt = now
		rec.Revision = rev
		if err := sqliteWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
		deleted++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MutationResult{Deleted: deleted, Revision: rev}, nil
}

func (r *SQLiteRepository) GetAccounts(ctx context.Context, tokens []string) ([]*Record, error) {
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
		rec, err := sqliteRecordByToken(ctx, db, tok)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (r *SQLiteRepository) ListAccounts(ctx context.Context, q ListQuery) (*Page, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.queryRecordsLocked(ctx, "SELECT data_json, revision FROM accounts")
	if err != nil {
		return nil, err
	}
	rev, err := r.currentRevisionLocked(ctx)
	if err != nil {
		return nil, err
	}
	return pageRecords(records, q, rev), nil
}

func (r *SQLiteRepository) ReplacePool(ctx context.Context, pool string, upserts []Upsert) (*MutationResult, error) {
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
	rows, err := tx.QueryContext(ctx, "SELECT data_json, revision FROM accounts WHERE pool = ? AND deleted_at IS NULL", pool)
	if err != nil {
		return nil, err
	}
	existing, err := scanSQLiteRecords(rows)
	if err != nil {
		return nil, err
	}
	for _, rec := range existing {
		rec.DeletedAt = &now
		rec.UpdatedAt = now
		rec.Revision = rev
		if err := sqliteWriteRecord(ctx, tx, rec); err != nil {
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
		old, err := sqliteRecordByToken(ctx, tx, it.Token)
		if err != nil {
			return nil, err
		}
		if old != nil {
			rec.CreatedAt = old.CreatedAt
		}
		if err := sqliteWriteRecord(ctx, tx, rec); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MutationResult{Upserted: len(normalized), Revision: rev}, nil
}

func (r *SQLiteRepository) ResetExpiredConsoleWindows(ctx context.Context) (int, error) {
	return r.mutateMatchingRecords(ctx, func(rec *Record, now int64) bool {
		if rec.IsDeleted() || rec.Status != StatusActive {
			return false
		}
		qs := rec.QuotaSet()
		w := qs.Console
		if w == nil {
			return false
		}
		return w.Remaining <= 0 || w.ResetAt == nil || *w.ResetAt <= now
	}, func(rec *Record, now int64) {
		rec.Quota["console"] = DefaultQuotaWindow("basic", 5).ToMap()
		rec.LastSyncAt = &now
	})
}

func (r *SQLiteRepository) RecoverConsoleExpiredAccounts(ctx context.Context) (int, error) {
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

func (r *SQLiteRepository) mutateMatchingRecords(ctx context.Context, match func(*Record, int64) bool, mutate func(*Record, int64)) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.queryRecordsLocked(ctx, "SELECT data_json, revision FROM accounts")
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
		if err := sqliteWriteRecord(ctx, tx, rec); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(selected), nil
}

func (r *SQLiteRepository) beginMutationLocked(ctx context.Context) (*sql.Tx, int, error) {
	db, err := r.dbLocked()
	if err != nil {
		return nil, 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	var rev int
	res, err := tx.ExecContext(ctx, "UPDATE account_meta SET value = value + 1 WHERE key = 'revision'")
	if err != nil {
		_ = tx.Rollback()
		return nil, 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return nil, 0, err
	}
	if affected != 1 {
		_ = tx.Rollback()
		return nil, 0, errors.New("sqlite account revision metadata is missing")
	}
	if err := tx.QueryRowContext(ctx, "SELECT value FROM account_meta WHERE key = 'revision'").Scan(&rev); err != nil {
		_ = tx.Rollback()
		return nil, 0, err
	}
	return tx, rev, nil
}

func (r *SQLiteRepository) currentRevisionLocked(ctx context.Context) (int, error) {
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

func (r *SQLiteRepository) queryRecordsLocked(ctx context.Context, query string, args ...any) ([]*Record, error) {
	db, err := r.dbLocked()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanSQLiteRecords(rows)
}

func (r *SQLiteRepository) dbLocked() (*sql.DB, error) {
	if r.db == nil {
		return nil, errors.New("sqlite account repository is not initialized")
	}
	return r.db, nil
}

type sqliteRecordReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type sqliteRecordWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func sqliteRecordByToken(ctx context.Context, q sqliteRecordReader, token string) (*Record, error) {
	var payload string
	var rev int
	err := q.QueryRowContext(ctx, "SELECT data_json, revision FROM accounts WHERE token = ?", token).Scan(&payload, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeSQLiteRecord(payload, rev)
}

func sqliteWriteRecord(ctx context.Context, w sqliteRecordWriter, rec *Record) error {
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
VALUES (?, ?, ?, ?, ?, ?, ?)
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

func scanSQLiteRecords(rows *sql.Rows) ([]*Record, error) {
	defer rows.Close()
	var records []*Record
	for rows.Next() {
		var payload string
		var rev int
		if err := rows.Scan(&payload, &rev); err != nil {
			return nil, err
		}
		rec, err := decodeSQLiteRecord(payload, rev)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func decodeSQLiteRecord(payload string, rev int) (*Record, error) {
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

func normalizeRecord(rec *Record) {
	if rec.Tags == nil {
		rec.Tags = []string{}
	}
	if rec.Ext == nil {
		rec.Ext = map[string]any{}
	}
	if rec.Quota == nil {
		rec.Quota = DefaultQuotaSet(rec.Pool).ToMap()
	}
}

func applyPatchToRecord(rec *Record, p Patch, now int64, rev int) {
	normalizeRecord(rec)
	if p.Pool != nil {
		rec.Pool = *p.Pool
	}
	if p.Status != nil {
		rec.Status = *p.Status
	}
	if p.Tags != nil {
		rec.Tags = SortTags(p.Tags)
	}
	if len(p.AddTags) > 0 || len(p.RemoveTags) > 0 {
		rec.Tags = MergeTags(rec.Tags, p.AddTags, p.RemoveTags)
	}
	if p.QuotaAuto != nil {
		rec.Quota["auto"] = *p.QuotaAuto
	}
	if p.QuotaFast != nil {
		rec.Quota["fast"] = *p.QuotaFast
	}
	if p.QuotaExpert != nil {
		rec.Quota["expert"] = *p.QuotaExpert
	}
	if p.QuotaHeavy != nil {
		rec.Quota["heavy"] = *p.QuotaHeavy
	}
	if p.QuotaGrok43 != nil {
		rec.Quota["grok_4_3"] = *p.QuotaGrok43
	}
	if p.QuotaConsole != nil {
		rec.Quota["console"] = *p.QuotaConsole
	}
	if p.UsageUseDelta != nil {
		rec.UsageUseCount += *p.UsageUseDelta
		if rec.UsageUseCount < 0 {
			rec.UsageUseCount = 0
		}
	}
	if p.UsageFailDelta != nil {
		rec.UsageFailCount += *p.UsageFailDelta
		if rec.UsageFailCount < 0 {
			rec.UsageFailCount = 0
		}
	}
	if p.UsageSyncDelta != nil {
		rec.UsageSyncCount += *p.UsageSyncDelta
		if rec.UsageSyncCount < 0 {
			rec.UsageSyncCount = 0
		}
	}
	if p.LastUseAt != nil {
		rec.LastUseAt = p.LastUseAt
	}
	if p.LastFailAt != nil {
		rec.LastFailAt = p.LastFailAt
	}
	if p.LastFailReason != nil {
		rec.LastFailReason = p.LastFailReason
	}
	if p.LastSyncAt != nil {
		rec.LastSyncAt = p.LastSyncAt
	}
	if p.LastClearAt != nil {
		rec.LastClearAt = p.LastClearAt
	}
	if p.StateReason != nil {
		rec.StateReason = p.StateReason
	}
	if p.ClearFailures {
		rec.UsageFailCount = 0
		rec.LastFailAt = nil
		rec.LastFailReason = nil
		rec.StateReason = nil
		rec.Status = StatusActive
		deleteExtKeys(rec.Ext, "cooldown_until", "cooldown_reason",
			"disabled_at", "disabled_reason", "expired_at",
			"expired_reason", "forbidden_strikes")
	} else if len(p.ExtMerge) > 0 {
		for k, v := range p.ExtMerge {
			rec.Ext[k] = v
		}
	}
	rec.UpdatedAt = now
	rec.Revision = rev
}

func pageRecords(records []*Record, q ListQuery, revision int) *Page {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = 50
	}
	if q.PageSize > 2000 {
		q.PageSize = 2000
	}
	items := make([]*Record, 0, len(records))
	for _, rec := range records {
		if !q.IncludeDeleted && rec.IsDeleted() {
			continue
		}
		if q.Pool != "" && rec.Pool != q.Pool {
			continue
		}
		if q.Status != nil && rec.Status != *q.Status {
			continue
		}
		if len(q.Tags) > 0 && !recordHasAllTags(rec, q.Tags) {
			continue
		}
		items = append(items, rec)
	}
	sort.Slice(items, func(i, j int) bool {
		less := recordLess(items[i], items[j], q.SortBy)
		if q.SortDesc {
			return !less
		}
		return less
	})
	total := len(items)
	totalPages := 1
	if total > 0 {
		totalPages = (total + q.PageSize - 1) / q.PageSize
	}
	offset := (q.Page - 1) * q.PageSize
	if offset > total {
		offset = total
	}
	end := offset + q.PageSize
	if end > total {
		end = total
	}
	pageItems := items[offset:end]
	out := make([]*Record, len(pageItems))
	for i, rec := range pageItems {
		cp := *rec
		out[i] = &cp
	}
	return &Page{Items: out, Total: total, Page: q.Page, PageSize: q.PageSize, TotalPages: totalPages, Revision: revision}
}

func recordHasAllTags(rec *Record, want []string) bool {
	for _, tag := range want {
		found := false
		for _, have := range rec.Tags {
			if have == tag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func recordLess(a, b *Record, sortBy string) bool {
	switch sortBy {
	case "created_at":
		return a.CreatedAt < b.CreatedAt
	case "last_use_at":
		ai, bi := int64(0), int64(0)
		if a.LastUseAt != nil {
			ai = *a.LastUseAt
		}
		if b.LastUseAt != nil {
			bi = *b.LastUseAt
		}
		return ai < bi
	case "token":
		return a.Token < b.Token
	case "usage_use_count":
		return a.UsageUseCount < b.UsageUseCount
	case "usage_fail_count":
		return a.UsageFailCount < b.UsageFailCount
	default:
		return a.UpdatedAt < b.UpdatedAt
	}
}

func sqliteAccountDSN(path string) string {
	return platform.SQLiteFileDSN(path)
}

const sqliteAccountSchema = `
CREATE TABLE IF NOT EXISTS accounts (
  token TEXT PRIMARY KEY,
  pool TEXT NOT NULL,
  status TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  revision INTEGER NOT NULL,
  data_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_revision ON accounts(revision);
CREATE INDEX IF NOT EXISTS idx_accounts_pool_status ON accounts(pool, status);
CREATE INDEX IF NOT EXISTS idx_accounts_deleted_at ON accounts(deleted_at);
CREATE TABLE IF NOT EXISTS account_meta (
  key TEXT PRIMARY KEY,
  value INTEGER NOT NULL
);
INSERT INTO account_meta (key, value)
VALUES ('revision', (SELECT COALESCE(MAX(revision), 0) FROM accounts))
ON CONFLICT(key) DO NOTHING;
UPDATE account_meta
SET value = (SELECT COALESCE(MAX(revision), 0) FROM accounts)
WHERE key = 'revision'
  AND value < (SELECT COALESCE(MAX(revision), 0) FROM accounts);
`

var _ Repository = (*SQLiteRepository)(nil)

func (r *SQLiteRepository) String() string {
	return fmt.Sprintf("sqlite:%s", strings.TrimSpace(r.path))
}
