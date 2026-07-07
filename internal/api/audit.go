package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/DeliciousBuding/grok2api/internal/logger"
	"github.com/DeliciousBuding/grok2api/internal/platform"
)

const (
	adminAuditContextKey  = "admin_audit_event"
	adminAuditMaxTokenIDs = 32
)

// AdminAuditEvent is the sanitized audit record emitted for admin mutations.
type AdminAuditEvent struct {
	Time        string   `json:"time,omitempty"`
	Operation   string   `json:"operation"`
	Outcome     string   `json:"outcome"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Status      int      `json:"status,omitempty"`
	Pool        string   `json:"pool,omitempty"`
	State       string   `json:"state,omitempty"`
	MediaType   string   `json:"media_type,omitempty"`
	TokenCount  int      `json:"token_count,omitempty"`
	TokenIDs    []string `json:"token_ids,omitempty"`
	AssetIDHash string   `json:"asset_id_hash,omitempty"`
	Count       int      `json:"count,omitempty"`
	Upserted    int      `json:"upserted,omitempty"`
	Patched     int      `json:"patched,omitempty"`
	Deleted     int      `json:"deleted,omitempty"`
	Missing     int      `json:"missing,omitempty"`
	Failed      int      `json:"failed,omitempty"`
	ErrorCode   string   `json:"error_code,omitempty"`
}

// AdminAuditSink receives sanitized admin audit events.
type AdminAuditSink interface {
	RecordAdminAudit(AdminAuditEvent)
}

// AdminAuditFunc adapts a function into an AdminAuditSink.
type AdminAuditFunc func(AdminAuditEvent)

func (f AdminAuditFunc) RecordAdminAudit(event AdminAuditEvent) { f(event) }

func (s *Server) adminAuditMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if !isAdminAuditMethod(c.Request.Method) {
			return
		}
		event := adminAuditEventFromContext(c)
		if event.Operation == "" {
			event.Operation = adminAuditOperation(c.Request.Method, c.FullPath())
		}
		status := c.Writer.Status()
		if status == 0 {
			status = http.StatusOK
		}
		event.Status = status
		if status >= http.StatusBadRequest {
			event.Outcome = "failure"
		} else if event.Outcome == "" {
			event.Outcome = "success"
		}
		s.emitAdminAudit(c, event)
	}
}

func setAdminAudit(c *gin.Context, event AdminAuditEvent) {
	c.Set(adminAuditContextKey, event)
}

func adminAuditEventFromContext(c *gin.Context) AdminAuditEvent {
	raw, ok := c.Get(adminAuditContextKey)
	if !ok {
		return AdminAuditEvent{}
	}
	event, ok := raw.(AdminAuditEvent)
	if !ok {
		return AdminAuditEvent{}
	}
	return event
}

func (s *Server) emitAdminAudit(c *gin.Context, event AdminAuditEvent) {
	if event.Time == "" {
		event.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if event.Method == "" && c.Request != nil {
		event.Method = c.Request.Method
	}
	if event.Path == "" {
		if p := c.FullPath(); p != "" {
			event.Path = p
		} else if c.Request != nil && c.Request.URL != nil {
			event.Path = c.Request.URL.Path
		}
	}
	if event.Outcome == "" {
		event.Outcome = "success"
	}
	if event.TokenCount == 0 && len(event.TokenIDs) > 0 {
		event.TokenCount = len(event.TokenIDs)
	}
	if s.AdminAudit != nil {
		s.AdminAudit.RecordAdminAudit(event)
		return
	}
	b, err := json.Marshal(event)
	if err != nil {
		logger.Infof("admin_audit operation=%s outcome=%s status=%d", event.Operation, event.Outcome, event.Status)
		return
	}
	logger.Infof("admin_audit event=%s", string(b))
}

func isAdminAuditMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func adminAuditOperation(method, path string) string {
	switch method + " " + path {
	case "POST /admin/api/config":
		return "config.update"
	case "POST /admin/api/sync":
		return "sync.run"
	case "POST /admin/api/tokens":
		return "tokens.replace"
	case "POST /admin/api/tokens/add":
		return "tokens.add"
	case "DELETE /admin/api/tokens":
		return "tokens.delete"
	case "DELETE /admin/api/tokens/invalid":
		return "tokens.delete_invalid"
	case "PUT /admin/api/tokens/edit":
		return "tokens.edit"
	case "POST /admin/api/tokens/disabled":
		return "tokens.disabled"
	case "POST /admin/api/tokens/disabled/batch":
		return "tokens.disabled_batch"
	case "PUT /admin/api/pool":
		return "pool.replace"
	case "POST /admin/api/batch/nsfw":
		return "batch.nsfw"
	case "POST /admin/api/batch/refresh":
		return "batch.refresh"
	case "POST /admin/api/batch/cache-clear":
		return "batch.cache_clear"
	case "POST /admin/api/assets/delete-item":
		return "assets.delete_item"
	case "POST /admin/api/assets/clear-token":
		return "assets.clear_token"
	case "POST /admin/api/cache/clear":
		return "cache.clear"
	case "POST /admin/api/cache/item/delete":
		return "cache.item_delete"
	case "POST /admin/api/cache/items/delete":
		return "cache.items_delete"
	default:
		return strings.ToLower(method) + "." + strings.Trim(strings.ReplaceAll(path, "/", "."), ".")
	}
}

func adminAuditTokenIDs(tokens []string) []string {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(tokens))
	for _, raw := range tokens {
		token := platform.SanitizeToken(raw)
		if token == "" {
			continue
		}
		id := adminAuditHash(token)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > adminAuditMaxTokenIDs {
		ids = ids[:adminAuditMaxTokenIDs]
	}
	return ids
}

func adminAuditTokenCount(tokens []string) int {
	return len(sanitizeTokenList(tokens))
}

func adminAuditHash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}
