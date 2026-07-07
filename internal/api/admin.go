package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/DeliciousBuding/grok2api/internal/account"
	"github.com/DeliciousBuding/grok2api/internal/config"
	"github.com/DeliciousBuding/grok2api/internal/grok"
	"github.com/DeliciousBuding/grok2api/internal/logger"
	"github.com/DeliciousBuding/grok2api/internal/platform"
	"github.com/DeliciousBuding/grok2api/internal/storage"
)

// --- System endpoints ---

func (s *Server) handleStorageGet(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"type": repositoryStorageType(s.Repo)})
}

func repositoryStorageType(repo account.Repository) string {
	switch repo.(type) {
	case *account.SQLiteRepository:
		return "sqlite"
	case *account.PostgresRepository:
		return "postgres"
	case *account.TxtRepository:
		return "jsonl"
	default:
		return "unknown"
	}
}

func (s *Server) handleStatusGet(c *gin.Context) {
	if s.Directory == nil {
		writeAppError(c, platform.NewAppError("directory not initialised", platform.ErrServer, "directory_not_initialised", http.StatusServiceUnavailable))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":             "ok",
		"size":               s.Directory.Size(),
		"revision":           s.Directory.Revision(),
		"selection_strategy": string(s.Directory.Strategy()),
	})
}

func (s *Server) handleSync(c *gin.Context) {
	if s.Directory == nil {
		writeAppError(c, platform.NewAppError("directory not initialised", platform.ErrServer, "directory_not_initialised", http.StatusServiceUnavailable))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 30))
	defer cancel()
	changed, err := s.Directory.SyncIfChanged(ctx)
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"changed":  changed,
		"revision": s.Directory.Revision(),
	})
}

// --- Config ---

func (s *Server) handleConfigGet(c *gin.Context) {
	raw := config.Global().Raw()
	c.JSON(http.StatusOK, raw)
}

func (s *Server) handleConfigUpdate(c *gin.Context) {
	var patch map[string]any
	if err := readJSON(c, &patch); err != nil {
		writeAppError(c, err)
		return
	}
	if err := validatePatch(patch); err != nil {
		writeAppError(c, err)
		return
	}
	if err := config.Global().Update(patch); err != nil {
		if config.IsValidationError(err) {
			writeAppError(c, platform.ValidationErrorCode(err.Error(), "config", "invalid_config"))
			return
		}
		writeAppError(c, platform.UpstreamError("config update failed: "+err.Error(), 500, ""))
		return
	}
	_ = config.Load()
	strategy := "random"
	if config.Global().GetBool("account.refresh.enabled", false) {
		strategy = "quota"
	}
	if s.Directory != nil {
		s.Directory.SetStrategy(account.Strategy(strategy))
	}
	setAdminAudit(c, AdminAuditEvent{
		Operation: "config.update",
		Count:     len(patch),
	})
	c.JSON(http.StatusOK, gin.H{
		"status":             "success",
		"message":            "配置已更新",
		"selection_strategy": strategy,
	})
}

// validatePatch rejects startup-only config paths from runtime patches.
func validatePatch(patch map[string]any) error {
	return validatePatchNode("", patch)
}

func validatePatchNode(prefix string, patch map[string]any) error {
	for k, v := range patch {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if config.IsStartupOnlyConfigKey(path) {
			return platform.ValidationErrorCode(
				"Config key '"+path+"' is reserved for startup", path, "startup_only_config")
		}
		if child, ok := v.(map[string]any); ok {
			if err := validatePatchNode(path, child); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- Tokens CRUD ---

const (
	adminDefaultPageSize = 50
	adminMaxPageSize     = 1000

	adminDefaultBatchConcurrency = 50
	adminMaxBatchConcurrency     = 80
	adminMaxTokenMutationTokens  = 1000
	adminMaxBatchTokens          = adminMaxTokenMutationTokens
	adminMaxTokenLength          = account.MaxTokenLength
	adminMaxTags                 = account.MaxTags
	adminMaxTagLength            = account.MaxTagLength

	adminDefaultCachePageSize = 1000
	adminMaxCachePageSize     = 1000
	adminMaxCacheItemNames    = 1000

	adminDefaultAssetConcurrency = 20
	adminMaxAssetConcurrency     = 80
)

func (s *Server) handleTokensList(c *gin.Context) {
	query, err := parseAdminListQuery(c)
	if err != nil {
		writeAppError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 30))
	defer cancel()
	page, err := s.Repo.ListAccounts(ctx, query)
	if err != nil {
		writeAppError(c, err)
		return
	}
	out := []map[string]any{}
	for _, rec := range page.Items {
		out = append(out, serializeRecord(rec))
	}
	c.JSON(http.StatusOK, gin.H{
		"tokens": out,
		"pagination": gin.H{
			"page":        page.Page,
			"page_size":   page.PageSize,
			"total":       page.Total,
			"total_pages": page.TotalPages,
			"has_more":    page.Page < page.TotalPages,
			"revision":    page.Revision,
		},
	})
}

func parseAdminListQuery(c *gin.Context) (account.ListQuery, error) {
	page, err := parsePositiveQueryInt(c, "page", 1)
	if err != nil {
		return account.ListQuery{}, err
	}
	pageSize, err := parsePositiveQueryInt(c, "page_size", adminDefaultPageSize)
	if err != nil {
		return account.ListQuery{}, err
	}
	if pageSize > adminMaxPageSize {
		return account.ListQuery{}, platform.ValidationErrorCode(
			fmt.Sprintf("page_size must be <= %d", adminMaxPageSize),
			"page_size",
			"invalid_page_size",
		)
	}
	q := account.ListQuery{Page: page, PageSize: pageSize, IncludeDeleted: false}
	if pool := strings.TrimSpace(c.Query("pool")); pool != "" {
		if _, ok := account.PoolFromName(pool); !ok {
			return account.ListQuery{}, platform.ValidationErrorCode("Invalid pool '"+pool+"'", "pool", "invalid_pool")
		}
		q.Pool = pool
	}
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		st := account.Status(status)
		switch st {
		case account.StatusActive, account.StatusCooling, account.StatusExpired, account.StatusDisabled:
			q.Status = &st
		default:
			return account.ListQuery{}, platform.ValidationErrorCode("Invalid status '"+status+"'", "status", "invalid_status")
		}
	}
	return q, nil
}

func parsePositiveQueryInt(c *gin.Context, key string, def int) (int, error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, platform.ValidationErrorCode(key+" must be a positive integer", key, "invalid_"+key)
	}
	return n, nil
}

func parseBoundedPositiveQueryInt(c *gin.Context, key string, def, max int) (int, error) {
	n, err := parsePositiveQueryInt(c, key, def)
	if err != nil {
		return 0, err
	}
	if n > max {
		return 0, platform.ValidationErrorCode(
			fmt.Sprintf("%s must be <= %d", key, max),
			key,
			"invalid_"+key,
		)
	}
	return n, nil
}

func parseAdminBoolQuery(c *gin.Context, key string, def bool) (bool, error) {
	raw, ok := c.GetQuery(key)
	if !ok {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, platform.ValidationErrorCode(key+" must be true or false", key, "invalid_"+key)
	}
}

func parseAdminBatchConcurrency(c *gin.Context) (int, error) {
	return parseBoundedPositiveQueryInt(c, "concurrency", adminDefaultBatchConcurrency, adminMaxBatchConcurrency)
}

func runAdminTokenWorkers(ctx context.Context, tokens []string, concurrency int, work func(context.Context, string)) {
	if len(tokens) == 0 {
		return
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(tokens) {
		concurrency = len(tokens)
	}
	jobs := make(chan string)
	wg := sync.WaitGroup{}
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for token := range jobs {
				work(ctx, token)
				if ctx.Err() != nil {
					return
				}
			}
		}()
	}
	for _, token := range tokens {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- token:
		}
	}
	close(jobs)
	wg.Wait()
}

func serializeRecord(rec *account.Record) map[string]any {
	qs := rec.QuotaSet()
	quota := map[string]any{}
	addQ := func(name string, w *account.QuotaWindow) {
		if w == nil || w.WindowSeconds <= 0 {
			return
		}
		quota[name] = map[string]any{"remaining": w.Remaining, "total": w.Total}
	}
	auto := qs.Auto
	fast := qs.Fast
	expert := qs.Expert
	addQ("auto", &auto)
	addQ("fast", &fast)
	addQ("expert", &expert)
	addQ("heavy", qs.Heavy)
	addQ("console", qs.Console)
	lastUsed := int64(0)
	if rec.LastUseAt != nil {
		lastUsed = *rec.LastUseAt
	}
	pool := rec.Pool
	if pool == "" {
		pool = "basic"
	}
	return map[string]any{
		"token":        rec.Token,
		"pool":         pool,
		"status":       string(rec.Status),
		"quota":        quota,
		"use_count":    rec.UsageUseCount,
		"last_used_at": lastUsed,
		"tags":         rec.Tags,
	}
}

func (s *Server) handleTokensReplace(c *gin.Context) {
	var body map[string]any
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	total := 0
	allTokens := []string{}
	type poolUpserts struct {
		pool    string
		upserts []account.Upsert
	}
	ops := []poolUpserts{}
	for poolName, items := range body {
		if poolName == "" {
			writeAppError(c, platform.ValidationErrorCode("Pool name is required", "pool", "invalid_pool"))
			return
		}
		_, ok := account.PoolFromName(poolName)
		if !ok {
			writeAppError(c, platform.ValidationErrorCode("Invalid pool '"+poolName+"'", "pool", "invalid_pool"))
			return
		}
		tokenList, _ := items.([]any)
		if tokenList == nil {
			writeAppError(c, platform.ValidationErrorCode("Pool '"+poolName+"' must be an array", poolName, "invalid_pool_payload"))
			return
		}
		upserts := []account.Upsert{}
		seen := map[string]bool{}
		for _, item := range tokenList {
			var token string
			var tags []string
			switch v := item.(type) {
			case string:
				token = v
			case map[string]any:
				if t, ok := v["token"].(string); ok {
					token = t
				}
				if tagList, ok := v["tags"].([]any); ok {
					for _, t := range tagList {
						if ts, ok := t.(string); ok {
							tags = append(tags, ts)
						}
					}
				}
			}
			token, err := sanitizeAdminToken(token)
			if err != nil {
				writeAppError(c, err)
				return
			}
			if token == "" || seen[token] {
				continue
			}
			seen[token] = true
			cleanTags, err := sanitizeAccountTags(tags)
			if err != nil {
				writeAppError(c, err)
				return
			}
			upserts = append(upserts, account.Upsert{Token: token, Pool: poolName, Tags: cleanTags})
			allTokens = append(allTokens, token)
		}
		total += len(upserts)
		if err := ensureAdminTokenMutationLimit(total); err != nil {
			writeAppError(c, err)
			return
		}
		ops = append(ops, poolUpserts{pool: poolName, upserts: upserts})
	}
	for _, op := range ops {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 60))
		_, err := s.Repo.ReplacePool(ctx, op.pool, op.upserts)
		cancel()
		if err != nil {
			writeAppError(c, err)
			return
		}
	}
	tokenIDs := adminAuditTokenIDs(allTokens)
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "tokens.replace",
		TokenCount: adminAuditTokenCount(allTokens),
		TokenIDs:   tokenIDs,
		Count:      total,
		Upserted:   total,
	})
	c.JSON(http.StatusOK, gin.H{"status": "success", "count": total})
}

func (s *Server) handleTokensAdd(c *gin.Context) {
	var body struct {
		Tokens []string `json:"tokens"`
		Pool   string   `json:"pool"`
		Tags   []string `json:"tags"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	pool := body.Pool
	if pool == "" {
		pool = "basic"
	}
	autoDetect := pool == "auto"
	if autoDetect {
		pool = "basic" // temporary; will be inferred after refresh
	}
	if _, ok := account.PoolFromName(pool); !ok && !autoDetect {
		writeAppError(c, platform.ValidationErrorCode("Invalid pool '"+pool+"'", "pool", "invalid_pool"))
		return
	}
	tokens, err := sanitizeAdminTokenMutationTokens(body.Tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tags, err := sanitizeAccountTags(body.Tags)
	if err != nil {
		writeAppError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 60))
	defer cancel()

	existing, err := s.Repo.GetAccounts(ctx, tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	existingActive := map[string]bool{}
	for _, rec := range existing {
		if rec != nil && !rec.IsDeleted() {
			existingActive[rec.Token] = true
		}
	}
	var newTokens []string
	upserts := []account.Upsert{}
	skipped := 0
	for _, tok := range tokens {
		if existingActive[tok] {
			skipped++
			continue
		}
		upserts = append(upserts, account.Upsert{Token: tok, Pool: pool, Tags: tags})
		newTokens = append(newTokens, tok)
	}
	if len(upserts) > 0 {
		if _, err := s.Repo.UpsertAccounts(ctx, upserts); err != nil {
			writeAppError(c, err)
			return
		}
	}

	// Trigger async refresh + auto-detect pool for newly imported tokens.
	if len(newTokens) > 0 && s.Refresh != nil {
		if autoDetect {
			started := s.tryStartAdminBackgroundTask(timeoutClassDuration("admin", 120), func(refCtx context.Context) {
				refreshed, failed, err := s.Refresh.RefreshTokens(refCtx, newTokens)
				if err != nil {
					return
				}
				_ = refreshed
				_ = failed
				// After refresh, check if pool was auto-inferred.
				checkCtx, checkCancel := context.WithTimeout(refCtx, 10*time.Second)
				defer checkCancel()
				recs, _ := s.Repo.GetAccounts(checkCtx, newTokens)
				for _, rec := range recs {
					if rec == nil || rec.IsDeleted() {
						continue
					}
					inferred := account.InferPool(rec.QuotaSet())
					if inferred != "" && inferred != rec.Pool {
						patchCtx, patchCancel := context.WithTimeout(refCtx, 10*time.Second)
						p := rec.Pool
						patch := account.Patch{Token: rec.Token, Pool: &inferred}
						_, _ = s.Repo.PatchAccounts(patchCtx, []account.Patch{patch})
						patchCancel()
						logger.Infof("admin auto-detect pool: token=%s... previous=%s current=%s", platform.TokenLogPrefix(rec.Token), p, inferred)
					}
				}
			})
			if !started {
				logger.Warnf("admin token import refresh skipped: background capacity exhausted count=%d auto_detect=true", len(newTokens))
			}
		} else {
			started := s.tryStartAdminBackgroundTask(timeoutClassDuration("admin", 120), func(refCtx context.Context) {
				_, _, _ = s.Refresh.RefreshTokens(refCtx, newTokens)
			})
			if !started {
				logger.Warnf("admin token import refresh skipped: background capacity exhausted count=%d auto_detect=false", len(newTokens))
			}
		}
	}

	tokenIDs := adminAuditTokenIDs(newTokens)
	state := ""
	if autoDetect {
		state = "auto_detect"
	}
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "tokens.add",
		Pool:       pool,
		State:      state,
		TokenCount: adminAuditTokenCount(newTokens),
		TokenIDs:   tokenIDs,
		Count:      len(upserts),
		Upserted:   len(upserts),
	})
	c.JSON(http.StatusOK, gin.H{
		"status":  "success",
		"count":   len(upserts),
		"skipped": skipped,
	})
}

func (s *Server) handleTokensDelete(c *gin.Context) {
	var tokens []string
	if err := readJSON(c, &tokens); err != nil {
		writeAppError(c, err)
		return
	}
	clean, err := sanitizeAdminTokenMutationTokens(tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 60))
	defer cancel()
	result, err := s.Repo.DeleteAccounts(ctx, clean)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokenIDs := adminAuditTokenIDs(clean)
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "tokens.delete",
		TokenCount: adminAuditTokenCount(clean),
		TokenIDs:   tokenIDs,
		Deleted:    result.Deleted,
	})
	c.JSON(http.StatusOK, gin.H{"deleted": result.Deleted})
}

func (s *Server) handleTokensDeleteInvalid(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 60))
	defer cancel()
	page, err := s.Repo.ListAccounts(ctx, account.ListQuery{Page: 1, PageSize: 5000, IncludeDeleted: false})
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokens := []string{}
	for _, rec := range page.Items {
		if rec.Status == account.StatusActive || rec.Status == account.StatusCooling || rec.Status == account.StatusDisabled {
			continue
		}
		tokens = append(tokens, rec.Token)
	}
	if len(tokens) == 0 {
		setAdminAudit(c, AdminAuditEvent{
			Operation: "tokens.delete_invalid",
			Deleted:   0,
		})
		c.JSON(http.StatusOK, gin.H{"deleted": 0})
		return
	}
	result, err := s.Repo.DeleteAccounts(ctx, tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokenIDs := adminAuditTokenIDs(tokens)
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "tokens.delete_invalid",
		TokenCount: adminAuditTokenCount(tokens),
		TokenIDs:   tokenIDs,
		Deleted:    result.Deleted,
	})
	c.JSON(http.StatusOK, gin.H{"deleted": result.Deleted})
}

func (s *Server) handleTokensEdit(c *gin.Context) {
	var body struct {
		OldToken string `json:"old_token"`
		Token    string `json:"token"`
		Pool     string `json:"pool"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	old, err := sanitizeAdminToken(body.OldToken)
	if err != nil {
		writeAppError(c, err)
		return
	}
	newTok, err := sanitizeAdminToken(body.Token)
	if err != nil {
		writeAppError(c, err)
		return
	}
	pool := body.Pool
	if pool == "" {
		pool = "basic"
	}
	if old == "" || newTok == "" {
		writeAppError(c, platform.ValidationError("Missing token", "body"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 30))
	defer cancel()
	recs, err := s.Repo.GetAccounts(ctx, []string{old, newTok})
	if err != nil {
		writeAppError(c, err)
		return
	}
	var oldRec, newRec *account.Record
	for _, rec := range recs {
		if rec.Token == old {
			oldRec = rec
		} else if rec.Token == newTok {
			newRec = rec
		}
	}
	if oldRec == nil {
		writeAppError(c, platform.ValidationErrorCode("Account not found", "old_token", "account_not_found"))
		return
	}
	if old != newTok && newRec != nil && !newRec.IsDeleted() {
		writeAppError(c, platform.NewAppError("Token conflict", platform.ErrValidation, "token_conflict", http.StatusConflict))
		return
	}
	tags := oldRec.Tags
	ext := oldRec.Ext
	upserts := []account.Upsert{{Token: newTok, Pool: pool, Tags: tags, Ext: ext}}
	if _, err := s.Repo.UpsertAccounts(ctx, upserts); err != nil {
		writeAppError(c, err)
		return
	}
	if old != newTok {
		if _, err := s.Repo.DeleteAccounts(ctx, []string{old}); err != nil {
			writeAppError(c, err)
			return
		}
	}
	deleted := 0
	if old != newTok {
		deleted = 1
	}
	tokenIDs := adminAuditTokenIDs([]string{old, newTok})
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "tokens.edit",
		Pool:       pool,
		TokenCount: adminAuditTokenCount([]string{old, newTok}),
		TokenIDs:   tokenIDs,
		Upserted:   1,
		Deleted:    deleted,
	})
	c.JSON(http.StatusOK, gin.H{"status": "success", "token": newTok, "pool": pool})
}

func (s *Server) handleTokensToggleDisabled(c *gin.Context) {
	var body struct {
		Token    string `json:"token"`
		Disabled bool   `json:"disabled"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	token, err := sanitizeAdminToken(body.Token)
	if err != nil {
		writeAppError(c, err)
		return
	}
	if token == "" {
		writeAppError(c, platform.ValidationError("Missing token", "token"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 30))
	defer cancel()
	recs, err := s.Repo.GetAccounts(ctx, []string{token})
	if err != nil || len(recs) == 0 {
		writeAppError(c, platform.ValidationErrorCode("Account not found", "token", "account_not_found"))
		return
	}
	patches := []account.Patch{buildTogglePatch(token, body.Disabled)}
	if _, err := s.Repo.PatchAccounts(ctx, patches); err != nil {
		writeAppError(c, err)
		return
	}
	state := "active"
	if body.Disabled {
		state = "disabled"
	}
	tokenIDs := adminAuditTokenIDs([]string{token})
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "tokens.disabled",
		State:      state,
		TokenCount: adminAuditTokenCount([]string{token}),
		TokenIDs:   tokenIDs,
		Patched:    1,
	})
	c.JSON(http.StatusOK, gin.H{"status": "success", "token": token, "disabled": body.Disabled})
}

func (s *Server) handleTokensToggleDisabledBatch(c *gin.Context) {
	var body struct {
		Tokens   []string `json:"tokens"`
		Disabled bool     `json:"disabled"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	clean, err := sanitizeAdminTokenMutationTokens(body.Tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 30))
	defer cancel()
	recs, err := s.Repo.GetAccounts(ctx, clean)
	if err != nil || len(recs) == 0 {
		writeAppError(c, platform.ValidationErrorCode("No accounts found", "tokens", "account_not_found"))
		return
	}
	patches := []account.Patch{}
	for _, rec := range recs {
		patches = append(patches, buildTogglePatch(rec.Token, body.Disabled))
	}
	result, err := s.Repo.PatchAccounts(ctx, patches)
	if err != nil {
		writeAppError(c, err)
		return
	}
	patchedTokens := make([]string, 0, len(patches))
	for _, patch := range patches {
		patchedTokens = append(patchedTokens, patch.Token)
	}
	state := "active"
	if body.Disabled {
		state = "disabled"
	}
	tokenIDs := adminAuditTokenIDs(patchedTokens)
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "tokens.disabled_batch",
		State:      state,
		TokenCount: adminAuditTokenCount(patchedTokens),
		TokenIDs:   tokenIDs,
		Patched:    result.Patched,
		Failed:     len(patches) - result.Patched,
	})
	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"disabled": body.Disabled,
		"summary": gin.H{
			"total": len(patches), "ok": result.Patched, "fail": len(patches) - result.Patched,
		},
	})
}

func buildTogglePatch(token string, disabled bool) account.Patch {
	now := platform.NowMs()
	p := account.Patch{Token: token}
	if disabled {
		st := account.StatusDisabled
		p.Status = &st
		reason := "operator_disabled"
		p.StateReason = &reason
		p.ExtMerge = map[string]any{
			"disabled_at":     now,
			"disabled_reason": "operator_disabled",
		}
	} else {
		st := account.StatusActive
		p.Status = &st
		p.ClearFailures = true
	}
	return p
}

func (s *Server) handlePoolReplace(c *gin.Context) {
	var body struct {
		Pool   string   `json:"pool"`
		Tokens []string `json:"tokens"`
		Tags   []string `json:"tags"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	pool := body.Pool
	if pool == "" {
		pool = "basic"
	}
	if _, ok := account.PoolFromName(pool); !ok {
		writeAppError(c, platform.ValidationErrorCode("Invalid pool '"+pool+"'", "pool", "invalid_pool"))
		return
	}
	tokens, err := sanitizeAdminTokenMutationTokens(body.Tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tags, err := sanitizeAccountTags(body.Tags)
	if err != nil {
		writeAppError(c, err)
		return
	}
	upserts := []account.Upsert{}
	for _, tok := range tokens {
		upserts = append(upserts, account.Upsert{Token: tok, Pool: pool, Tags: tags})
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 60))
	defer cancel()
	result, err := s.Repo.ReplacePool(ctx, pool, upserts)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokenIDs := adminAuditTokenIDs(tokens)
	upserted := len(upserts)
	if result != nil && result.Upserted > 0 {
		upserted = result.Upserted
	}
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "pool.replace",
		Pool:       pool,
		TokenCount: adminAuditTokenCount(tokens),
		TokenIDs:   tokenIDs,
		Count:      len(upserts),
		Upserted:   upserted,
	})
	c.JSON(http.StatusOK, gin.H{"pool": pool, "count": len(upserts)})
}

// --- Batch operations ---

func (s *Server) handleBatchNSFW(c *gin.Context) {
	var body struct {
		Tokens []string `json:"tokens"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	enabled, err := parseAdminBoolQuery(c, "enabled", true)
	if err != nil {
		writeAppError(c, err)
		return
	}
	conc, err := parseAdminBatchConcurrency(c)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokens, err := sanitizeAdminBatchTokens(body.Tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 300))
	defer cancel()
	results := map[string]any{}
	mu := sync.Mutex{}
	successCount := 0
	failedCount := 0
	runAdminTokenWorkers(ctx, tokens, conc, func(ctx context.Context, t string) {
		err := s.runNSFWOne(ctx, t, enabled)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			results[maskToken(t)] = gin.H{"error": err.Error()}
			failedCount++
		} else {
			results[maskToken(t)] = gin.H{"success": true}
			successCount++
		}
	})
	tokenIDs := adminAuditTokenIDs(tokens)
	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "batch.nsfw",
		State:      state,
		TokenCount: adminAuditTokenCount(tokens),
		TokenIDs:   tokenIDs,
		Count:      successCount,
		Failed:     failedCount,
	})
	c.JSON(http.StatusOK, gin.H{
		"status":  "success",
		"results": results,
	})
}

func (s *Server) runNSFWOne(ctx context.Context, token string, enabled bool) error {
	if enabled {
		return grok.NSFWSequence(ctx, s.Transport, token)
	}
	_, err := grok.DisableNSFW(ctx, s.Transport, token)
	return err
}

func (s *Server) handleBatchRefresh(c *gin.Context) {
	var body struct {
		Tokens []string `json:"tokens"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	conc, err := parseAdminBatchConcurrency(c)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokens, err := sanitizeAdminBatchTokens(body.Tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	if s.Refresh == nil {
		writeAppError(c, platform.NewAppError("refresh service not available", platform.ErrServer, "no_refresh", 503))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 300))
	defer cancel()
	results := map[string]any{}
	mu := sync.Mutex{}
	refreshedCount := 0
	failedCount := 0
	runAdminTokenWorkers(ctx, tokens, conc, func(ctx context.Context, t string) {
		refreshed, _, err := s.Refresh.RefreshTokens(ctx, []string{t})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			results[maskToken(t)] = gin.H{"error": err.Error()}
			failedCount++
		} else {
			results[maskToken(t)] = gin.H{"refreshed": refreshed > 0}
			if refreshed > 0 {
				refreshedCount++
			}
		}
	})
	tokenIDs := adminAuditTokenIDs(tokens)
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "batch.refresh",
		TokenCount: adminAuditTokenCount(tokens),
		TokenIDs:   tokenIDs,
		Count:      refreshedCount,
		Failed:     failedCount,
	})
	c.JSON(http.StatusOK, gin.H{
		"status":  "success",
		"results": results,
	})
}

func (s *Server) handleBatchCacheClear(c *gin.Context) {
	var body struct {
		Tokens []string `json:"tokens"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	conc, err := parseAdminBatchConcurrency(c)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokens, err := sanitizeAdminBatchTokens(body.Tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	if s.Refresh == nil {
		writeAppError(c, platform.NewAppError("refresh service not available", platform.ErrServer, "no_refresh", 503))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 300))
	defer cancel()
	results := map[string]any{}
	mu := sync.Mutex{}
	deletedTotal := 0
	failedCount := 0
	runAdminTokenWorkers(ctx, tokens, conc, func(ctx context.Context, t string) {
		deleted, err := s.clearTokenAssets(ctx, t)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			results[maskToken(t)] = gin.H{"error": err.Error()}
			failedCount++
		} else {
			results[maskToken(t)] = gin.H{"deleted": deleted}
			deletedTotal += deleted
		}
	})
	tokenIDs := adminAuditTokenIDs(tokens)
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "batch.cache_clear",
		TokenCount: adminAuditTokenCount(tokens),
		TokenIDs:   tokenIDs,
		Deleted:    deletedTotal,
		Failed:     failedCount,
	})
	c.JSON(http.StatusOK, gin.H{
		"status":  "success",
		"results": results,
	})
}

func (s *Server) clearTokenAssets(ctx context.Context, token string) (int, error) {
	resp, err := grok.ListAssets(ctx, s.Transport, token)
	if err != nil {
		return 0, err
	}
	items := extractAssetItems(resp)
	deleted := clearAssetIDs(ctx, items, func(ctx context.Context, assetID string) error {
		_, err := grok.DeleteAsset(ctx, s.Transport, token, assetID)
		return err
	})
	return deleted, nil
}

func clearAssetIDs(ctx context.Context, items []string, deleteAsset func(context.Context, string) error) int {
	deleted := 0
	for _, assetID := range items {
		if ctx.Err() != nil {
			break
		}
		if assetID == "" {
			continue
		}
		if err := deleteAsset(ctx, assetID); err == nil {
			deleted++
		}
	}
	return deleted
}

// extractAssetItems returns the list of asset IDs from a ListAssets response.
func extractAssetItems(resp map[string]any) []string {
	raw, _ := resp["assets"].([]any)
	if raw == nil {
		raw, _ = resp["items"].([]any)
	}
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := item["id"].(string); id != "" {
			out = append(out, id)
		} else if id, _ := item["assetId"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// extractAssetRows returns a normalized list of asset rows from a ListAssets response.
func extractAssetRows(token string, resp map[string]any, err error) map[string]any {
	if err != nil {
		return map[string]any{
			"token":  token,
			"masked": maskToken(token),
			"count":  0,
			"assets": []any{},
			"error":  err.Error(),
		}
	}
	raw, _ := resp["assets"].([]any)
	if raw == nil {
		raw, _ = resp["items"].([]any)
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id, _ := item["id"].(string)
		if id == "" {
			id, _ = item["assetId"].(string)
		}
		name, _ := item["fileName"].(string)
		if name == "" {
			name, _ = item["name"].(string)
		}
		filePath, _ := item["filePath"].(string)
		if filePath == "" {
			filePath, _ = item["file_path"].(string)
		}
		contentType, _ := item["contentType"].(string)
		if contentType == "" {
			contentType, _ = item["content_type"].(string)
		}
		size := 0
		if v, ok := item["fileSize"].(float64); ok {
			size = int(v)
		} else if v, ok := item["size"].(float64); ok {
			size = int(v)
		}
		createdAt, _ := item["createdAt"].(string)
		if createdAt == "" {
			createdAt, _ = item["created_at"].(string)
		}
		rows = append(rows, map[string]any{
			"id":           id,
			"name":         name,
			"file_path":    filePath,
			"content_type": contentType,
			"size":         size,
			"created_at":   createdAt,
		})
	}
	return map[string]any{
		"token":  token,
		"masked": maskToken(token),
		"count":  len(rows),
		"assets": rows,
	}
}

// --- Assets ---

func (s *Server) handleAssetsList(c *gin.Context) {
	query, err := parseAdminListQuery(c)
	if err != nil {
		writeAppError(c, err)
		return
	}
	conc, err := parseBoundedPositiveQueryInt(c, "concurrency", adminDefaultAssetConcurrency, adminMaxAssetConcurrency)
	if err != nil {
		writeAppError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 60))
	defer cancel()
	page, err := s.Repo.ListAccounts(ctx, query)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokens := make([]string, 0, len(page.Items))
	for _, rec := range page.Items {
		tokens = append(tokens, rec.Token)
	}
	results, totalAssets := collectAssetRows(ctx, tokens, conc, func(ctx context.Context, token string) (map[string]any, error) {
		return grok.ListAssets(ctx, s.Transport, token)
	})
	c.JSON(http.StatusOK, gin.H{
		"tokens":       results,
		"total_assets": totalAssets,
		"pagination": gin.H{
			"page":        page.Page,
			"page_size":   page.PageSize,
			"total":       page.Total,
			"total_pages": page.TotalPages,
			"has_more":    page.Page < page.TotalPages,
			"revision":    page.Revision,
		},
	})
}

type assetListFunc func(context.Context, string) (map[string]any, error)

func collectAssetRows(ctx context.Context, tokens []string, concurrency int, list assetListFunc) ([]map[string]any, int) {
	if len(tokens) == 0 {
		return []map[string]any{}, 0
	}
	if err := ctx.Err(); err != nil {
		return []map[string]any{}, 0
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(tokens) {
		concurrency = len(tokens)
	}
	jobs := make(chan string)
	results := make([]map[string]any, 0, len(tokens))
	totalAssets := 0
	mu := sync.Mutex{}
	wg := sync.WaitGroup{}
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for token := range jobs {
				var resp map[string]any
				var err error
				if ctxErr := ctx.Err(); ctxErr != nil {
					err = ctxErr
				} else {
					resp, err = list(ctx, token)
				}
				row := extractAssetRows(token, resp, err)
				mu.Lock()
				results = append(results, row)
				if count, _ := row["count"].(int); count > 0 {
					totalAssets += count
				}
				mu.Unlock()
			}
		}()
	}
	for _, token := range tokens {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return results, totalAssets
		case jobs <- token:
		}
	}
	close(jobs)
	wg.Wait()
	return results, totalAssets
}

func (s *Server) handleAssetsDeleteItem(c *gin.Context) {
	var body struct {
		Token        string `json:"token"`
		AssetID      string `json:"asset_id"`
		AssetIDCamel string `json:"assetId"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	token, err := sanitizeAdminToken(body.Token)
	if err != nil {
		writeAppError(c, err)
		return
	}
	if token == "" {
		writeAppError(c, platform.ValidationErrorCode("Missing token", "token", "missing_token"))
		return
	}
	assetID := strings.TrimSpace(body.AssetID)
	if assetID == "" {
		assetID = strings.TrimSpace(body.AssetIDCamel)
	}
	if assetID == "" {
		writeAppError(c, platform.ValidationErrorCode("Missing asset_id", "asset_id", "missing_asset_id"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 30))
	defer cancel()
	if _, err := grok.DeleteAsset(ctx, s.Transport, token, assetID); err != nil {
		writeAppError(c, err)
		return
	}
	tokenIDs := adminAuditTokenIDs([]string{token})
	setAdminAudit(c, AdminAuditEvent{
		Operation:   "assets.delete_item",
		TokenCount:  adminAuditTokenCount([]string{token}),
		TokenIDs:    tokenIDs,
		AssetIDHash: adminAuditHash(assetID),
		Deleted:     1,
	})
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (s *Server) handleAssetsClearToken(c *gin.Context) {
	var body struct {
		Token   string `json:"token"`
		Confirm bool   `json:"confirm"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	token, err := sanitizeAdminToken(body.Token)
	if err != nil {
		writeAppError(c, err)
		return
	}
	if token == "" {
		writeAppError(c, platform.ValidationErrorCode("Missing token", "token", "missing_token"))
		return
	}
	if !body.Confirm {
		writeAppError(c, platform.ValidationErrorCode("Set confirm=true to clear all assets for a token", "confirm", "confirmation_required"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeoutClassDuration("admin", 60))
	defer cancel()
	deleted, err := s.clearTokenAssets(ctx, token)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokenIDs := adminAuditTokenIDs([]string{token})
	setAdminAudit(c, AdminAuditEvent{
		Operation:  "assets.clear_token",
		TokenCount: adminAuditTokenCount([]string{token}),
		TokenIDs:   tokenIDs,
		Deleted:    deleted,
	})
	c.JSON(http.StatusOK, gin.H{"status": "success", "deleted": deleted})
}

// --- Cache ---

func (s *Server) handleCacheStats(c *gin.Context) {
	imgStats := cacheStatsFor(storage.MediaImage)
	vidStats := cacheStatsFor(storage.MediaVideo)
	c.JSON(http.StatusOK, gin.H{
		"local_image": imgStats,
		"local_video": vidStats,
	})
}

func cacheStatsFor(mediaType storage.MediaType) map[string]any {
	var dir string
	var err error
	if mediaType == storage.MediaImage {
		dir, err = storage.ImageFilesDir()
	} else {
		dir, err = storage.VideoFilesDir()
	}
	if err != nil {
		return gin.H{"count": 0, "size_bytes": 0, "error": err.Error()}
	}
	entries, _ := os.ReadDir(dir)
	count := 0
	var size int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		count++
		size += info.Size()
	}
	limitMB := config.Global().GetInt("cache.local."+string(mediaType)+"_max_mb", 0)
	limitBytes := int64(limitMB) * 1024 * 1024
	usageRatio := 0.0
	usagePercent := 0.0
	if limitBytes > 0 {
		usageRatio = float64(size) / float64(limitBytes)
		usagePercent = usageRatio * 100.0
	}
	return gin.H{
		"count":         count,
		"size_mb":       float64(size) / 1024.0 / 1024.0,
		"size_bytes":    size,
		"limit_mb":      limitMB,
		"limit_bytes":   limitBytes,
		"limited":       limitBytes > 0,
		"usage_ratio":   usageRatio,
		"usage_percent": usagePercent,
	}
}

func (s *Server) handleCacheList(c *gin.Context) {
	mediaType, err := parseAdminCacheTypeQuery(c)
	if err != nil {
		writeAppError(c, err)
		return
	}
	page, err := parsePositiveQueryInt(c, "page", 1)
	if err != nil {
		writeAppError(c, err)
		return
	}
	pageSize, err := parseBoundedPositiveQueryInt(c, "page_size", adminDefaultCachePageSize, adminMaxCachePageSize)
	if err != nil {
		writeAppError(c, err)
		return
	}
	var dir string
	if mediaType == storage.MediaImage {
		dir, err = storage.ImageFilesDir()
	} else {
		dir, err = storage.VideoFilesDir()
	}
	if err != nil {
		writeAppError(c, err)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, gin.H{"status": "success", "total": 0, "page": page, "page_size": pageSize, "items": []any{}})
			return
		}
		writeAppError(c, err)
		return
	}
	type fileItem struct {
		name       string
		size       int64
		modifiedAt float64
	}
	items := []fileItem{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, fileItem{name: e.Name(), size: info.Size(), modifiedAt: float64(info.ModTime().UnixMilli())})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].modifiedAt > items[j].modifiedAt })
	total := len(items)
	totalPages := 1
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	pageItems := items[start:end]
	out := []map[string]any{}
	for _, it := range pageItems {
		out = append(out, map[string]any{
			"name":        it.name,
			"size_bytes":  it.size,
			"modified_at": it.modifiedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    "success",
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"pagination": gin.H{
			"page":        page,
			"page_size":   pageSize,
			"total":       total,
			"total_pages": totalPages,
			"has_more":    page < totalPages,
		},
		"items": out,
	})
}

func parseAdminCacheTypeQuery(c *gin.Context) (storage.MediaType, error) {
	raw := c.Query("cache_type")
	if raw == "" {
		raw = c.Query("type")
	}
	return parseAdminCacheTypeValue(raw)
}

func parseAdminCacheTypeValue(raw string) (storage.MediaType, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "image":
		return storage.MediaImage, nil
	case "video":
		return storage.MediaVideo, nil
	default:
		return "", platform.ValidationErrorCode("cache type must be image or video", "type", "invalid_cache_type")
	}
}

func (s *Server) mediaStore() *storage.LocalMediaCacheStore {
	if s.Media == nil {
		s.Media = storage.NewLocalMediaCacheStore()
	}
	return s.Media
}

func (s *Server) handleCacheClear(c *gin.Context) {
	var body struct {
		Type string `json:"type"`
	}
	if err := readOptionalJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	mediaType, err := parseAdminCacheTypeValue(body.Type)
	if err != nil {
		writeAppError(c, err)
		return
	}
	removed, err := s.mediaStore().Clear(mediaType)
	if err != nil {
		writeAppError(c, err)
		return
	}
	setAdminAudit(c, AdminAuditEvent{
		Operation: "cache.clear",
		MediaType: string(mediaType),
		Deleted:   removed,
	})
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"result": gin.H{"removed": removed},
	})
}

func (s *Server) handleCacheItemDelete(c *gin.Context) {
	var body struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	if body.Name == "" {
		writeAppError(c, platform.ValidationErrorCode("Missing file name", "name", "missing_file_name"))
		return
	}
	mediaType, err := parseAdminCacheTypeValue(body.Type)
	if err != nil {
		writeAppError(c, err)
		return
	}
	ok, err := s.mediaStore().Delete(mediaType, body.Name)
	if err != nil {
		writeAppError(c, platform.ValidationErrorCode(err.Error(), "name", "invalid_file_name"))
		return
	}
	if !ok {
		writeAppError(c, platform.ValidationErrorCode("File not found", "name", "file_not_found"))
		return
	}
	setAdminAudit(c, AdminAuditEvent{
		Operation: "cache.item_delete",
		MediaType: string(mediaType),
		Deleted:   1,
	})
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"result": gin.H{"deleted": body.Name},
	})
}

func (s *Server) handleCacheItemsDelete(c *gin.Context) {
	var body struct {
		Type  string   `json:"type"`
		Names []string `json:"names"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	clean, err := sanitizeAdminCacheItemNames(body.Names)
	if err != nil {
		writeAppError(c, err)
		return
	}
	mediaType, err := parseAdminCacheTypeValue(body.Type)
	if err != nil {
		writeAppError(c, err)
		return
	}
	deleted := 0
	missing := 0
	for _, name := range clean {
		ok, err := s.mediaStore().Delete(mediaType, name)
		if err != nil || !ok {
			missing++
			continue
		}
		deleted++
	}
	setAdminAudit(c, AdminAuditEvent{
		Operation: "cache.items_delete",
		MediaType: string(mediaType),
		Count:     len(clean),
		Deleted:   deleted,
		Missing:   missing,
	})
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"result": gin.H{"deleted": deleted, "missing": missing},
	})
}

// --- helpers ---

func sanitizeAdminToken(raw string) (string, error) {
	token := platform.SanitizeToken(raw)
	if len(token) > adminMaxTokenLength {
		return "", platform.ValidationErrorCode(
			fmt.Sprintf("tokens must be <= %d characters", adminMaxTokenLength),
			"tokens",
			"token_too_long",
		)
	}
	return token, nil
}

func sanitizeTokenList(raw []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range raw {
		var err error
		t, err = sanitizeAdminToken(t)
		if err != nil {
			return nil, err
		}
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out, nil
}

func sanitizeAdminBatchTokens(raw []string) ([]string, error) {
	return sanitizeBoundedAdminTokens(raw, adminMaxBatchTokens)
}

func sanitizeAdminTokenMutationTokens(raw []string) ([]string, error) {
	return sanitizeBoundedAdminTokens(raw, adminMaxTokenMutationTokens)
}

func sanitizeBoundedAdminTokens(raw []string, max int) ([]string, error) {
	tokens, err := sanitizeTokenList(raw)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, platform.ValidationError("No tokens provided", "tokens")
	}
	if len(tokens) > max {
		return nil, platform.ValidationErrorCode(
			fmt.Sprintf("tokens must be <= %d", max),
			"tokens",
			"too_many_tokens",
		)
	}
	return tokens, nil
}

func ensureAdminTokenMutationLimit(n int) error {
	if n > adminMaxTokenMutationTokens {
		return platform.ValidationErrorCode(
			fmt.Sprintf("tokens must be <= %d", adminMaxTokenMutationTokens),
			"tokens",
			"too_many_tokens",
		)
	}
	return nil
}

func sanitizeAccountTags(raw []string) ([]string, error) {
	seen := map[string]bool{}
	for _, tag := range raw {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len(tag) > adminMaxTagLength {
			return nil, platform.ValidationErrorCode(
				fmt.Sprintf("tags must be <= %d characters", adminMaxTagLength),
				"tags",
				"tag_too_long",
			)
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		if len(seen) > adminMaxTags {
			return nil, platform.ValidationErrorCode(
				fmt.Sprintf("tags must be <= %d", adminMaxTags),
				"tags",
				"too_many_tags",
			)
		}
	}
	clean := make([]string, 0, len(seen))
	for tag := range seen {
		clean = append(clean, tag)
	}
	sort.Strings(clean)
	return clean, nil
}

func sanitizeAdminCacheItemNames(raw []string) ([]string, error) {
	clean := []string{}
	for _, n := range raw {
		n = strings.TrimSpace(n)
		if n != "" {
			clean = append(clean, n)
		}
	}
	if len(clean) == 0 {
		return nil, platform.ValidationErrorCode("Missing file names", "names", "missing_file_names")
	}
	if len(clean) > adminMaxCacheItemNames {
		return nil, platform.ValidationErrorCode(
			fmt.Sprintf("names must be <= %d", adminMaxCacheItemNames),
			"names",
			"too_many_file_names",
		)
	}
	return clean, nil
}

func maskToken(t string) string {
	if len(t) <= 20 {
		return t
	}
	return t[:8] + "..." + t[len(t)-8:]
}

// _ unused imports to silence linter when paths evolve.
var (
	_ = filepath.Join
	_ = json.Marshal
	_ = fmt.Sprintf
	_ = grok.DefaultUserAgent
)
