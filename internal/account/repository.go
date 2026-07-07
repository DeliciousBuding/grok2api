package account

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/DeliciousBuding/grok2api/internal/platform"
)

// Upsert is a create-or-replace account command.
type Upsert struct {
	Token string
	Pool  string
	Tags  []string
	Ext   map[string]any
}

const (
	MaxTokenLength  = 4096
	MaxTags         = 10
	MaxTagLength    = 64
	MaxReasonLength = 512
)

var ErrTokenTooLong = errors.New("token_too_long: account token exceeds maximum length")
var ErrTagTooLong = errors.New("tag_too_long: account tag exceeds maximum length")
var ErrTooManyTags = errors.New("too_many_tags: account has too many tags")

func NormalizeAccountToken(raw string) (string, error) {
	token := platform.SanitizeToken(raw)
	if len(token) > MaxTokenLength {
		return "", ErrTokenTooLong
	}
	return token, nil
}

func NormalizeAccountTags(raw []string) ([]string, error) {
	seen := map[string]bool{}
	for _, tag := range raw {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len(tag) > MaxTagLength {
			return nil, ErrTagTooLong
		}
		seen[tag] = true
		if len(seen) > MaxTags {
			return nil, ErrTooManyTags
		}
	}
	out := make([]string, 0, len(seen))
	for tag := range seen {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out, nil
}

func NormalizeAccountReason(raw string) string {
	reason := strings.TrimSpace(raw)
	reason = strings.ReplaceAll(reason, "\r", " ")
	reason = strings.ReplaceAll(reason, "\n", " ")
	if len(reason) > MaxReasonLength {
		reason = reason[:MaxReasonLength]
	}
	return reason
}

func normalizeUpserts(items []Upsert, forcedPool string) ([]Upsert, error) {
	out := make([]Upsert, 0, len(items))
	for _, it := range items {
		tok, err := NormalizeAccountToken(it.Token)
		if err != nil {
			return nil, err
		}
		if tok == "" {
			continue
		}
		pool := it.Pool
		if forcedPool != "" {
			pool = forcedPool
		} else if _, ok := PoolFromName(pool); !ok {
			pool = "basic"
		}
		ext := it.Ext
		if ext == nil {
			ext = map[string]any{}
		}
		tags, err := NormalizeAccountTags(it.Tags)
		if err != nil {
			return nil, err
		}
		out = append(out, Upsert{
			Token: tok,
			Pool:  pool,
			Tags:  tags,
			Ext:   ext,
		})
	}
	return out, nil
}

func normalizePatches(patches []Patch) ([]Patch, error) {
	out := make([]Patch, len(patches))
	copy(out, patches)
	for i := range out {
		tok, err := NormalizeAccountToken(out[i].Token)
		if err != nil {
			return nil, err
		}
		out[i].Token = tok
		if out[i].Tags != nil {
			tags, err := NormalizeAccountTags(out[i].Tags)
			if err != nil {
				return nil, err
			}
			out[i].Tags = tags
		}
		if len(out[i].AddTags) > 0 {
			tags, err := NormalizeAccountTags(out[i].AddTags)
			if err != nil {
				return nil, err
			}
			out[i].AddTags = tags
		}
		if len(out[i].RemoveTags) > 0 {
			tags, err := NormalizeAccountTags(out[i].RemoveTags)
			if err != nil {
				return nil, err
			}
			out[i].RemoveTags = tags
		}
		if out[i].LastFailReason != nil {
			reason := NormalizeAccountReason(*out[i].LastFailReason)
			out[i].LastFailReason = &reason
		}
		if out[i].StateReason != nil {
			reason := NormalizeAccountReason(*out[i].StateReason)
			out[i].StateReason = &reason
		}
	}
	return out, nil
}

func patchTags(current []string, p Patch) ([]string, bool, error) {
	if p.Tags == nil && len(p.AddTags) == 0 && len(p.RemoveTags) == 0 {
		return current, false, nil
	}
	tags := current
	if p.Tags != nil {
		tags = p.Tags
	}
	if len(p.AddTags) > 0 || len(p.RemoveTags) > 0 {
		tags = MergeTags(tags, p.AddTags, p.RemoveTags)
	}
	tags, err := NormalizeAccountTags(tags)
	if err != nil {
		return nil, false, err
	}
	return tags, true, nil
}

// Patch mutates an existing account (only set fields are applied).
type Patch struct {
	Token          string
	Pool           *string
	Status         *Status
	Tags           []string
	AddTags        []string
	RemoveTags     []string
	QuotaAuto      *map[string]any
	QuotaFast      *map[string]any
	QuotaExpert    *map[string]any
	QuotaHeavy     *map[string]any
	QuotaGrok43    *map[string]any
	QuotaConsole   *map[string]any
	UsageUseDelta  *int
	UsageFailDelta *int
	UsageSyncDelta *int
	LastUseAt      *int64
	LastFailAt     *int64
	LastFailReason *string
	LastSyncAt     *int64
	LastClearAt    *int64
	StateReason    *string
	ExtMerge       map[string]any
	ClearFailures  bool
}

// ListQuery filters and paginates the account list.
type ListQuery struct {
	Page           int
	PageSize       int
	Pool           string
	Status         *Status
	Tags           []string
	IncludeDeleted bool
	SortBy         string
	SortDesc       bool
}

// Page is a paginated slice of records.
type Page struct {
	Items      []*Record
	Total      int
	Page       int
	PageSize   int
	TotalPages int
	Revision   int
}

// MutationResult summarizes a repository mutation.
type MutationResult struct {
	Upserted int
	Patched  int
	Deleted  int
	Revision int
}

// ChangeSet is an incremental scan result.
type ChangeSet struct {
	Revision      int
	BatchMaxRev   int
	Items         []*Record
	DeletedTokens []string
	HasMore       bool
}

// Snapshot is the full runtime view.
type Snapshot struct {
	Revision int
	Items    []*Record
}

// Repository is the storage contract for accounts.
type Repository interface {
	Initialize(ctx context.Context) error
	GetRevision(ctx context.Context) (int, error)
	RuntimeSnapshot(ctx context.Context) (*Snapshot, error)
	ScanChanges(ctx context.Context, since int, limit int) (*ChangeSet, error)
	UpsertAccounts(ctx context.Context, items []Upsert) (*MutationResult, error)
	PatchAccounts(ctx context.Context, patches []Patch) (*MutationResult, error)
	DeleteAccounts(ctx context.Context, tokens []string) (*MutationResult, error)
	GetAccounts(ctx context.Context, tokens []string) ([]*Record, error)
	ListAccounts(ctx context.Context, query ListQuery) (*Page, error)
	ReplacePool(ctx context.Context, pool string, upserts []Upsert) (*MutationResult, error)
	ResetExpiredConsoleWindows(ctx context.Context) (int, error)
	RecoverConsoleExpiredAccounts(ctx context.Context) (int, error)
	Close(ctx context.Context) error
}
