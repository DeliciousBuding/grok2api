package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/DeliciousBuding/grok2api/internal/account"
	"github.com/DeliciousBuding/grok2api/internal/metrics"
	"github.com/DeliciousBuding/grok2api/internal/model"
	"github.com/DeliciousBuding/grok2api/internal/platform"
)

func preAcquireAdmission(t *testing.T, server *Server, scope string, limit int) []func() {
	t.Helper()
	releases := make([]func(), 0, limit)
	for i := 0; i < limit; i++ {
		release, ok := server.Admission.TryAcquire(scope, limit)
		if !ok {
			releaseAll(releases)
			t.Fatalf("pre-acquire %s slot %d/%d should pass", scope, i+1, limit)
		}
		releases = append(releases, release)
	}
	return releases
}

func releaseAll(releases []func()) {
	for i := len(releases) - 1; i >= 0; i-- {
		releases[i]()
	}
}

func TestRequestSizeMiddlewareRejectsOversizedBody(t *testing.T) {
	t.Setenv("GROK_APP_API_KEY", "")
	loadTestConfig(t, "[server]\nmax_body_bytes = 8\n")

	r := NewServer(nil, nil, nil, nil, nil).Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"too":"large"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRequestSizeMiddlewareReportsChunkedOversizedJSONAs413(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n[server]\nmax_body_bytes = 8\n")

	r := NewServer(&snapshotRepo{}, nil, nil, nil, nil).Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/tokens/add", io.NopCloser(strings.NewReader(`{"tokens":["tok-a"]}`)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "request_body_too_large") {
		t.Fatalf("expected request_body_too_large code, got %s", w.Body.String())
	}
}

func TestRequestSizeMiddlewareAppliesDefaultJSONLimit(t *testing.T) {
	loadTestConfig(t, "")

	r := NewServer(nil, nil, nil, nil, nil).Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("x", defaultNonMultipartMaxBodyBytes+1)))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "request_body_too_large") {
		t.Fatalf("expected request_body_too_large code, got %s", w.Body.String())
	}
}

func TestRequestSizeMiddlewareAppliesDefaultLimitWithoutContentType(t *testing.T) {
	loadTestConfig(t, "")

	r := NewServer(nil, nil, nil, nil, nil).Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("x", defaultNonMultipartMaxBodyBytes+1)))

	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRequestSizeMiddlewareDefaultJSONLimitDoesNotCapMultipart(t *testing.T) {
	loadTestConfig(t, "")

	largePrompt := strings.Repeat("x", defaultNonMultipartMaxBodyBytes+1)
	body, contentType := makeMultipartBody(t, map[string]string{
		"model":  "missing-model",
		"prompt": largePrompt,
	})
	r := NewServer(nil, nil, nil, nil, nil).Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	req.Header.Set("Content-Type", contentType)

	r.ServeHTTP(w, req)

	if w.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("default JSON limit should not cap multipart body: %s", w.Body.String())
	}
}

func TestAdminStorageEndpointReportsRepositoryBackend(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")

	server := NewServer(account.NewSQLiteRepository("accounts.sqlite3"), nil, nil, nil, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/storage", nil)
	req.Header.Set("Authorization", "Bearer admin")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"type":"sqlite"`) {
		t.Fatalf("expected sqlite storage type, got %s", w.Body.String())
	}
}

func TestValidatePatchRejectsNestedStartupOnlyKeys(t *testing.T) {
	err := validatePatch(map[string]any{
		"server": map[string]any{
			"max_header_bytes": 1048576,
		},
	})
	if err == nil {
		t.Fatal("expected nested startup-only config patch to be rejected")
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) || appErr.Code != "startup_only_config" || appErr.Param != "server.max_header_bytes" {
		t.Fatalf("expected startup_only_config for nested key, got %#v", err)
	}
}

func TestAdminConfigUpdateRejectsInvalidStatsigPairAsBadRequest(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)
	body := `{"proxy":{"clearance":{"statsig_seed":"abc","statsig_hex":"not-hex"}}}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_config") {
		t.Fatalf("expected invalid_config code, got %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "not-hex") || strings.Contains(w.Body.String(), "abc") {
		t.Fatalf("config validation error should not echo raw statsig values: %s", w.Body.String())
	}
}

func TestAdminConfigUpdateRejectsUnsafeClearanceHeaderConfig(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)
	body := `{"proxy":{"clearance":{"cf_cookies":"clearance_cookie=secret\r\nX-Injected: yes"}}}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_config") {
		t.Fatalf("expected invalid_config code, got %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "X-Injected") {
		t.Fatalf("config validation error should not echo raw clearance values: %s", w.Body.String())
	}
}

func TestMetricsEndpointDoesNotExposeTokens(t *testing.T) {
	loadTestConfig(t, "")

	r := NewServer(nil, nil, nil, nil, nil).Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sso") || strings.Contains(w.Body.String(), "tok-") {
		t.Fatalf("metrics should not expose token-like values: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "grok2api_build_info") {
		t.Fatalf("metrics should include build info, got %s", w.Body.String())
	}
}

func TestMetricsEndpointIncludesOperationalCounters(t *testing.T) {
	loadTestConfig(t, "")
	server := NewServer(nil, nil, nil, nil, nil)
	server.Metrics.IncAttempt("chat", "grok-4.20-fast")
	server.Metrics.IncRetry("chat", "grok-4.20-fast", "429")
	server.Metrics.IncUpstreamStatus("chat", "grok-4.20-fast", 429)
	server.Metrics.IncFeedback("rate_limited")
	server.Metrics.IncEmptyOutput("responses", "grok-4.20-fast")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	for _, needle := range []string{
		`grok2api_attempts_total{model="grok-4.20-fast",surface="chat"} 1`,
		`grok2api_retries_total{model="grok-4.20-fast",reason="429",surface="chat"} 1`,
		`grok2api_upstream_responses_total{model="grok-4.20-fast",status="429",surface="chat"} 1`,
		`grok2api_account_feedback_total{kind="rate_limited"} 1`,
		`grok2api_empty_outputs_total{model="grok-4.20-fast",surface="responses"} 1`,
	} {
		if !strings.Contains(w.Body.String(), needle) {
			t.Fatalf("expected metrics output to contain %q, got:\n%s", needle, w.Body.String())
		}
	}
}

func TestMetricsEndpointIncludesRequestDurationHistogram(t *testing.T) {
	loadTestConfig(t, "")
	server := NewServer(nil, nil, nil, nil, nil)
	router := server.Router()

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	needle := `grok2api_http_request_duration_seconds_count{method="GET",path="/health",status="200"} 1`
	if !strings.Contains(w.Body.String(), needle) {
		t.Fatalf("expected metrics output to contain %q, got:\n%s", needle, w.Body.String())
	}
}

func TestReadyEndpointReportsNotReadyWithoutAccountPool(t *testing.T) {
	loadTestConfig(t, "")
	server := NewServer(nil, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"process":{"status":"ok"`) {
		t.Fatalf("expected process check in readiness body, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"account_pool":{"active":0`) ||
		!strings.Contains(w.Body.String(), `"status":"not_ready"`) {
		t.Fatalf("expected account pool not_ready check, got %s", w.Body.String())
	}
}

func TestReadyEndpointReportsReadyWithActiveAccount(t *testing.T) {
	loadTestConfig(t, "")
	repo := &snapshotRepo{items: []*account.Record{account.NewRecord("tok-ready")}}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, dir, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"ready"`) {
		t.Fatalf("expected ready status, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"account_pool":{"active":1`) {
		t.Fatalf("expected active account count, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"upstream":{"status":"unknown"`) {
		t.Fatalf("expected upstream unknown before observations, got %s", w.Body.String())
	}
}

func TestGlobalAdmissionRejectsBeforeBodyParsing(t *testing.T) {
	loadTestConfig(t, "[admission]\nglobal_max_inflight = 1\n")
	server := NewServer(nil, nil, nil, nil, nil)
	release, ok := server.Admission.TryAcquire("global", 1)
	if !ok {
		t.Fatal("pre-acquire should pass")
	}
	defer release()

	r := server.Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{`))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admission_control_exhausted") {
		t.Fatalf("expected structured admission error, got %s", w.Body.String())
	}
}

func TestGlobalAdmissionClampsMisconfiguredLimit(t *testing.T) {
	loadTestConfig(t, fmt.Sprintf("[admission]\nglobal_max_inflight = %d\n", admissionMaxInflight+1))
	server := NewServer(nil, nil, nil, nil, nil)
	releases := preAcquireAdmission(t, server, "global", admissionMaxInflight)
	defer releaseAll(releases)

	r := server.Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{`))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 from clamped admission limit, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admission_control_exhausted") {
		t.Fatalf("expected structured admission error, got %s", w.Body.String())
	}
}

func TestModelAdmissionRejectsBeforeAccountSelection(t *testing.T) {
	loadTestConfig(t, "[admission]\nper_model_max_inflight = 1\n")
	server := NewServer(nil, nil, nil, nil, nil)
	release, ok := server.Admission.TryAcquire("model:grok-4.20-fast", 1)
	if !ok {
		t.Fatal("pre-acquire should pass")
	}
	defer release()

	r := server.Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.20-fast","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model:grok-4.20-fast") {
		t.Fatalf("expected model admission scope in response, got %s", w.Body.String())
	}
}

func TestModelAdmissionClampsMisconfiguredLimit(t *testing.T) {
	loadTestConfig(t, fmt.Sprintf("[admission]\nper_model_max_inflight = %d\n", admissionMaxInflight+1))
	server := NewServer(nil, nil, nil, nil, nil)
	releases := preAcquireAdmission(t, server, "model:grok-4.20-fast", admissionMaxInflight)
	defer releaseAll(releases)

	r := server.Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.20-fast","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 from clamped model admission limit, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model:grok-4.20-fast") {
		t.Fatalf("expected model admission scope in response, got %s", w.Body.String())
	}
}

func TestModelAdmissionReleasesOnValidationError(t *testing.T) {
	loadTestConfig(t, "[admission]\nper_model_max_inflight = 1\n")
	server := NewServer(nil, nil, nil, nil, nil)

	r := server.Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.20-fast","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected validation failure, got %d body=%s", w.Code, w.Body.String())
	}
	if got := server.Admission.Snapshot()["model:grok-4.20-fast"]; got != 0 {
		t.Fatalf("expected model admission to release on handler error, got %d", got)
	}
}

func TestModelAdmissionRejectsMediaEndpointsBeforeWork(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string
		model       string
		contentType string
		body        string
		multipart   map[string]string
	}{
		{
			name:        "image generations",
			method:      http.MethodPost,
			path:        "/v1/images/generations",
			model:       "grok-imagine-image",
			contentType: "application/json",
			body:        `{"model":"grok-imagine-image","prompt":"a city","n":1}`,
		},
		{
			name:      "image edits",
			method:    http.MethodPost,
			path:      "/v1/images/edits",
			model:     "grok-imagine-image-edit",
			multipart: map[string]string{"model": "grok-imagine-image-edit", "prompt": "edit it"},
		},
		{
			name:      "video create",
			method:    http.MethodPost,
			path:      "/v1/videos",
			model:     "grok-imagine-video",
			multipart: map[string]string{"model": "grok-imagine-video", "prompt": "a city"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loadTestConfig(t, "[admission]\nper_model_max_inflight = 1\n")
			server := NewServer(nil, nil, nil, nil, nil)
			scope := "model:" + tc.model
			release, ok := server.Admission.TryAcquire(scope, 1)
			if !ok {
				t.Fatal("pre-acquire should pass")
			}
			defer release()

			var req *http.Request
			if tc.multipart != nil {
				body, contentType := makeMultipartBody(t, tc.multipart)
				req = httptest.NewRequest(tc.method, tc.path, body)
				req.Header.Set("Content-Type", contentType)
			} else {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("expected 429, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), scope) {
				t.Fatalf("expected %s in response, got %s", scope, w.Body.String())
			}
		})
	}
}

func TestVideoAdmissionReleasesWhenBackgroundJobFails(t *testing.T) {
	loadTestConfig(t, "[admission]\nper_model_max_inflight = 1\n")
	server := NewServer(nil, nil, nil, nil, nil)

	body, contentType := makeMultipartBody(t, map[string]string{
		"model":  "grok-imagine-video",
		"prompt": "a city",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected queued video job, got %d body=%s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for {
		if got := server.Admission.Snapshot()["model:grok-imagine-video"]; got == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected video admission to release after failed background job, snapshot=%v", server.Admission.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestVideoJobSnapshotIsRaceSafeDuringFailureUpdates(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	job := &videoJob{
		ID:        "video_test",
		Object:    "video",
		CreatedAt: 1,
		Status:    "queued",
		Model:     "grok-imagine-video",
		Prompt:    "a city",
		Seconds:   6,
		Size:      "720x1280",
		Quality:   "standard",
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				server.failVideoJob(job, "upstream failed")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = job.toDict()
			}
		}()
	}
	wg.Wait()
}

func TestRegisterVideoJobPrunesOldJobs(t *testing.T) {
	resetVideoJobsForTest(t)

	for i := 0; i < maxVideoJobs+1; i++ {
		registerVideoJob(&videoJob{
			ID:        fmt.Sprintf("video_%04d", i),
			Object:    "video",
			CreatedAt: int64(i),
			Status:    "queued",
			Model:     "grok-imagine-video",
			Prompt:    "a city",
			Seconds:   6,
			Size:      "720x1280",
			Quality:   "standard",
		})
	}

	if lookupVideoJob("video_0000") != nil {
		t.Fatal("oldest video job should be pruned after registry exceeds the retention limit")
	}
	if lookupVideoJob("video_1024") == nil {
		t.Fatal("newest video job should be retained")
	}
	videoJobsMutex.Lock()
	size := len(videoJobsMap)
	videoJobsMutex.Unlock()
	if size != maxVideoJobs {
		t.Fatalf("expected video job registry size %d, got %d", maxVideoJobs, size)
	}
}

func TestCompleteVideoJobRecordsMetricsAndFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	job := &videoJob{
		ID:        "video_test",
		Object:    "video",
		CreatedAt: 1,
		Status:    "in_progress",
		Model:     "grok-imagine-video",
		Prompt:    "a city",
		Seconds:   6,
		Size:      "720x1280",
		Quality:   "standard",
	}

	server.completeVideoJob(job, "grok-imagine-video", &account.Lease{Token: "tok-a", ModeID: 1}, "https://assets.grok.com/video.mp4")

	snapshot := job.toDict()
	if snapshot["status"] != "completed" || snapshot["video_url"] != "https://assets.grok.com/video.mp4" {
		t.Fatalf("expected completed video job, got %#v", snapshot)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-video",status="200",surface="video"} 1`) {
		t.Fatalf("expected video 200 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="success"} 1`) {
		t.Fatalf("expected success feedback metric, got:\n%s", rendered)
	}
}

func TestFailVideoJobWithAccountFeedbackRecordsMetricsAndFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	job := &videoJob{
		ID:        "video_test",
		Object:    "video",
		CreatedAt: 1,
		Status:    "in_progress",
		Model:     "grok-imagine-video",
		Prompt:    "a city",
		Seconds:   6,
		Size:      "720x1280",
		Quality:   "standard",
	}

	server.failVideoJobWithAccountFeedback(job, "grok-imagine-video", &account.Lease{Token: "tok-a", ModeID: 1}, platform.UpstreamError("rate limited", http.StatusTooManyRequests, ""))

	snapshot := job.toDict()
	if snapshot["status"] != "failed" {
		t.Fatalf("expected failed video job, got %#v", snapshot)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-video",status="429",surface="video"} 1`) {
		t.Fatalf("expected video 429 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="rate_limited"} 1`) {
		t.Fatalf("expected rate_limited feedback metric, got:\n%s", rendered)
	}
}

func TestFailVideoJobWithAccountFeedbackClassifiesDeadline(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	job := &videoJob{
		ID:        "video_test",
		Object:    "video",
		CreatedAt: 1,
		Status:    "in_progress",
		Model:     "grok-imagine-video",
		Prompt:    "a city",
		Seconds:   6,
		Size:      "720x1280",
		Quality:   "standard",
	}

	server.failVideoJobWithAccountFeedback(job, "grok-imagine-video", &account.Lease{Token: "tok-a", ModeID: 1}, context.DeadlineExceeded)

	snapshot := job.toDict()
	if snapshot["status"] != "failed" {
		t.Fatalf("expected failed video job, got %#v", snapshot)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-video",status="504",surface="video"} 1`) {
		t.Fatalf("expected video 504 timeout metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="server_error"} 1`) {
		t.Fatalf("expected timeout to feed account health as server_error, got:\n%s", rendered)
	}
}

func TestAdminTokensListUsesBoundedPagination(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	repo := &snapshotRepo{listPage: &account.Page{
		Items:      []*account.Record{account.NewRecord("tok-a"), account.NewRecord("tok-b")},
		Total:      5,
		Page:       2,
		PageSize:   2,
		TotalPages: 3,
		Revision:   9,
	}}
	server := NewServer(repo, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/tokens?page=2&page_size=2", nil)
	req.Header.Set("Authorization", "Bearer admin")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if repo.lastListQuery.Page != 2 || repo.lastListQuery.PageSize != 2 {
		t.Fatalf("expected page query to reach repository, got %+v", repo.lastListQuery)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	pagination, ok := body["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("expected pagination metadata, got %s", w.Body.String())
	}
	if pagination["page"].(float64) != 2 || pagination["page_size"].(float64) != 2 ||
		pagination["total"].(float64) != 5 || pagination["total_pages"].(float64) != 3 ||
		pagination["has_more"].(bool) != true {
		t.Fatalf("unexpected pagination metadata: %#v", pagination)
	}
}

func TestAdminTokensListAppliesFilters(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	repo := &snapshotRepo{listPage: &account.Page{}}
	server := NewServer(repo, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/tokens?pool=super&status=disabled", nil)
	req.Header.Set("Authorization", "Bearer admin")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if repo.lastListQuery.Page != 1 || repo.lastListQuery.PageSize != adminDefaultPageSize {
		t.Fatalf("expected default pagination, got %+v", repo.lastListQuery)
	}
	if repo.lastListQuery.Pool != "super" {
		t.Fatalf("expected pool filter super, got %+v", repo.lastListQuery)
	}
	if repo.lastListQuery.Status == nil || *repo.lastListQuery.Status != account.StatusDisabled {
		t.Fatalf("expected disabled status filter, got %+v", repo.lastListQuery.Status)
	}
	if repo.lastListQuery.IncludeDeleted {
		t.Fatal("expected token list to exclude deleted accounts")
	}
}

func TestAdminTokensListRejectsOversizedPage(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/tokens?page_size=1001", nil)
	req.Header.Set("Authorization", "Bearer admin")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_page_size") {
		t.Fatalf("expected invalid_page_size error, got %s", w.Body.String())
	}
}

func TestAdminTokensListRejectsInvalidQueryValues(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name string
		path string
		code string
	}{
		{name: "page", path: "/admin/api/tokens?page=0", code: "invalid_page"},
		{name: "pool", path: "/admin/api/tokens?pool=unknown", code: "invalid_pool"},
		{name: "status", path: "/admin/api/tokens?status=deleted", code: "invalid_status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer admin")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestAdminTokensReplaceRejectsInvalidPool(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/tokens", strings.NewReader(`{"unknown":["tok-a"]}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_pool") {
		t.Fatalf("expected invalid_pool error, got %s", w.Body.String())
	}
}

func TestAdminTokensReplaceRejectsMalformedPoolPayload(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/tokens", strings.NewReader(`{"basic":{"token":"tok-a"}}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_pool_payload") {
		t.Fatalf("expected invalid_pool_payload error, got %s", w.Body.String())
	}
}

func TestAdminBatchRejectsInvalidQueryValues(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name string
		path string
		code string
	}{
		{name: "bad concurrency", path: "/admin/api/batch/nsfw?concurrency=zero", code: "invalid_concurrency"},
		{name: "low concurrency", path: "/admin/api/batch/nsfw?concurrency=0", code: "invalid_concurrency"},
		{name: "high concurrency", path: "/admin/api/batch/nsfw?concurrency=81", code: "invalid_concurrency"},
		{name: "enabled enum", path: "/admin/api/batch/nsfw?enabled=maybe", code: "invalid_enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"tokens":[]}`))
			req.Header.Set("Authorization", "Bearer admin")
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestAdminBatchRejectsTooManyTokensBeforeWork(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)
	body := batchTokensJSON(adminMaxBatchTokens + 1)

	for _, path := range []string{
		"/admin/api/batch/nsfw",
		"/admin/api/batch/refresh",
		"/admin/api/batch/cache-clear",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer admin")
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "too_many_tokens") {
				t.Fatalf("expected too_many_tokens error, got %s", w.Body.String())
			}
		})
	}
}

func TestAdminTokenMutationsRejectTooManyTokensBeforeWork(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)
	n := adminMaxBatchTokens + 1

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "add", method: http.MethodPost, path: "/admin/api/tokens/add", body: tokenMutationBodyJSON(n, "add")},
		{name: "replace", method: http.MethodPost, path: "/admin/api/tokens", body: tokenMutationBodyJSON(n, "replace")},
		{name: "delete", method: http.MethodDelete, path: "/admin/api/tokens", body: tokenMutationBodyJSON(n, "delete")},
		{name: "disable batch", method: http.MethodPost, path: "/admin/api/tokens/disabled/batch", body: tokenMutationBodyJSON(n, "disabled")},
		{name: "pool replace", method: http.MethodPut, path: "/admin/api/pool", body: tokenMutationBodyJSON(n, "pool")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer admin")
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "too_many_tokens") {
				t.Fatalf("expected too_many_tokens error, got %s", w.Body.String())
			}
		})
	}
}

func TestAdminTokenMutationsRejectOversizedTokensBeforeWork(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)
	const expectedAdminMaxTokenLength = 4096
	oversized := "tok_" + strings.Repeat("x", expectedAdminMaxTokenLength+1)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "add", method: http.MethodPost, path: "/admin/api/tokens/add", body: `{"tokens":["` + oversized + `"],"pool":"basic"}`},
		{name: "replace", method: http.MethodPost, path: "/admin/api/tokens", body: `{"basic":["` + oversized + `"]}`},
		{name: "delete", method: http.MethodDelete, path: "/admin/api/tokens", body: `["` + oversized + `"]`},
		{name: "disable batch", method: http.MethodPost, path: "/admin/api/tokens/disabled/batch", body: `{"tokens":["` + oversized + `"],"disabled":true}`},
		{name: "pool replace", method: http.MethodPut, path: "/admin/api/pool", body: `{"pool":"basic","tokens":["` + oversized + `"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer admin")
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "token_too_long") {
				t.Fatalf("expected token_too_long error, got %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), strings.Repeat("x", 32)) {
				t.Fatalf("token length validation should not echo raw token material: %s", w.Body.String())
			}
		})
	}
}

func TestAdminTokenMutationsRejectInvalidTagsBeforeWork(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		code   string
	}{
		{
			name:   "add too many tags",
			method: http.MethodPost,
			path:   "/admin/api/tokens/add",
			body:   `{"tokens":["tok-a"],"pool":"basic","tags":` + tagListJSON(adminMaxTags+1, 8) + `}`,
			code:   "too_many_tags",
		},
		{
			name:   "pool tag too long",
			method: http.MethodPut,
			path:   "/admin/api/pool",
			body:   `{"pool":"basic","tokens":["tok-a"],"tags":["` + strings.Repeat("x", adminMaxTagLength+1) + `"]}`,
			code:   "tag_too_long",
		},
		{
			name:   "replace embedded tag too long",
			method: http.MethodPost,
			path:   "/admin/api/tokens",
			body:   `{"basic":[{"token":"tok-a","tags":["` + strings.Repeat("x", adminMaxTagLength+1) + `"]}]}`,
			code:   "tag_too_long",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer admin")
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestChatCompletionsRejectInvalidPreferTagsBeforeRouting(t *testing.T) {
	loadTestConfig(t, "")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name string
		tags string
		code string
	}{
		{
			name: "too many tags",
			tags: tagListJSON(adminMaxTags+1, 8),
			code: "too_many_tags",
		},
		{
			name: "tag too long",
			tags: `["` + strings.Repeat("x", adminMaxTagLength+1) + `"]`,
			code: "tag_too_long",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"grok-4.20-fast","messages":[{"role":"user","content":"hello"}],"grok2api_prefer_tags":` + tc.tags + `}`
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestResponsesRejectInvalidPreferTagsBeforeRouting(t *testing.T) {
	loadTestConfig(t, "")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	body := `{"model":"grok-4.3-console","input":"hello","grok2api_prefer_tags":` + tagListJSON(adminMaxTags+1, 8) + `}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "too_many_tags") {
		t.Fatalf("expected too_many_tags error, got %s", w.Body.String())
	}
}

func TestRunAdminTokenWorkersBoundsActiveWork(t *testing.T) {
	tokens := []string{"tok-a", "tok-b", "tok-c", "tok-d", "tok-e"}
	started := make(chan struct{}, len(tokens))
	release := make(chan struct{})
	done := make(chan struct{})
	mu := sync.Mutex{}
	active := 0
	maxActive := 0
	processed := 0

	go func() {
		runAdminTokenWorkers(context.Background(), tokens, 2, func(ctx context.Context, token string) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			started <- struct{}{}
			<-release
			mu.Lock()
			active--
			processed++
			mu.Unlock()
		})
		close(done)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("started more work than configured concurrency before release")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workers did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if processed != len(tokens) {
		t.Fatalf("expected all tokens processed, got %d", processed)
	}
	if maxActive > 2 {
		t.Fatalf("expected max active work <= 2, got %d", maxActive)
	}
}

func TestStartAdminBackgroundTaskBoundsInflight(t *testing.T) {
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)
	server.adminBackground = make(chan struct{}, 1)

	started := make(chan struct{})
	release := make(chan struct{})
	if !server.tryStartAdminBackgroundTask(time.Second, func(ctx context.Context) {
		close(started)
		<-release
	}) {
		t.Fatal("expected first background task to start")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background task did not start")
	}

	if server.tryStartAdminBackgroundTask(time.Second, func(ctx context.Context) {
		t.Fatal("second background task should not start while capacity is exhausted")
	}) {
		t.Fatal("expected second background task to be rejected while capacity is exhausted")
	}

	close(release)
	deadline := time.After(time.Second)
	for {
		if server.tryStartAdminBackgroundTask(time.Second, func(ctx context.Context) {}) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("background task slot was not released")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestFeedbackBoundsAsyncQuotaRefreshTasks(t *testing.T) {
	ctx := context.Background()
	repo := account.NewTxtRepository(t.TempDir() + "/accounts.jsonl")
	if err := repo.Initialize(ctx); err != nil {
		t.Fatalf("initialize repo: %v", err)
	}
	upserts := []account.Upsert{
		{Token: "tok-a", Pool: "basic"},
		{Token: "tok-b", Pool: "basic"},
		{Token: "tok-c", Pool: "basic"},
	}
	if _, err := repo.UpsertAccounts(ctx, upserts); err != nil {
		t.Fatalf("upsert accounts: %v", err)
	}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap directory: %v", err)
	}
	fetcher := &apiBlockingQuotaFetcher{
		started: make(chan string, len(upserts)),
		release: make(chan struct{}),
	}
	server := NewServer(repo, dir, account.NewRefreshService(repo, fetcher), nil, nil)
	server.adminBackground = make(chan struct{}, 2)

	for _, upsert := range upserts {
		server.feedback(upsert.Token, account.FbSuccess, 1, nil, nil)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-fetcher.started:
		case <-time.After(time.Second):
			t.Fatal("expected bounded quota refresh task to start")
		}
	}
	select {
	case token := <-fetcher.started:
		t.Fatalf("quota refresh background gate should reject saturated task before release, but %s started", token)
	case <-time.After(50 * time.Millisecond):
	}
	close(fetcher.release)
	deadline := time.After(time.Second)
	for {
		if len(server.adminBackground) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("quota refresh background tasks did not finish")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestFeedbackErrorPersistsFailureWithDeadline(t *testing.T) {
	repo := &deadlineCheckingRepo{called: make(chan struct{})}
	server := NewServer(repo, account.NewDirectory(nil), account.NewRefreshService(repo, nil), nil, nil)

	server.feedbackError("tok-a", platform.NewAppError("unauthorized", platform.ErrUpstream, "unauthorized", http.StatusUnauthorized), 1)

	select {
	case <-repo.called:
	case <-time.After(time.Second):
		t.Fatal("expected feedbackError to persist unauthorized failure")
	}
	if !repo.hadDeadline {
		t.Fatal("expected feedbackError RecordFailure context to include a deadline")
	}
}

func TestFeedbackErrorSkipsLocalCancellation(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(err.Error(), func(t *testing.T) {
			ctx := context.Background()
			repo := account.NewTxtRepository(t.TempDir() + "/accounts.jsonl")
			if err := repo.Initialize(ctx); err != nil {
				t.Fatalf("initialize repo: %v", err)
			}
			if _, err := repo.UpsertAccounts(ctx, []account.Upsert{{Token: "tok-a", Pool: "basic"}}); err != nil {
				t.Fatalf("upsert accounts: %v", err)
			}
			dir := account.NewDirectory(repo)
			if err := dir.Bootstrap(ctx); err != nil {
				t.Fatalf("bootstrap directory: %v", err)
			}
			server := NewServer(repo, dir, account.NewRefreshService(repo, nil), nil, nil)

			before := *dir.Snapshot()[0]
			server.feedbackError("tok-a", err, 1)
			after := *dir.Snapshot()[0]

			if after.FailCount != before.FailCount {
				t.Fatalf("expected local cancellation not to increase fail count, before=%d after=%d", before.FailCount, after.FailCount)
			}
			if after.Health != before.Health {
				t.Fatalf("expected local cancellation not to change health, before=%v after=%v", before.Health, after.Health)
			}
			if after.LastFailAt != before.LastFailAt {
				t.Fatalf("expected local cancellation not to set last fail time, before=%d after=%d", before.LastFailAt, after.LastFailAt)
			}
		})
	}
}

func TestShouldRecordUpstreamStatusSkipsLocalCancellation(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if shouldRecordUpstreamStatus(err) {
			t.Fatalf("expected %v not to be recorded as an upstream response", err)
		}
	}
	if !shouldRecordUpstreamStatus(platform.UpstreamError("rate limited", http.StatusTooManyRequests, "")) {
		t.Fatal("expected upstream app errors to be recorded")
	}
	if !shouldRecordUpstreamStatus(errors.New("dial timeout")) {
		t.Fatal("expected non-cancellation transport errors to be recorded")
	}
}

func TestStreamResponseErrorPreservesAppErrorStatus(t *testing.T) {
	err := markStreamResponseError(platform.UpstreamError("rate limited", http.StatusTooManyRequests, ""))

	if !isStreamResponseError(err) {
		t.Fatalf("expected stream response error marker, got %T %[1]v", err)
	}
	if got := metricStatusCode(err); got != http.StatusTooManyRequests {
		t.Fatalf("expected wrapped stream error to preserve status 429, got %d", got)
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusTooManyRequests {
		t.Fatalf("expected errors.As to expose AppError 429, got %T %[1]v", err)
	}
}

type apiBlockingQuotaFetcher struct {
	started chan string
	release chan struct{}
}

func (f *apiBlockingQuotaFetcher) FetchAllQuotas(ctx context.Context, token, pool string, bootstrap bool) (map[int]account.ModeQuota, error) {
	f.started <- token
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[int]account.ModeQuota{
		1: {Remaining: 29, Total: 30, WindowSec: account.BasicFastWindowSec},
	}, nil
}

func (f *apiBlockingQuotaFetcher) FetchModeQuota(ctx context.Context, token, pool string, modeID int) (*account.ModeQuota, error) {
	return &account.ModeQuota{Remaining: 29, Total: 30, WindowSec: account.BasicFastWindowSec}, nil
}

type deadlineCheckingRepo struct {
	snapshotRepo
	called      chan struct{}
	hadDeadline bool
}

func (r *deadlineCheckingRepo) PatchAccounts(ctx context.Context, patches []account.Patch) (*account.MutationResult, error) {
	_, r.hadDeadline = ctx.Deadline()
	close(r.called)
	return &account.MutationResult{Revision: 1, Patched: len(patches)}, nil
}

func TestReadImageEditFileBytesRejectsOversizedInput(t *testing.T) {
	loadTestConfig(t, "[asset]\nmax_inline_image_bytes = 4\n")

	_, err := readImageEditFileBytes(strings.NewReader("12345"))
	if err == nil {
		t.Fatal("expected oversized image file to fail")
	}
	if !strings.Contains(err.Error(), "image file exceeds") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestReadImageEditFileBytesUsesDefaultLimitWhenUnconfigured(t *testing.T) {
	loadTestConfig(t, "[asset]\nmax_inline_image_bytes = 0\n")

	_, err := readImageEditFileBytes(strings.NewReader(strings.Repeat("x", defaultImageEditMaxFileBytes+1)))
	if err == nil {
		t.Fatal("expected default image edit file limit to reject oversized input")
	}
}

func TestFetchImageBase64RejectsNonSuccessStatus(t *testing.T) {
	loadTestConfig(t, "")
	withFetchImageTransport(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("not found")),
			Request:    req,
		}, nil
	})

	_, err := fetchImageBase64(context.Background(), "https://assets.grok.com/missing.png")
	if err == nil {
		t.Fatal("expected non-success image response to fail")
	}
	if !strings.Contains(err.Error(), "image fetch returned 404") {
		t.Fatalf("expected status error, got %v", err)
	}
}

func TestFetchImageBase64HonorsCanceledContext(t *testing.T) {
	loadTestConfig(t, "")
	var requests int32
	withFetchImageTransport(t, func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&requests, 1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("not reached")),
			Request:    req,
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetchImageBase64(ctx, "https://assets.grok.com/image.png")
	if err == nil {
		t.Fatal("expected canceled context to fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %T %[1]v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("expected canceled context to prevent request, got %d requests", got)
	}
}

func TestFetchImageBase64RejectsOversizedBody(t *testing.T) {
	loadTestConfig(t, "[asset]\nmax_fetch_image_bytes = 4\n")
	withFetchImageTransport(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(strings.NewReader("12345")),
			Request:    req,
		}, nil
	})

	_, err := fetchImageBase64(context.Background(), "https://assets.grok.com/image.png")
	if err == nil {
		t.Fatal("expected oversized fetched image to fail")
	}
	if !strings.Contains(err.Error(), "image fetch exceeds") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestFetchImageBase64UsesConfiguredTimeout(t *testing.T) {
	loadTestConfig(t, "[asset]\nfetch_image_timeout_sec = 1\n")
	withFetchImageTransport(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	start := time.Now()
	_, err := fetchImageBase64(context.Background(), "https://assets.grok.com/slow.png")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected configured image fetch timeout to fail")
	}
	if elapsed > 1700*time.Millisecond {
		t.Fatalf("expected configured timeout near 1s, elapsed %s", elapsed)
	}
}

func TestFetchImageHTTPClientSharesTransportWithCurrentTimeout(t *testing.T) {
	loadTestConfig(t, "[asset]\nfetch_image_timeout_sec = 1\n")
	first := fetchImageHTTPClient()

	loadTestConfig(t, "[asset]\nfetch_image_timeout_sec = 2\n")
	second := fetchImageHTTPClient()

	if first.Transport == nil || second.Transport == nil {
		t.Fatal("expected image fetch clients to use an explicit reusable transport")
	}
	if first.Transport != second.Transport {
		t.Fatal("expected image fetch clients to share the same transport for connection reuse")
	}
	if first.Timeout != time.Second {
		t.Fatalf("expected first timeout 1s, got %s", first.Timeout)
	}
	if second.Timeout != 2*time.Second {
		t.Fatalf("expected second timeout 2s, got %s", second.Timeout)
	}
}

func TestFetchImageTransportHasExplicitIdlePool(t *testing.T) {
	tr, ok := fetchImageTransport.(*http.Transport)
	if !ok {
		t.Fatalf("expected image fetch transport to be *http.Transport, got %T", fetchImageTransport)
	}
	if tr.MaxIdleConnsPerHost < defaultFetchImageMaxIdleConnsPerHost {
		t.Fatalf("expected MaxIdleConnsPerHost >= %d, got %d", defaultFetchImageMaxIdleConnsPerHost, tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Fatalf("expected MaxIdleConns %d to cover per-host idle pool %d", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
}

func TestFetchImageBase64BoundsConcurrentDownloads(t *testing.T) {
	loadTestConfig(t, "[asset]\nmax_fetch_image_concurrency = 1\n")

	var current int32
	var maxSeen int32
	entered := make(chan struct{}, 2)
	releaseFirst := make(chan struct{})
	withFetchImageTransport(t, func(req *http.Request) (*http.Response, error) {
		now := atomic.AddInt32(&current, 1)
		for {
			seen := atomic.LoadInt32(&maxSeen)
			if now <= seen || atomic.CompareAndSwapInt32(&maxSeen, seen, now) {
				break
			}
		}
		entered <- struct{}{}
		if now == 1 {
			<-releaseFirst
		}
		atomic.AddInt32(&current, -1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    req,
		}, nil
	})

	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := fetchImageBase64(context.Background(), "https://assets.grok.com/image.png")
			errCh <- err
		}()
	}

	<-entered
	select {
	case <-entered:
		close(releaseFirst)
		t.Fatal("second image fetch reached upstream before concurrency slot was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)

	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("fetch %d failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&maxSeen); got > 1 {
		t.Fatalf("expected at most 1 concurrent upstream fetch, saw %d", got)
	}
}

func TestDynamicConcurrencyLimiterReleaseIsIdempotent(t *testing.T) {
	limiter := newDynamicConcurrencyLimiter()
	ctx := context.Background()

	releaseA, err := limiter.acquire(ctx, 2)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	releaseB, err := limiter.acquire(ctx, 2)
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}
	defer releaseB()

	releaseA()
	releaseA()

	releaseC, err := limiter.acquire(ctx, 2)
	if err != nil {
		t.Fatalf("acquire C after one real release: %v", err)
	}
	defer releaseC()

	blockedCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	releaseD, err := limiter.acquire(blockedCtx, 2)
	if err == nil {
		releaseD()
		t.Fatal("double release should not create a phantom concurrency slot")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline while waiting for real capacity, got %T %[1]v", err)
	}
}

func TestRenderGeneratedImagesRejectsEmptyOutput(t *testing.T) {
	_, err := renderGeneratedImages(context.Background(), "url", nil)
	if err == nil {
		t.Fatal("expected empty generated image output to fail")
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T %[1]v", err)
	}
	if appErr.Status != http.StatusBadGateway || appErr.Code != "upstream_error" {
		t.Fatalf("expected 502 upstream_error, got status=%d code=%s", appErr.Status, appErr.Code)
	}
}

func TestRenderGeneratedImagesRejectsURLResponseWithoutURL(t *testing.T) {
	_, err := renderGeneratedImages(context.Background(), "url", []generatedImage{{blob: "abc123"}})
	if err == nil {
		t.Fatal("expected url response without a URL to fail")
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T %[1]v", err)
	}
	if appErr.Status != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", appErr.Status)
	}
}

func TestBuildWSImageChatContentRejectsEmptyOutput(t *testing.T) {
	_, err := buildWSImageChatContent(nil)
	if err == nil {
		t.Fatal("expected empty WS image chat output to fail")
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T %[1]v", err)
	}
	if appErr.Status != http.StatusBadGateway || appErr.Code != "upstream_error" {
		t.Fatalf("expected 502 upstream_error, got status=%d code=%s", appErr.Status, appErr.Code)
	}
}

func TestBuildWSImageStreamMarkdownRejectsMissingURL(t *testing.T) {
	if md, ok := buildWSImageStreamMarkdown(""); ok || md != "" {
		t.Fatalf("expected missing stream image URL to be rejected, got ok=%v md=%q", ok, md)
	}
}

func TestRequestWithTimeoutClassUsesConfiguredDeadline(t *testing.T) {
	loadTestConfig(t, "[timeout]\nimage_sec = 7\n")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	timedReq, cancel := requestWithTimeoutClass(req, "image", 300)
	defer cancel()

	deadline, ok := timedReq.Context().Deadline()
	if !ok {
		t.Fatal("expected timeout class to set a request deadline")
	}
	remaining := time.Until(deadline)
	if remaining < 6*time.Second || remaining > 8*time.Second {
		t.Fatalf("expected image timeout near 7s, got %s", remaining)
	}
}

func TestWSImageRequestContextErrorClassifiesDeadline(t *testing.T) {
	err := wsImageRequestContextError(context.DeadlineExceeded)
	var appErr *platform.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T %[1]v", err)
	}
	if appErr.Status != http.StatusGatewayTimeout || appErr.Code != "image_generation_timeout" {
		t.Fatalf("expected image generation 504 timeout, got status=%d code=%s", appErr.Status, appErr.Code)
	}
}

func TestWSImageRequestContextErrorIgnoresCancellation(t *testing.T) {
	if err := wsImageRequestContextError(context.Canceled); err != nil {
		t.Fatalf("expected client cancellation to stay silent, got %T %[1]v", err)
	}
}

func TestWriteWSImageStreamFailureRecordsMetricsAndFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec)

	server.writeWSImageStreamFailure(sw, "grok-imagine-image", &account.Lease{Token: "tok-a", ModeID: 1}, platform.UpstreamError("upstream failed", http.StatusBadGateway, ""))

	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected SSE error frame and done marker, got %q", body)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-image",status="502",surface="image_ws"} 1`) {
		t.Fatalf("expected image_ws 502 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="server_error"} 1`) {
		t.Fatalf("expected server_error feedback metric, got:\n%s", rendered)
	}
}

func TestWriteWSImageGenerationFailureRecordsMetricsAndFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	server.writeWSImageGenerationFailure(c, "grok-imagine-image", &account.Lease{Token: "tok-a", ModeID: 1}, platform.UpstreamError("upstream failed", http.StatusBadGateway, ""))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 response, got %d body=%s", rec.Code, rec.Body.String())
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-image",status="502",surface="image_ws"} 1`) {
		t.Fatalf("expected image_ws 502 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="server_error"} 1`) {
		t.Fatalf("expected server_error feedback metric, got:\n%s", rendered)
	}
}

func TestWriteWSImageGenerationEmptyOutputRecordsMetricsAndFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	server.writeWSImageGenerationEmptyOutput(c, "grok-imagine-image", &account.Lease{Token: "tok-a", ModeID: 1})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 response, got %d body=%s", rec.Code, rec.Body.String())
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_empty_outputs_total{model="grok-imagine-image",surface="image_ws"} 1`) {
		t.Fatalf("expected image_ws empty-output metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="server_error"} 1`) {
		t.Fatalf("expected server_error feedback metric, got:\n%s", rendered)
	}
}

func TestWriteWSImageGenerationSuccessRecordsMetricsAndFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)

	server.writeWSImageGenerationSuccess("grok-imagine-image", &account.Lease{Token: "tok-a", ModeID: 1})

	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-image",status="200",surface="image_ws"} 1`) {
		t.Fatalf("expected image_ws 200 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="success"} 1`) {
		t.Fatalf("expected success feedback metric, got:\n%s", rendered)
	}
}

func TestWriteWSImageGenerationPostProcessFailureRecordsClientFailureAndAccountSuccess(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	server.writeWSImageGenerationPostProcessFailure(c, "grok-imagine-image", &account.Lease{Token: "tok-a", ModeID: 1}, platform.UpstreamError("image fetch failed: upstream 404", http.StatusBadGateway, ""))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 response, got %d body=%s", rec.Code, rec.Body.String())
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-image",status="502",surface="image_ws"} 1`) {
		t.Fatalf("expected image_ws 502 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="success"} 1`) {
		t.Fatalf("expected account success feedback, got:\n%s", rendered)
	}
	if strings.Contains(rendered, `grok2api_account_feedback_total{kind="server_error"}`) {
		t.Fatalf("post-processing failure must not poison the account pool, got:\n%s", rendered)
	}
}

func TestFinishCapturedImageURLsRecordsSuccessFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	body := mustMarshalChatResponse(t, "grok-imagine-image-lite", "![image](https://assets.grok.com/generated.png)")

	urls := server.finishCapturedImageURLs("grok-imagine-image-lite", &account.Lease{Token: "tok-a", ModeID: 1}, body, nil)

	if len(urls) != 1 || urls[0] != "https://assets.grok.com/generated.png" {
		t.Fatalf("expected generated image URL, got %v", urls)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-image-lite",status="200",surface="image"} 1`) {
		t.Fatalf("expected image 200 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="success"} 1`) {
		t.Fatalf("expected success feedback metric, got:\n%s", rendered)
	}
	if strings.Contains(rendered, `grok2api_empty_outputs_total{model="grok-imagine-image-lite",surface="image"}`) {
		t.Fatalf("successful capture should not record empty output, got:\n%s", rendered)
	}
}

func TestFinishCapturedImageURLsRecordsEmptyOutputFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	body := mustMarshalChatResponse(t, "grok-imagine-image-lite", "no usable image")

	urls := server.finishCapturedImageURLs("grok-imagine-image-lite", &account.Lease{Token: "tok-a", ModeID: 1}, body, nil)

	if len(urls) != 0 {
		t.Fatalf("expected no generated image URLs, got %v", urls)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_empty_outputs_total{model="grok-imagine-image-lite",surface="image"} 1`) {
		t.Fatalf("expected image empty-output metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-image-lite",status="502",surface="image"} 1`) {
		t.Fatalf("expected image 502 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="server_error"} 1`) {
		t.Fatalf("expected server_error feedback metric, got:\n%s", rendered)
	}
}

func TestFinishCapturedImageURLsRecordsUpstreamFailureFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)

	urls := server.finishCapturedImageURLs("grok-imagine-image-lite", &account.Lease{Token: "tok-a", ModeID: 1}, nil, platform.UpstreamError("rate limited", http.StatusTooManyRequests, ""))

	if len(urls) != 0 {
		t.Fatalf("expected no generated image URLs, got %v", urls)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-imagine-image-lite",status="429",surface="image"} 1`) {
		t.Fatalf("expected image 429 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="rate_limited"} 1`) {
		t.Fatalf("expected rate_limited feedback metric, got:\n%s", rendered)
	}
}

func TestFinishCapturedChatTextRecordsSuccessFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	body := mustMarshalChatResponse(t, "grok-4.20-fast", "hello")

	text := server.finishCapturedChatText("responses", "grok-4.20-fast", &account.Lease{Token: "tok-a", ModeID: 1}, body, nil)

	if text != "hello" {
		t.Fatalf("expected captured text, got %q", text)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-4.20-fast",status="200",surface="responses"} 1`) {
		t.Fatalf("expected responses 200 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="success"} 1`) {
		t.Fatalf("expected success feedback metric, got:\n%s", rendered)
	}
}

func TestCaptureChatTextRecordsAttemptBeforeAccountSelection(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	spec, ok := model.Resolve("grok-4.20-fast")
	if !ok {
		t.Fatal("resolve chat test model")
	}
	req := &chatCompletionRequest{
		Model:    "grok-4.20-fast",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	text := server.captureChatText(httpReq, req, spec, "responses")

	if text != "" {
		t.Fatalf("expected no captured text without account pool, got %q", text)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_attempts_total{model="grok-4.20-fast",surface="responses"} 1`) {
		t.Fatalf("expected responses attempt metric, got:\n%s", rendered)
	}
}

func TestFinishCapturedChatTextRecordsEmptyOutputFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)
	body := mustMarshalChatResponse(t, "grok-4.20-fast", "")

	text := server.finishCapturedChatText("responses", "grok-4.20-fast", &account.Lease{Token: "tok-a", ModeID: 1}, body, nil)

	if text != "" {
		t.Fatalf("expected no captured text, got %q", text)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_empty_outputs_total{model="grok-4.20-fast",surface="responses"} 1`) {
		t.Fatalf("expected responses empty-output metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-4.20-fast",status="502",surface="responses"} 1`) {
		t.Fatalf("expected responses 502 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="server_error"} 1`) {
		t.Fatalf("expected server_error feedback metric, got:\n%s", rendered)
	}
}

func TestFinishCapturedChatTextRecordsUpstreamFailureFeedback(t *testing.T) {
	server := NewServer(nil, nil, nil, nil, nil)

	text := server.finishCapturedChatText("responses", "grok-4.20-fast", &account.Lease{Token: "tok-a", ModeID: 1}, nil, platform.UpstreamError("rate limited", http.StatusTooManyRequests, ""))

	if text != "" {
		t.Fatalf("expected no captured text, got %q", text)
	}
	rendered := server.metricsRegistry().RenderText(nil)
	if !strings.Contains(rendered, `grok2api_upstream_responses_total{model="grok-4.20-fast",status="429",surface="responses"} 1`) {
		t.Fatalf("expected responses 429 metric, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `grok2api_account_feedback_total{kind="rate_limited"} 1`) {
		t.Fatalf("expected rate_limited feedback metric, got:\n%s", rendered)
	}
}

func TestRenderGeneratedImagesReturnsUpstreamErrorForB64FetchFailure(t *testing.T) {
	loadTestConfig(t, "")
	oldTransport := fetchImageTransport
	fetchImageTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("not found")),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { fetchImageTransport = oldTransport })

	_, err := renderGeneratedImages(context.Background(), "b64_json", []generatedImage{
		{url: "https://assets.grok.com/missing.png"},
	})
	if err == nil {
		t.Fatal("expected b64_json image fetch failure to return an error")
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T %[1]v", err)
	}
	if appErr.Status != http.StatusBadGateway || appErr.Code != "upstream_error" {
		t.Fatalf("expected 502 upstream_error, got status=%d code=%s", appErr.Status, appErr.Code)
	}
}

func TestRenderGeneratedImagesRecordsB64FetchMetrics(t *testing.T) {
	loadTestConfig(t, "[asset]\nmax_fetch_image_bytes = 4\n")
	reg := metrics.NewRegistry()
	oldTransport := fetchImageTransport
	fetchImageTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := ""
		status := http.StatusOK
		switch req.URL.Path {
		case "/ok.png":
			body = "ok"
		case "/large.png":
			body = "12345"
		default:
			status = http.StatusNotFound
			body = "not found"
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { fetchImageTransport = oldTransport })

	if _, err := renderGeneratedImages(context.Background(), "b64_json", []generatedImage{
		{url: "https://assets.grok.com/ok.png"},
	}, reg); err != nil {
		t.Fatalf("expected successful fetch: %v", err)
	}
	if _, err := renderGeneratedImages(context.Background(), "b64_json", []generatedImage{
		{url: "https://assets.grok.com/missing.png"},
	}, reg); err == nil {
		t.Fatal("expected status failure")
	}
	if _, err := renderGeneratedImages(context.Background(), "b64_json", []generatedImage{
		{url: "https://assets.grok.com/large.png"},
	}, reg); err == nil {
		t.Fatal("expected oversize failure")
	}

	out := reg.RenderText(nil)
	for _, needle := range []string{
		`grok2api_asset_fetch_total{kind="success"} 1`,
		`grok2api_asset_fetch_total{kind="status"} 1`,
		`grok2api_asset_fetch_total{kind="too_large"} 1`,
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("expected metrics output to contain %q, got:\n%s", needle, out)
		}
	}
	if strings.Contains(out, "assets.grok.com") {
		t.Fatalf("asset fetch metrics must not expose source URLs: %s", out)
	}
}

func TestValidateFetchImageURLRejectsUnsafeDestinations(t *testing.T) {
	cases := []string{
		"/relative.png",
		"file:///tmp/image.png",
		"http://localhost/image.png",
		"http://127.0.0.1/image.png",
		"http://10.0.0.5/image.png",
		"http://100.64.0.5/image.png",
		"http://198.51.100.5/image.png",
		"http://[::1]/image.png",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if err := validateFetchImageURL(raw); err == nil {
				t.Fatalf("expected unsafe image URL %q to be rejected", raw)
			}
		})
	}

	if err := validateFetchImageURL("https://assets.grok.com/generated.png"); err != nil {
		t.Fatalf("expected public Grok asset URL to be accepted: %v", err)
	}
}

func TestFetchImageDialContextDialsResolvedPublicAddress(t *testing.T) {
	resolver := func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host != "assets.example" {
			t.Fatalf("unexpected resolver host %q", host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	var dialAddress string
	dial := fetchImageDialContext(resolver, func(ctx context.Context, network, address string) (net.Conn, error) {
		dialAddress = address
		return nil, nil
	})

	if _, err := dial(context.Background(), "tcp", "assets.example:443"); err != nil {
		t.Fatalf("expected public DNS result to dial: %v", err)
	}
	if dialAddress != "93.184.216.34:443" {
		t.Fatalf("expected dial to use resolved IP literal, got %q", dialAddress)
	}
}

func TestFetchImageDialContextBlocksDNSResolvedPrivateAddress(t *testing.T) {
	resolver := func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host != "attacker.example" {
			t.Fatalf("unexpected resolver host %q", host)
		}
		return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
	}
	dialed := false
	dial := fetchImageDialContext(resolver, func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("dial should not run")
	})

	_, err := dial(context.Background(), "tcp", "attacker.example:443")
	if err == nil {
		t.Fatal("expected DNS-resolved private address to be blocked")
	}
	if got := imageFetchMetricKind(err); got != "blocked" {
		t.Fatalf("expected blocked metric kind, got %q from %v", got, err)
	}
	if dialed {
		t.Fatal("unsafe DNS result must be rejected before dialing")
	}
}

func TestFetchImageBase64BlocksUnsafeRedirect(t *testing.T) {
	oldTransport := fetchImageTransport
	fetchImageTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "assets.grok.com" {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://127.0.0.1/private.png"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		t.Fatalf("unsafe redirect target should be rejected before RoundTrip, got %s", req.URL.String())
		return nil, nil
	})
	t.Cleanup(func() { fetchImageTransport = oldTransport })

	_, err := fetchImageBase64(context.Background(), "https://assets.grok.com/generated.png")
	if err == nil {
		t.Fatal("expected unsafe redirect to be rejected")
	}
	if got := imageFetchMetricKind(err); got != "blocked" {
		t.Fatalf("expected blocked metric kind, got %q from %v", got, err)
	}
}

func mustMarshalChatResponse(t *testing.T, modelName, content string) []byte {
	t.Helper()
	b, err := json.Marshal(makeChatResponse("chatcmpl-test", 1, modelName, content, "", false))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCaptureLiteImageBatchHonorsCanceledContextBeforeFanout(t *testing.T) {
	loadTestConfig(t, "")
	spec, ok := model.Resolve("grok-imagine-image-lite")
	if !ok {
		t.Fatal("resolve image test model")
	}
	repo := &snapshotRepo{items: []*account.Record{account.NewRecord("tok-ready")}}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := NewServer(repo, dir, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(ctx)

	got := server.captureLiteImageBatch(req, spec, "Drawing: canceled", 4)
	if len(got) != 0 {
		t.Fatalf("expected canceled batch to return no image URLs, got %v", got)
	}
}

func TestAdminBatchCacheClearRejectsEmptyTokensBeforeRefreshCheck(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/batch/cache-clear", strings.NewReader(`{"tokens":[]}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_value") {
		t.Fatalf("expected token validation error, got %s", w.Body.String())
	}
}

func TestClearAssetIDsStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := []string{}

	deleted := clearAssetIDs(ctx, []string{"asset-a", "asset-b", "asset-c"}, func(ctx context.Context, assetID string) error {
		called = append(called, assetID)
		cancel()
		return nil
	})

	if deleted != 1 {
		t.Fatalf("expected only first asset to be counted as deleted, got %d", deleted)
	}
	if len(called) != 1 || called[0] != "asset-a" {
		t.Fatalf("expected cancellation to stop remaining asset deletes, called=%v", called)
	}
}

func TestAdminCacheListRejectsInvalidQueryValues(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name string
		path string
		code string
	}{
		{name: "type", path: "/admin/api/cache/list?type=audio", code: "invalid_cache_type"},
		{name: "page", path: "/admin/api/cache/list?page=0", code: "invalid_page"},
		{name: "page size", path: "/admin/api/cache/list?page_size=1001", code: "invalid_page_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer admin")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestAdminCacheMutationsRejectInvalidTypeAndJSON(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name   string
		path   string
		body   string
		code   string
		status int
	}{
		{name: "clear bad json", path: "/admin/api/cache/clear", body: `{`, code: "invalid_value", status: http.StatusBadRequest},
		{name: "clear invalid type", path: "/admin/api/cache/clear", body: `{"type":"audio"}`, code: "invalid_cache_type", status: http.StatusBadRequest},
		{name: "item invalid type", path: "/admin/api/cache/item/delete", body: `{"type":"audio","name":"a.jpg"}`, code: "invalid_cache_type", status: http.StatusBadRequest},
		{name: "items invalid type", path: "/admin/api/cache/items/delete", body: `{"type":"audio","names":["a.jpg"]}`, code: "invalid_cache_type", status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer admin")
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != tc.status {
				t.Fatalf("expected %d, got %d body=%s", tc.status, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestAdminCacheItemsDeleteRejectsTooManyNames(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/cache/items/delete", strings.NewReader(cacheNamesJSON(adminMaxCacheItemNames+1)))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "too_many_file_names") {
		t.Fatalf("expected too_many_file_names error, got %s", w.Body.String())
	}
}

func TestAdminAssetsListUsesBoundedPaginationAndFilters(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	repo := &snapshotRepo{listPage: &account.Page{
		Total:      9,
		Page:       2,
		PageSize:   3,
		TotalPages: 3,
		Revision:   11,
	}}
	server := NewServer(repo, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/assets?page=2&page_size=3&pool=heavy&status=disabled&concurrency=5", nil)
	req.Header.Set("Authorization", "Bearer admin")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if repo.lastListQuery.Page != 2 || repo.lastListQuery.PageSize != 3 {
		t.Fatalf("expected bounded pagination query, got %+v", repo.lastListQuery)
	}
	if repo.lastListQuery.Pool != "heavy" {
		t.Fatalf("expected heavy pool filter, got %+v", repo.lastListQuery)
	}
	if repo.lastListQuery.Status == nil || *repo.lastListQuery.Status != account.StatusDisabled {
		t.Fatalf("expected disabled status filter, got %+v", repo.lastListQuery.Status)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	pagination, ok := body["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("expected pagination metadata, got %s", w.Body.String())
	}
	if pagination["page"].(float64) != 2 || pagination["page_size"].(float64) != 3 ||
		pagination["total"].(float64) != 9 || pagination["total_pages"].(float64) != 3 ||
		pagination["has_more"].(bool) != true {
		t.Fatalf("unexpected pagination metadata: %#v", pagination)
	}
}

func TestCollectAssetRowsBoundsActiveListCalls(t *testing.T) {
	tokens := []string{"tok-a", "tok-b", "tok-c", "tok-d", "tok-e"}
	started := make(chan struct{}, len(tokens))
	release := make(chan struct{})
	mu := sync.Mutex{}
	active := 0
	maxActive := 0

	done := make(chan struct{})
	var rows []map[string]any
	var total int
	go func() {
		rows, total = collectAssetRows(context.Background(), tokens, 2, func(ctx context.Context, token string) (map[string]any, error) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			started <- struct{}{}
			<-release
			mu.Lock()
			active--
			mu.Unlock()
			return map[string]any{"assets": []any{map[string]any{"id": token + "-asset"}}}, nil
		})
		close(done)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("asset workers did not start")
		}
	}
	select {
	case <-started:
		close(release)
		t.Fatal("started more asset list calls than configured concurrency before release")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("asset workers did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive > 2 {
		t.Fatalf("expected max active asset list calls <= 2, got %d", maxActive)
	}
	if len(rows) != len(tokens) {
		t.Fatalf("expected one row per token, got %d", len(rows))
	}
	if total != len(tokens) {
		t.Fatalf("expected total asset count %d, got %d", len(tokens), total)
	}
}

func TestCollectAssetRowsSkipsQueuedWorkAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := 0

	rows, total := collectAssetRows(ctx, []string{"tok-a", "tok-b", "tok-c"}, 2, func(ctx context.Context, token string) (map[string]any, error) {
		called++
		return map[string]any{"assets": []any{map[string]any{"id": token + "-asset"}}}, nil
	})

	if called != 0 {
		t.Fatalf("canceled asset list should not call upstream list, called=%d", called)
	}
	if len(rows) != 0 || total != 0 {
		t.Fatalf("canceled asset list should not synthesize queued rows, rows=%d total=%d", len(rows), total)
	}
}

func TestAdminAssetsListRejectsInvalidQueryValues(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name string
		path string
		code string
	}{
		{name: "page", path: "/admin/api/assets?page=0", code: "invalid_page"},
		{name: "page size", path: "/admin/api/assets?page_size=1001", code: "invalid_page_size"},
		{name: "pool", path: "/admin/api/assets?pool=unknown", code: "invalid_pool"},
		{name: "status", path: "/admin/api/assets?status=deleted", code: "invalid_status"},
		{name: "bad concurrency", path: "/admin/api/assets?concurrency=bad", code: "invalid_concurrency"},
		{name: "high concurrency", path: "/admin/api/assets?concurrency=81", code: "invalid_concurrency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer admin")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestAdminAssetsDeleteItemRejectsMissingFields(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name string
		body string
		code string
	}{
		{name: "token", body: `{"asset_id":"asset-a"}`, code: "missing_token"},
		{name: "asset", body: `{"token":"tok-a"}`, code: "missing_asset_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/admin/api/assets/delete-item", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer admin")
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestAdminAssetsClearTokenRejectsMissingTokenAndRequiresConfirmation(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)

	cases := []struct {
		name string
		body string
		code string
	}{
		{name: "missing token", body: `{}`, code: "missing_token"},
		{name: "missing confirmation", body: `{"token":"tok-a"}`, code: "confirmation_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/admin/api/assets/clear-token", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer admin")
			req.Header.Set("Content-Type", "application/json")

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s error, got %s", tc.code, w.Body.String())
			}
		})
	}
}

func TestAdminAuditTokensDeleteUsesHashedTokenIdentifiers(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	rawToken := "sso-super-secret-token-12345678901234567890"
	repo := &snapshotRepo{}
	server := NewServer(repo, nil, nil, nil, nil)
	events := []AdminAuditEvent{}
	server.AdminAudit = AdminAuditFunc(func(event AdminAuditEvent) {
		events = append(events, event)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/tokens", strings.NewReader(`["`+rawToken+`"]`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("expected one audit event, got %#v", events)
	}
	event := events[0]
	if event.Operation != "tokens.delete" || event.Outcome != "success" || event.Method != http.MethodDelete || event.Path != "/admin/api/tokens" {
		t.Fatalf("unexpected audit event identity: %#v", event)
	}
	if event.TokenCount != 1 || len(event.TokenIDs) != 1 || event.TokenIDs[0] == "" {
		t.Fatalf("expected one hashed token identifier, got %#v", event)
	}
	if event.Deleted != 1 {
		t.Fatalf("expected deleted count in audit event, got %#v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), rawToken) || strings.Contains(string(encoded), "super-secret") || strings.Contains(string(encoded), "sso-") {
		t.Fatalf("audit event leaked raw token data: %s", encoded)
	}
	if event.TokenIDs[0] == rawToken || strings.Contains(event.TokenIDs[0], "...") {
		t.Fatalf("token identifier should be a non-reversible hash, got %q", event.TokenIDs[0])
	}
}

func TestAdminAuditPoolReplaceOmitsRawTokenPayload(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	rawA := "sso-pool-secret-token-a-12345678901234567890"
	rawB := "sso-pool-secret-token-b-12345678901234567890"
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)
	events := []AdminAuditEvent{}
	server.AdminAudit = AdminAuditFunc(func(event AdminAuditEvent) {
		events = append(events, event)
	})

	body := `{"pool":"heavy","tokens":["` + rawA + `","` + rawB + `"],"tags":["private-tag"]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/api/pool", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("expected one audit event, got %#v", events)
	}
	event := events[0]
	if event.Operation != "pool.replace" || event.Pool != "heavy" || event.TokenCount != 2 || event.Upserted != 2 {
		t.Fatalf("unexpected pool replace audit event: %#v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{rawA, rawB, "pool-secret", "sso-", "private-tag"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("audit event leaked forbidden payload %q: %s", forbidden, encoded)
		}
	}
}

func TestAdminAuditPoolReplaceBoundsTokenIdentifiers(t *testing.T) {
	loadTestConfig(t, "[app]\napp_key = \"admin\"\n")
	server := NewServer(&snapshotRepo{}, nil, nil, nil, nil)
	events := []AdminAuditEvent{}
	server.AdminAudit = AdminAuditFunc(func(event AdminAuditEvent) {
		events = append(events, event)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/api/pool", strings.NewReader(tokenMutationBodyJSON(adminMaxBatchTokens, "pool")))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("expected one audit event, got %#v", events)
	}
	event := events[0]
	if event.TokenCount != adminMaxBatchTokens {
		t.Fatalf("audit token_count should keep the full mutation size, got %d", event.TokenCount)
	}
	if len(event.TokenIDs) > adminAuditMaxTokenIDs {
		t.Fatalf("audit token_ids should be bounded, got %d identifiers", len(event.TokenIDs))
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 2048 {
		t.Fatalf("audit event should stay compact, got %d bytes", len(encoded))
	}
}

func makeMultipartBody(t *testing.T, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("write multipart field: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, writer.FormDataContentType()
}

func batchTokensJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"tokens":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"tok-%04d"`, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

func tokenMutationBodyJSON(n int, shape string) string {
	var tokens strings.Builder
	tokens.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			tokens.WriteByte(',')
		}
		fmt.Fprintf(&tokens, `"tok-%04d"`, i)
	}
	tokens.WriteByte(']')
	switch shape {
	case "add":
		return `{"tokens":` + tokens.String() + `,"pool":"basic"}`
	case "replace":
		return `{"basic":` + tokens.String() + `}`
	case "delete":
		return tokens.String()
	case "disabled":
		return `{"tokens":` + tokens.String() + `,"disabled":true}`
	case "pool":
		return `{"pool":"basic","tokens":` + tokens.String() + `}`
	default:
		return `{}`
	}
}

func tagListJSON(n int, width int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"tag-%0*d"`, width, i)
	}
	b.WriteByte(']')
	return b.String()
}

func cacheNamesJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"names":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"cache-%04d.png"`, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withFetchImageTransport(t *testing.T, fn func(*http.Request) (*http.Response, error)) {
	t.Helper()
	oldTransport := fetchImageTransport
	fetchImageTransport = roundTripFunc(fn)
	t.Cleanup(func() { fetchImageTransport = oldTransport })
}

type snapshotRepo struct {
	items         []*account.Record
	listPage      *account.Page
	lastListQuery account.ListQuery
}

func (r *snapshotRepo) Initialize(ctx context.Context) error { return nil }
func (r *snapshotRepo) GetRevision(ctx context.Context) (int, error) {
	return 1, nil
}
func (r *snapshotRepo) RuntimeSnapshot(ctx context.Context) (*account.Snapshot, error) {
	return &account.Snapshot{Revision: 1, Items: r.items}, nil
}
func (r *snapshotRepo) ScanChanges(ctx context.Context, since int, limit int) (*account.ChangeSet, error) {
	return &account.ChangeSet{Revision: 1}, nil
}
func (r *snapshotRepo) UpsertAccounts(ctx context.Context, items []account.Upsert) (*account.MutationResult, error) {
	return &account.MutationResult{}, nil
}
func (r *snapshotRepo) PatchAccounts(ctx context.Context, patches []account.Patch) (*account.MutationResult, error) {
	return &account.MutationResult{}, nil
}
func (r *snapshotRepo) DeleteAccounts(ctx context.Context, tokens []string) (*account.MutationResult, error) {
	return &account.MutationResult{Deleted: len(tokens)}, nil
}
func (r *snapshotRepo) GetAccounts(ctx context.Context, tokens []string) ([]*account.Record, error) {
	return nil, nil
}
func (r *snapshotRepo) ListAccounts(ctx context.Context, query account.ListQuery) (*account.Page, error) {
	r.lastListQuery = query
	if r.listPage != nil {
		return r.listPage, nil
	}
	return &account.Page{}, nil
}
func (r *snapshotRepo) ReplacePool(ctx context.Context, pool string, upserts []account.Upsert) (*account.MutationResult, error) {
	return &account.MutationResult{Upserted: len(upserts)}, nil
}
func (r *snapshotRepo) ResetExpiredConsoleWindows(ctx context.Context) (int, error) {
	return 0, nil
}
func (r *snapshotRepo) RecoverConsoleExpiredAccounts(ctx context.Context) (int, error) {
	return 0, nil
}
func (r *snapshotRepo) Close(ctx context.Context) error { return nil }

func resetVideoJobsForTest(t *testing.T) {
	t.Helper()
	videoJobsMutex.Lock()
	old := videoJobsMap
	videoJobsMap = map[string]*videoJob{}
	videoJobsMutex.Unlock()
	t.Cleanup(func() {
		videoJobsMutex.Lock()
		videoJobsMap = old
		videoJobsMutex.Unlock()
	})
}
