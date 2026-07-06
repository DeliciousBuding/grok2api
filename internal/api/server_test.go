package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/account"
	"github.com/DeliciousBuding/grok2api/internal/model"
	"github.com/DeliciousBuding/grok2api/internal/platform"
)

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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(upstream.Close)

	_, err := fetchImageBase64(context.Background(), upstream.URL+"/missing.png")
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte("not reached"))
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetchImageBase64(ctx, upstream.URL+"/image.png")
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("12345"))
	}))
	t.Cleanup(upstream.Close)

	_, err := fetchImageBase64(context.Background(), upstream.URL+"/image.png")
	if err == nil {
		t.Fatal("expected oversized fetched image to fail")
	}
	if !strings.Contains(err.Error(), "image fetch exceeds") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestFetchImageBase64UsesConfiguredTimeout(t *testing.T) {
	loadTestConfig(t, "[asset]\nfetch_image_timeout_sec = 1\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	start := time.Now()
	_, err := fetchImageBase64(context.Background(), upstream.URL+"/slow.png")
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

func TestFetchImageBase64BoundsConcurrentDownloads(t *testing.T) {
	loadTestConfig(t, "[asset]\nmax_fetch_image_concurrency = 1\n")

	var current int32
	var maxSeen int32
	entered := make(chan struct{}, 2)
	releaseFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		_, _ = w.Write([]byte("ok"))
		atomic.AddInt32(&current, -1)
	}))
	t.Cleanup(upstream.Close)

	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := fetchImageBase64(context.Background(), upstream.URL+"/image.png")
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

func TestRenderGeneratedImagesReturnsUpstreamErrorForB64FetchFailure(t *testing.T) {
	loadTestConfig(t, "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(upstream.Close)

	_, err := renderGeneratedImages(context.Background(), "b64_json", []generatedImage{
		{url: upstream.URL + "/missing.png"},
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
