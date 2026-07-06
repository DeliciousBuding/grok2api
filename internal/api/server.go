package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/DeliciousBuding/grok2api/internal/account"
	"github.com/DeliciousBuding/grok2api/internal/admission"
	"github.com/DeliciousBuding/grok2api/internal/config"
	"github.com/DeliciousBuding/grok2api/internal/grok"
	"github.com/DeliciousBuding/grok2api/internal/metrics"
	"github.com/DeliciousBuding/grok2api/internal/platform"
	"github.com/DeliciousBuding/grok2api/internal/storage"
)

// Server bundles the dependencies every handler needs.
type Server struct {
	Repo       account.Repository
	Directory  *account.Directory
	Refresh    *account.RefreshService
	Transport  *grok.Transport
	Media      *storage.LocalMediaCacheStore
	Admission  *admission.Controller
	Metrics    *metrics.Registry
	AdminAudit AdminAuditSink

	adminBackground chan struct{}
}

// NewServer constructs a Server bound to the given dependencies.
func NewServer(repo account.Repository, dir *account.Directory, refresh *account.RefreshService, transport *grok.Transport, media *storage.LocalMediaCacheStore) *Server {
	return &Server{
		Repo:      repo,
		Directory: dir,
		Refresh:   refresh,
		Transport: transport,
		Media:     media,
		Admission: admission.NewController(),
		Metrics:   metrics.NewRegistry(),

		adminBackground: make(chan struct{}, adminBackgroundConcurrency),
	}
}

const adminBackgroundConcurrency = 2

func (s *Server) tryStartAdminBackgroundTask(timeout time.Duration, work func(context.Context)) bool {
	if work == nil {
		return false
	}
	if s.adminBackground == nil {
		s.adminBackground = make(chan struct{}, adminBackgroundConcurrency)
	}
	select {
	case s.adminBackground <- struct{}{}:
	default:
		return false
	}
	go func() {
		defer func() { <-s.adminBackground }()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		work(ctx)
	}()
	return true
}

// Router builds the gin.Engine for the whole API surface.
func (s *Server) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(logMiddleware())
	engine.Use(configReloadMiddleware())
	engine.Use(s.metricsMiddleware())
	engine.Use(requestSizeMiddleware())
	engine.Use(s.globalAdmissionMiddleware())
	engine.Use(corsMiddleware())

	// Health/meta (no auth).
	engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	engine.GET("/meta", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": "1.0.0"})
	})
	engine.GET("/metrics", s.handleMetrics)
	engine.GET("/ready", s.handleReady)

	// Public local media serving (no auth — file IDs are unguessable).
	engine.GET("/v1/files/image", s.handleFileImage)
	engine.GET("/v1/files/video", s.handleFileVideo)

	// OpenAI-compatible endpoints.
	v1 := engine.Group("/v1")
	v1.Use(verifyAPIKey())
	{
		v1.GET("/models", s.handleModels)
		v1.GET("/models/:id", s.handleModelGet)
		v1.POST("/chat/completions", s.handleChatCompletions)
		v1.POST("/responses", s.handleResponses)
		v1.POST("/images/generations", s.handleImageGenerations)
		v1.POST("/images/edits", s.handleImageEdits)
		v1.POST("/videos", s.handleVideoCreate)
		v1.GET("/videos/:id", s.handleVideoGet)
		v1.GET("/videos/:id/content", s.handleVideoGet)
	}

	// Anthropic-compatible endpoints.
	msg := engine.Group("/v1/messages")
	msg.Use(verifyAPIKey())
	{
		msg.POST("", s.handleMessages)
	}

	// Admin endpoints.
	admin := engine.Group("/admin/api")
	admin.Use(verifyAdminKey())
	admin.Use(s.adminAuditMiddleware())
	{
		admin.GET("/verify", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "success"})
		})
		admin.GET("/config", s.handleConfigGet)
		admin.POST("/config", s.handleConfigUpdate)
		admin.GET("/storage", s.handleStorageGet)
		admin.GET("/status", s.handleStatusGet)
		admin.POST("/sync", s.handleSync)
		admin.GET("/tokens", s.handleTokensList)
		admin.POST("/tokens", s.handleTokensReplace)
		admin.POST("/tokens/add", s.handleTokensAdd)
		admin.DELETE("/tokens", s.handleTokensDelete)
		admin.DELETE("/tokens/invalid", s.handleTokensDeleteInvalid)
		admin.PUT("/tokens/edit", s.handleTokensEdit)
		admin.POST("/tokens/disabled", s.handleTokensToggleDisabled)
		admin.POST("/tokens/disabled/batch", s.handleTokensToggleDisabledBatch)
		admin.PUT("/pool", s.handlePoolReplace)
		admin.POST("/batch/nsfw", s.handleBatchNSFW)
		admin.POST("/batch/refresh", s.handleBatchRefresh)
		admin.POST("/batch/cache-clear", s.handleBatchCacheClear)
		admin.GET("/assets", s.handleAssetsList)
		admin.POST("/assets/delete-item", s.handleAssetsDeleteItem)
		admin.POST("/assets/clear-token", s.handleAssetsClearToken)
		admin.GET("/cache", s.handleCacheStats)
		admin.GET("/cache/list", s.handleCacheList)
		admin.POST("/cache/clear", s.handleCacheClear)
		admin.POST("/cache/item/delete", s.handleCacheItemDelete)
		admin.POST("/cache/items/delete", s.handleCacheItemsDelete)
	}

	return engine
}

// configReloadMiddleware re-checks the config files on every request.
func configReloadMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		_ = config.LoadIfStale(requestConfigReloadMinInterval)
		c.Next()
	}
}

const requestConfigReloadMinInterval = 500 * time.Millisecond

// --- shared helpers ---

func writeJSON(c *gin.Context, status int, v any) {
	c.JSON(status, v)
}

func writeAppError(c *gin.Context, err error) {
	var appErr *platform.AppError
	if errors.As(err, &appErr) {
		c.JSON(appErr.Status, appErr.ToDict())
		return
	}
	appErr = platform.NewAppError(err.Error(), platform.ErrServer, "internal_error", 500)
	c.JSON(http.StatusInternalServerError, appErr.ToDict())
}

// readJSON decodes the request body into v using gin's binding.
func readJSON(c *gin.Context, v any) error {
	if err := c.ShouldBindJSON(v); err != nil {
		if isRequestBodyTooLarge(err) {
			return platform.NewAppError("Request body too large", platform.ErrValidation, "request_body_too_large", http.StatusRequestEntityTooLarge)
		}
		return platform.ValidationError("Invalid JSON body: "+err.Error(), "body")
	}
	return nil
}

func isRequestBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return true
	}
	return strings.Contains(err.Error(), "http: request body too large")
}

func readOptionalJSON(c *gin.Context, v any) error {
	if c.Request == nil || c.Request.Body == nil || c.Request.ContentLength == 0 {
		return nil
	}
	return readJSON(c, v)
}

// corsMiddleware adds permissive CORS headers.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func requestSizeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := config.Global().GetInt("server.max_body_bytes", 0)
		if limit > 0 {
			if c.Request.ContentLength > int64(limit) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
					"error": gin.H{
						"message": "Request body too large",
						"type":    "invalid_request_error",
						"code":    "request_body_too_large",
					},
				})
				return
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(limit))
		}
		c.Next()
	}
}

func (s *Server) metricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		s.metricsRegistry().ObserveRequestDuration(
			c.Request.Method,
			path,
			c.Writer.Status(),
			time.Since(start).Seconds(),
		)
	}
}

func (s *Server) globalAdmissionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions || c.Request.Method == http.MethodGet {
			c.Next()
			return
		}
		if s.Admission == nil {
			c.Next()
			return
		}
		limit := config.Global().GetInt("admission.global_max_inflight", 0)
		release, ok := s.Admission.TryAcquire("global", limit)
		if !ok {
			writeAdmissionRejected(c, "global")
			return
		}
		defer release()
		c.Next()
	}
}

func (s *Server) acquireModelAdmission(c *gin.Context, modelName string) (func(), bool) {
	if s.Admission == nil {
		return func() {}, true
	}
	limit := config.Global().GetInt("admission.per_model_max_inflight", 0)
	scope := "model:" + modelName
	release, ok := s.Admission.TryAcquire(scope, limit)
	if !ok {
		writeAdmissionRejected(c, scope)
		return nil, false
	}
	return release, true
}

func writeAdmissionRejected(c *gin.Context, scope string) {
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"message": "Admission control exhausted",
			"type":    "rate_limit_error",
			"code":    "admission_control_exhausted",
			"scope":   scope,
		},
	})
}

func (s *Server) handleMetrics(c *gin.Context) {
	total := 0
	active := 0
	inflight := 0
	if s.Directory != nil {
		slots := s.Directory.Snapshot()
		total = len(slots)
		for _, slot := range slots {
			if slot.StatusID == account.StatusIDActive {
				active++
			}
			inflight += slot.Inflight
		}
	}
	admissionInflight := 0
	if s.Admission != nil {
		for _, n := range s.Admission.Snapshot() {
			admissionInflight += n
		}
	}
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = c.Writer.Write([]byte(s.metricsRegistry().RenderText([]metrics.Gauge{
		{Name: "grok2api_build_info", Help: "Build information.", Labels: map[string]string{"version": "1.0.0"}, Value: 1},
		{Name: "grok2api_accounts_total", Help: "Accounts currently loaded in memory.", Value: float64(total)},
		{Name: "grok2api_accounts_active", Help: "Active accounts currently selectable.", Value: float64(active)},
		{Name: "grok2api_account_inflight", Help: "Current in-flight upstream requests across accounts.", Value: float64(inflight)},
		{Name: "grok2api_admission_inflight", Help: "Current in-flight admitted requests.", Value: float64(admissionInflight)},
	})))
}

func (s *Server) handleReady(c *gin.Context) {
	total := 0
	active := 0
	inflight := 0
	if s.Directory != nil {
		slots := s.Directory.Snapshot()
		total = len(slots)
		for _, slot := range slots {
			if slot.StatusID == account.StatusIDActive {
				active++
			}
			inflight += slot.Inflight
		}
	}

	accountStatus := "ok"
	if active == 0 {
		accountStatus = "not_ready"
	}
	upstreamStatus := s.metricsRegistry().UpstreamHealth()
	status := "ready"
	code := http.StatusOK
	if accountStatus != "ok" {
		status = "not_ready"
		code = http.StatusServiceUnavailable
	} else if upstreamStatus == "degraded" {
		status = "degraded"
	}

	c.JSON(code, gin.H{
		"status": status,
		"checks": gin.H{
			"process": gin.H{"status": "ok"},
			"account_pool": gin.H{
				"status":   accountStatus,
				"total":    total,
				"active":   active,
				"inflight": inflight,
			},
			"upstream": gin.H{"status": upstreamStatus},
		},
	})
}

func (s *Server) metricsRegistry() *metrics.Registry {
	if s.Metrics == nil {
		s.Metrics = metrics.NewRegistry()
	}
	return s.Metrics
}

func metricStatusCode(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var appErr *platform.AppError
	if errors.As(err, &appErr) && appErr.Status > 0 {
		return appErr.Status
	}
	return http.StatusInternalServerError
}

func metricReason(status int) string {
	return strconv.Itoa(status)
}

// logMiddleware logs each request line at debug level.
func logMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}

// trimLeadingSlash removes exactly one leading "/".
func trimLeadingSlash(s string) string {
	if strings.HasPrefix(s, "/") {
		return s[1:]
	}
	return s
}

// marshalJSON is a helper for manual JSON marshalling in admin handlers.
func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
